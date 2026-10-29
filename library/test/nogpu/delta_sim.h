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
 *   - the previous controller generation (v15), frozen
 *   - plant_cycle(): one watcher cycle of the real loop, both start paths
 *
 * ONE TICK IS ONE CONTROLLER CYCLE, 100 ms. The watcher visits each device
 * once per 100 ms (wait = 100/dev_count ms over a batch of dev_count
 * devices); TIME_TICK's 10 ms is only rate_limiter's back-off sleep. A
 * kernel that fills the GPU for wave_us microseconds consumes, at 100%
 * utilization and per cycle,
 *
 *     R / pool = (100000 / wave_us) * occupancy / FACTOR        (v16: threads)
 *     R / pool = (100000 / wave_us) * occupancy / (32 * tpb)    (v15: grids)
 *
 * so a 10 ms full-GPU kernel sits at pool/25.6 under v16 whatever its block
 * size, and block-heavy short kernels land above the pool (the headroom).
 * The campaigns sweep R/pool directly; the real-workload mapping per charge
 * is test_delta_realload.c's job (kmix_model.h).
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

/* ---- v15 (consumption-observed cut, grid charging, FACTOR 32), frozen
 * verbatim from include/sm_delta.h at the commit that introduced v16, with
 * every constant it reads. Only names changed. ---- */
#define V15_MIN_INCREMENT      5
#define V15_INC_DIVISOR        1310720
#define V15_GROW_CAP_DIVISOR   10
#define V15_REL_DAMPING        6
#define V15_STARVED_DAMPING    4
#define V15_BOOT_JUMP_DIVISOR  6400
#define V15_OBS_MIN_CONSUMED   100
#define V15_OBS_MIN_OVERSHOOT  (2 * V15_MIN_INCREMENT)
#define V15_OBS_BACKLOG_TICKS  8
#define V15_FACTOR             32

static uint64_t v15_isqrt(uint64_t n) {
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

static int64_t v15_step(int64_t total, int up_limit, int user_current,
                                    int64_t share, int64_t bucket, int boot,
                                    int inc_divisor, int ramp_divisor) {
  if (up_limit >= 100) {
    return total;
  }
  int raw_diff = abs(up_limit - user_current);
  int64_t up = up_limit > 0 ? up_limit : 1;

  if (unlikely(inc_divisor <= 0)) {
    inc_divisor = V15_INC_DIVISOR;
  }
  int64_t seed = total * V15_MIN_INCREMENT / inc_divisor;
  if (unlikely(seed < 1)) {
    seed = 1;
  }

  if (user_current <= up_limit) {
    if (boot) {
      int64_t jump = total * up / V15_BOOT_JUMP_DIVISOR;
      if (jump > total / 64) {
        jump = total / 64;
      }
      if (share < jump) {
        return jump;
      }
    }
    int damp = bucket <= 0 ? V15_STARVED_DAMPING : V15_REL_DAMPING;
    int64_t increment = share * (int64_t)raw_diff / (damp * up);
    if (increment < seed) {
      increment = seed;
    }
    /* Escape: a zero reading off a starved bucket means share is below 1%
     * of consumption. Climb quadratically out of the seed scale (what a
     * seed growing by one seed per tick would do) instead of crawling. */
    if (user_current == 0 && bucket <= 0) {
      __int128 q = (__int128)2 * seed * share;
      int64_t escape = (int64_t)v15_isqrt(q > INT64_MAX ? INT64_MAX : (uint64_t)q);
      if (increment < escape) {
        increment = escape;
      }
    }
    int64_t grow_cap = total / V15_GROW_CAP_DIVISOR;
    if (increment > grow_cap) {
      increment = grow_cap;
    }
    return (share + increment) > total ? total : (share + increment);
  }

  if (raw_diff > up_limit && raw_diff >= V15_MIN_INCREMENT) {
    /* Blowout: util beyond twice the target. */
    int64_t increment = share * (int64_t)raw_diff / (V15_REL_DAMPING * up);
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
  int64_t prop = share * (int64_t)raw_diff / (V15_REL_DAMPING * up);
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

static int64_t v15_observe(int up_limit, int user_current, int64_t share,
                                       int64_t next, int64_t bucket, int64_t consumed,
                                       int *drain) {
  *drain = 0;
  if (up_limit >= 100 || up_limit < 1 || consumed < V15_OBS_MIN_CONSUMED ||
      user_current - up_limit < V15_OBS_MIN_OVERSHOOT || consumed >= share ||
      bucket <= V15_OBS_BACKLOG_TICKS * share) {
    return next;
  }
  /* user_current > up_limit >= 1 here, so the division is safe; consumed is
   * bounded by the pool, so the product cannot overflow. */
  int64_t cap = consumed * up_limit / user_current;
  *drain = 1;
  return next < cap ? next : cap;
}

static int64_t v15_control(int64_t total, int up_limit, int user_current,
                                       int64_t share, int64_t bucket, int boot,
                                       int64_t consumed, int inc_divisor, int ramp_divisor,
                                       int *drain) {
  int64_t next = v15_step(total, up_limit, user_current, share, bucket, boot,
                               inc_divisor, ramp_divisor);
  return v15_observe(up_limit, user_current, share, next, bucket, consumed, drain);
}

/* ---- The plant: one watcher cycle ----
 *
 * Consumption: the workload drains up to R tokens per cycle from the bucket
 * (R = 0: idle). util = consumed/R, seen by the controller `delay` cycles
 * late. Then the controller runs and the bucket is refilled, as the watcher
 * does it:
 *
 *   soft start (Balance policy, or any non-exclusive device): always
 *     share = controller(...); bucket += share.
 *   hard start (Fixed policy, the default): while util < max(1, u/10) and
 *     nothing throttled since the last cycle, the bypass ASSIGNS the bucket
 *     to the controller's output and leaves share untouched; otherwise as
 *     soft. "Throttled" is a launch finding the bucket short -- here, the
 *     cycle's demand exceeding the bucket.
 *
 * Both generations measure consumption through the refill mark (mark -
 * bucket) and drain when the observed cut asks. The baseline is v15 as it
 * shipped (share and bucket capped at the pool); the candidate is the
 * shipped cycle -- sm_delta_est_control, which falls back to
 * sm_delta_control without a usable sample, and the bucket held between
 * sm_delta_debt_floor() and sm_delta_est_bucket_ceiling() of the share the
 * controller state holds, as refill_ceiling() in cuda_hook.c does it. R is
 * in tokens of whichever charge the caller models; the plant does not care. */
typedef struct {
  int64_t bucket, share, mark;
  int mark_valid, calls, throttled;
  int hist[8];
  sm_delta_est_t est;          /* the candidate's sample-driven estimate state */
  int hold_acc, hold_n, held;  /* g_plant_hold: the sample being accumulated / the one shown */
  uint64_t held_ts;
} plant_t;

/* Measurement model. 0 (the campaigns' original model): a fresh reading every
 * cycle, `delay` cycles late. H > 0: what NVML does on parts with a slow
 * sample period (the RTX 2080 Ti measured 1 s = 10 cycles): a new sample
 * every H cycles, the mean of the H cycles before it, shown until the next
 * one. Either way each reading carries the timestamp of the period it
 * describes, which the candidate pairs with its consumption records. */
static int g_plant_hold = 0;

static int plant_cycle(plant_t *p, int64_t total, int64_t R, int u, int delay, int t,
                       int candidate, int hard, int inc_divisor) {
  int64_t before = p->bucket;
  int64_t c = R == 0 ? 0 : (p->bucket < R ? p->bucket : R);
  if (c < 0) {
    c = 0;
  }
  p->bucket -= c;
  if (R > 0 && before < R) {
    p->throttled = 1;
  }
  int util = R == 0 ? 0 : (int)(c * 100 / R);
  int m;
  uint64_t sample_ts;
  if (g_plant_hold > 0) {
    p->hold_acc += util;
    if (++p->hold_n == g_plant_hold) {
      p->held = p->hold_acc / g_plant_hold;
      p->held_ts = (uint64_t)(t + 1) * 100;
      p->hold_acc = 0;
      p->hold_n = 0;
    }
    m = p->held;
    sample_ts = p->held_ts;
  } else {
    m = p->hist[t % delay];
    p->hist[t % delay] = util;
    /* cycle k covers (k*100, (k+1)*100]; the reading shown is cycle t - delay */
    sample_ts = t >= delay ? (uint64_t)(t + 1 - delay) * 100 : 0;
  }
  int boot = p->calls < 2;
  p->calls++;

  int64_t consumed = p->mark_valid ? p->mark - p->bucket : -1;
  int drain = 0;
  int64_t out = candidate
      ? sm_delta_est_control(&p->est, total, u, m, sample_ts, (uint32_t)(t + 1) * 100, p->share,
                             p->bucket, boot, consumed, inc_divisor, 64, &drain)
      : v15_control(total, u, m, p->share, p->bucket, boot, consumed, V15_INC_DIVISOR, 64, &drain);
  if (drain && p->bucket > 0) {
    p->bucket = 0;
  }
  int lo = u / 10 < 1 ? 1 : u / 10;
  if (hard && m < lo && !p->throttled) {
    p->bucket = out;
  } else {
    p->share = out;
    p->bucket += out;
  }
  int64_t ceiling = candidate ? sm_delta_est_bucket_ceiling(p->share) : total;
  int64_t floor = candidate ? sm_delta_debt_floor(p->share, ceiling) : 0;
  if (p->bucket > ceiling) {
    p->bucket = ceiling;
  }
  if (p->bucket < floor) {
    p->bucket = floor;
  }
  /* Serial plant: "the read plus every applied change" is just the bucket. */
  p->mark = p->bucket;
  p->mark_valid = 1;
  p->throttled = 0;
  return util;
}

#endif /* VGPU_TEST_DELTA_SIM_H */
