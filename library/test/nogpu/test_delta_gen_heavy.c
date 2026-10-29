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
 * test_delta_gen_campaign.c over the headroom range -- R/pool 1 to 1000 per
 * cycle, where v16's share runs past the pool and v15's could not -- through
 * the soft-limit start path. Under grid*block charging
 * block-heavy kernels on fast parts land here (kmix_model.h: B200
 * elementwise ~340 pools at FACTOR 256). A separate binary so each stays
 * inside make test-nogpu's per-test timeout.
 *
 * Budgets: v15 is capped at the pool here (util <= 100/(R/pool)), so most
 * cells improve by tens of points; the ratchet pins the ones that do not.
 */
#define GEN_HEAVY 1
#define GEN_START_HARD 0
#define HEAVY_SOFT_CELLS 0
#define HEAVY_SOFT_MAE   1.0
#define HEAVY_SOFT_P95   2
#define HEAVY_HARD_CELLS 0
#define HEAVY_HARD_MAE   1.0
#define HEAVY_HARD_P95   2
#include "test/nogpu/test_delta_gen_campaign.c"
