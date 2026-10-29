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
 * test_delta_transients.c over the headroom range: load drops, rises and
 * on/off bursts between 0.3 and 300 pools per cycle. v15 cannot supply more
 * than one pool per cycle, so its numbers here are its ceiling, not control.
 */
#define TRN_HEAVY 1
/* Ratchet (re-based at v16.1). DROP: none left (v16 had 204). BURST: the
 * worst cells are one-cycle bursts (on1/off1) between 3 pools and idle: the
 * sample-driven estimate holds the window's mean, so a 100 ms burst runs
 * above the target inside it, where v15, pinned at one pool, read 7%. Mean
 * BURST MAE 40.05 -> 8.96. */
#define HEAVY_DROP_CELLS  0
#define HEAVY_DROP_WORST  2.0
#define HEAVY_RISE_CELLS  0
#define HEAVY_RISE_WORST  2.0
#define HEAVY_BURST_CELLS 2295
#define HEAVY_BURST_WORST 34.69
#include "test/nogpu/test_delta_transients.c"
