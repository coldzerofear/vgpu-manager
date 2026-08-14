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
 * The delta controller's share-update step, as a pure function.
 *
 * Header-inline so the watcher's hot path keeps its inlining AND the math is
 * reachable from the no-GPU tests: every rule below was chosen by running the
 * closed-loop simulation in test/nogpu/test_delta_step.c, not by argument.
 * The step is:
 *
 *     step = max( total * MIN_INCREMENT / inc_divisor,                 (seed)
 *                 share * |target - util| / (DAMPING * target) )      (scaled)
 *
 *     grow (util <= target): share += min(step, total/10)
 *     cut  (util >  target): share -= step,
 *                            step floored at total*diff/(target*ramp_divisor)
 *                            when util > 2*target (emergency only)
 *
 * WHY THIS SHAPE. Its predecessor stepped by sm_num^2*thread*diff/2560
 * against a pool linear in sm_num, so the step FRACTION of the pool grew with
 * SM count: HAMi-core #274's 188-SM card got 11-22% of its pool per tick,
 * railed the bucket full within a second, and the limit never engaged (the
 * share only refills -- change_token adds and share clamps at 0 -- so a full
 * bucket drains only by consumption, which for light-grid workloads takes
 * minutes). The closed loop then showed that merely making the step linear
 * in the pool is NOT enough: any absolute step larger than what the workload
 * consumes per tick floods the bucket the same way, and what "large" means
 * depends on the workload, not the card. The only quantity that self-scales
 * to the workload is the share itself -- at equilibrium it EQUALS per-tick
 * consumption at the target -- hence the multiplicative term: corrections are
 * a fraction of share, proportional to the relative error.
 *
 * THE SEED is the escape from share=0 and the granularity floor near the
 * setpoint. inc_divisor sets it: total*MIN_INCREMENT/81920 = ~0.006% of the
 * pool, far below even a pool-drained-over-minutes consumption rate, so the
 * controller can always express a refill small enough not to accumulate.
 * (The closed loop pinned at 100% with this at 0.03% -- granularity is the
 * whole ballgame for light workloads, and response speed costs nothing here
 * because the multiplicative term owns it.)
 *
 * THE DAMPING (6) is set by feedback delay. The controller acts on a util
 * sample 2-4 ticks stale (NVML window + share-take-effect); share/2-scale
 * steps compound 1.5^4 before their effect is measured, and the simulation
 * showed exactly that: MAE 1-2 at 2-tick delay exploding to ~39 at 4-tick.
 * At /6 the compounding stays benign through 6-tick delay (MAE 2-3) while
 * cold-start ramp is still ~40 ticks, inside the ~64-tick budget the old
 * ramp floor was designed around.
 *
 * THE EMERGENCY FLOOR is what remains of the old symmetric ramp floor. Its
 * grow half is gone -- growing by an absolute fraction of the pool is the
 * flooding mechanism, and the multiplicative term ramps fast enough without
 * it. Its cut half survives, gated to real blowouts (util > 2*target): there
 * share may be tiny while the excess is huge, the share-proportional cut
 * stalls, and only an absolute cut recovers quickly. The historical ratchet
 * this floor was built against (grow floored but not cut -> hard_core=8
 * pinned at 15) cannot recur: there is no grow floor at all, and both sides
 * scale identically otherwise.
 *
 * THE GROW CAP (total/10, from HAMi's fix) stays as the invariant backstop:
 * no single tick may commit more than a tenth of the pool.
 *
 * Simulated MAE against the old formula (5 cards from 7-SM MIG to 188-SM,
 * targets 8-80, workloads pool/1500..pool/15 per tick, 2-tick delay):
 * old 0.6-91 depending on card and regime (pinned near 100% in most light
 * and medium cells); this 0.5-15 in every cell, identical across cards.
 * Ceiling on real hardware will differ; run test/ablation to validate.
 */

#ifndef _VGPU_SM_DELTA_H_
#define _VGPU_SM_DELTA_H_

#include "include/hook.h"

#ifdef __cplusplus
extern "C" {
#endif

#define MIN_INCREMENT                    5
#define DELTA_INCREMENT_DIVISOR_DEFAULT  81920
#define DELTA_GROW_CAP_DIVISOR           10
#define DELTA_REL_DAMPING                6

static inline int64_t sm_delta_step(int64_t total, int up_limit, int user_current,
                                    int64_t share, int inc_divisor, int ramp_divisor) {
  int raw_diff = abs(up_limit - user_current);
  int64_t up = up_limit > 0 ? up_limit : 1;

  if (unlikely(inc_divisor <= 0)) {
    inc_divisor = DELTA_INCREMENT_DIVISOR_DEFAULT;
  }
  int64_t seed = total * MIN_INCREMENT / inc_divisor;
  int64_t increment = share * (int64_t)raw_diff / (DELTA_REL_DAMPING * up);
  if (increment < seed) {
    increment = seed;
  }

  if (user_current <= up_limit) {
    int64_t grow_cap = total / DELTA_GROW_CAP_DIVISOR;
    if (increment > grow_cap) {
      increment = grow_cap;
    }
    share = (share + increment) > total ? total : (share + increment);
  } else {
    if (ramp_divisor > 0 && raw_diff > up_limit) {
      int64_t ramp_floor = total * (int64_t)raw_diff / (up * ramp_divisor);
      if (increment < ramp_floor) {
        increment = ramp_floor;
      }
    }
    if (unlikely(increment > total)) {
      increment = total;
    }
    share = (share - increment) < 0 ? 0 : (share - increment);
  }
  return share;
}

#ifdef __cplusplus
}
#endif

#endif /* _VGPU_SM_DELTA_H_ */
