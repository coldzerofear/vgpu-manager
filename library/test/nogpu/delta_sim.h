/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
 * Shared by the generational campaigns (test_delta_gen_*.c) and the
 * transient scenarios (test_delta_transients.c). Not a test itself: the
 * Makefile only runs test_*.c.
 *
 *   - the card list (pool shapes)
 *   - the previous controller generation, frozen
 *   - plant_cycle(): one watcher cycle of the real loop, both start paths
 *
 * ONE TICK IS ONE CONTROLLER CYCLE, 100 ms. The watcher visits each device
 * once per 100 ms (wait = 100/dev_count ms over a batch of dev_count
 * devices); TIME_TICK's 10 ms is only rate_limiter's back-off sleep. A
 * kernel that fills the GPU for wave_us microseconds with tpb threads per
 * block consumes, at 100% utilization,
 *
 *     R / pool = (100000 / wave_us) * occupancy / (32 * tpb)
 *
 * per cycle under grid charging: a 10 ms GEMM at tpb 1024 is pool/3277.
 */
#ifndef VGPU_TEST_DELTA_SIM_H
#define VGPU_TEST_DELTA_SIM_H

#include <stdint.h>
#include <stdlib.h>

#include "include/hook.h"
#include "include/sm_delta.h"

typedef struct { const char *n; int sm, th; } card_t;
static const card_t CARDS[] = {
  /* Pascal / Volta */
  {"GTX1050",5,2048},{"GTX1060",10,2048},{"GTX1070",15,2048},{"GTX1080",20,2048},
  {"GTX1080Ti",28,2048},{"P40",30,2048},{"P100",56,2048},
  {"TitanV",80,2048},{"V100",80,2048},{"V100S",80,2048},{"V100-32g",80,2048},
  /* Turing (1024 threads/SM) */
  {"GTX1650",14,1024},{"GTX1660",22,1024},{"GTX1660Ti",24,1024},{"T4",40,1024},
  {"RTX2060",30,1024},{"RTX2070",36,1024},{"RTX2080",46,1024},{"RTX2080Ti",68,1024},
  /* Ampere consumer / pro (1536) */
  {"RTX3050",20,1536},{"RTX3060",28,1536},{"RTX3070",46,1536},{"RTX3070Ti",48,1536},
  {"RTX3080",68,1536},{"RTX3080Ti",80,1536},{"RTX3090",82,1536},
  {"A2",10,1536},{"A10",72,1536},{"A40",84,1536},
  {"RTXA4000",48,1536},{"RTXA5000",64,1536},{"RTXA6000",84,1536},
  /* Ampere datacenter (2048) incl. MIG */
  {"A100-1g",14,2048},{"A30",56,2048},{"A800",108,2048},{"A100",108,2048},
  /* Ada (1536) */
  {"RTX4060",24,1536},{"RTX4070",46,1536},{"RTX4070Ti",60,1536},{"RTX4080",76,1536},
  {"RTX4090",128,1536},{"L4",58,1536},{"L20",92,1536},{"L40",142,1536},
  {"L40S",142,1536},{"RTX6000Ada",142,1536},
  /* Hopper (2048) incl. MIG and PCIe */
  {"H100-1g",16,2048},{"H20",78,2048},{"H100-PCIe",114,2048},{"H800",132,2048},
  {"H100",132,2048},{"H200",132,2048},
  /* Blackwell datacenter (2048) / consumer and pro (1536) */
  {"B200",148,2048},{"B300",160,2048},
  {"RTX5060",36,1536},{"RTX5070",50,1536},{"RTX5070Ti",70,1536},{"RTX5080",84,1536},
  {"RTX5090",170,1536},{"RTXPRO6000",188,1536},
  /* Synthetic future parts: more SMs, and twice today's threads per SM. */
  {"FUT-256x2048",256,2048},{"FUT-384x2048",384,2048},
  {"FUT-512x4096",512,4096},{"FUT-1024x4096",1024,4096},
};
#define NCARD (int)(sizeof(CARDS) / sizeof(CARDS[0]))

/* ---- The previous generation (v14: seed pool*5/1310720, escape, 1-token
 * trim), frozen verbatim from include/sm_delta.h at commit ec867e6 with
 * every constant it reads. Only names changed. A baseline that moved with
 * the code under test would not be a baseline. ---- */
#define PREV_MIN_INCREMENT     5
#define PREV_INC_DIVISOR       1310720
#define PREV_GROW_CAP_DIVISOR  10
#define PREV_REL_DAMPING       6
#define PREV_STARVED_DAMPING   4
#define PREV_BOOT_JUMP_DIVISOR 6400
#define PREV_RAMP_DIVISOR      64

static uint64_t prev_isqrt(uint64_t n) {
  uint64_t r = 0, bit = 1ULL << 62;
  while (bit > n) {
    bit >>= 2;
  }
  while (bit) {
    if (n >= r + bit) {
      n -= r + bit;
      r = (r >> 1) + bit;
    } else {
      r >>= 1;
    }
    bit >>= 2;
  }
  return r;
}

static int64_t prev_delta_step(int64_t total, int up_limit, int user_current,
                               int64_t share, int64_t bucket, int boot) {
  int inc_divisor = PREV_INC_DIVISOR, ramp_divisor = PREV_RAMP_DIVISOR;
  if (up_limit >= 100) {
    return total;
  }
  int raw_diff = abs(up_limit - user_current);
  int64_t up = up_limit > 0 ? up_limit : 1;

  if (unlikely(inc_divisor <= 0)) {
    inc_divisor = PREV_INC_DIVISOR;
  }
  int64_t seed = total * PREV_MIN_INCREMENT / inc_divisor;
  if (unlikely(seed < 1)) {
    seed = 1;
  }

  if (user_current <= up_limit) {
    if (boot) {
      int64_t jump = total * up / PREV_BOOT_JUMP_DIVISOR;
      if (jump > total / 64) {
        jump = total / 64;
      }
      if (share < jump) {
        return jump;
      }
    }
    int damp = bucket <= 0 ? PREV_STARVED_DAMPING : PREV_REL_DAMPING;
    int64_t increment = share * (int64_t)raw_diff / (damp * up);
    if (increment < seed) {
      increment = seed;
    }
    /* Escape: a zero reading off a starved bucket means share is below 1%
     * of consumption. Climb quadratically out of the seed scale (what a
     * seed growing by one seed per tick would do) instead of crawling. */
    if (user_current == 0 && bucket <= 0) {
      __int128 q = (__int128)2 * seed * share;
      int64_t escape = (int64_t)prev_isqrt(q > INT64_MAX ? INT64_MAX : (uint64_t)q);
      if (increment < escape) {
        increment = escape;
      }
    }
    int64_t grow_cap = total / PREV_GROW_CAP_DIVISOR;
    if (increment > grow_cap) {
      increment = grow_cap;
    }
    return (share + increment) > total ? total : (share + increment);
  }

  if (raw_diff > up_limit && raw_diff >= PREV_MIN_INCREMENT) {
    /* Blowout: util beyond twice the target. */
    int64_t increment = share * (int64_t)raw_diff / (PREV_REL_DAMPING * up);
    if (increment < seed) {
      increment = seed;
    }
    if (ramp_divisor > 0) {
      int64_t ramp_floor = total * (int64_t)raw_diff / (up * ramp_divisor);
      if (increment < ramp_floor) {
        increment = ramp_floor;
      }
    }
    if (unlikely(increment > total)) {
      increment = total;
    }
    return (share - increment) < 0 ? 0 : (share - increment);
  }

  /* Mild overshoot: cut in proportion to how much of it share explains.
   * The bucket can read NEGATIVE here (rate_limiter deducts before it
   * checks), which would zero or flip the denominator -- clamp it: an
   * overdrawn bucket is "no backlog", the strongest share-is-the-cause
   * signal. 128-bit product because prop*share overflows int64 for pools
   * beyond ~2^31 tokens (no current GPU, but the contract is any pool). */
  int64_t backlog = bucket > 0 ? bucket : 0;
  int64_t prop = share * (int64_t)raw_diff / (PREV_REL_DAMPING * up);
  int64_t by_cause = (int64_t)((__int128)prop * share / (share + backlog + 1));
  int64_t by_error = prop * (int64_t)raw_diff / 100;
  int64_t increment = by_cause > by_error ? by_cause : by_error;
  /* Both terms truncate to 0 for a small share near the target, which would
   * park util a few points over it for good. Trim by the integer resolution
   * -- unless the backlog explains the overshoot, where holding share is
   * the design (trimming it there empties share while the bucket drains). */
  if (increment < 1 && backlog < share) {
    increment = 1;
  }
  return (share - increment) < 0 ? 0 : (share - increment);
}

/* ---- The plant: one watcher cycle ----
 *
 * Consumption: the workload drains up to R tokens per cycle from the bucket
 * (R = 0: idle). util = consumed/R, seen by the controller `delay` cycles
 * late. Then the controller runs and the bucket is refilled, as the watcher
 * does it:
 *
 *   soft start (Balance policy, or any non-exclusive device): always
 *     share = controller(...); bucket += share, clamped to the pool.
 *   hard start (Fixed policy, the default): while util < max(1, u/10) and
 *     nothing throttled since the last cycle, the bypass ASSIGNS the bucket
 *     to the controller's output and leaves share untouched; otherwise as
 *     soft. "Throttled" is a launch finding the bucket short -- here, the
 *     cycle's demand exceeding the bucket.
 *
 * The candidate is the shipped cycle: sm_delta_control with the refill mark
 * (mark - bucket = consumed) and the drain. The baseline is prev_delta_step
 * alone, as v14 shipped. */
typedef struct {
  int64_t bucket, share, mark;
  int mark_valid, calls, throttled;
  int hist[8];
} plant_t;

static int plant_cycle(plant_t *p, int64_t total, int64_t R, int u, int delay, int t,
                       int candidate, int hard, int inc_divisor) {
  int64_t before = p->bucket;
  int64_t c = R == 0 ? 0 : (p->bucket < R ? p->bucket : R);
  p->bucket -= c;
  if (R > 0 && before < R) {
    p->throttled = 1;
  }
  int util = R == 0 ? 0 : (int)(c * 100 / R);
  int m = p->hist[t % delay];
  p->hist[t % delay] = util;
  int boot = p->calls < 2;
  p->calls++;

  int64_t out;
  if (candidate) {
    int64_t consumed = p->mark_valid ? p->mark - p->bucket : -1;
    int drain = 0;
    out = sm_delta_control(total, u, m, p->share, p->bucket, boot, consumed, inc_divisor,
                           PREV_RAMP_DIVISOR, &drain);
    if (drain) {
      p->bucket = 0;
    }
  } else {
    out = prev_delta_step(total, u, m, p->share, p->bucket, boot);
  }
  int lo = u / 10 < 1 ? 1 : u / 10;
  if (hard && m < lo && !p->throttled) {
    p->bucket = out;
  } else {
    p->share = out;
    p->bucket += out;
  }
  if (p->bucket > total) {
    p->bucket = total;
  }
  if (p->bucket < 0) {
    p->bucket = 0;
  }
  /* Serial plant: "the read plus every applied change" is just the bucket. */
  p->mark = p->bucket;
  p->mark_valid = 1;
  p->throttled = 0;
  return util;
}

#endif /* VGPU_TEST_DELTA_SIM_H */
