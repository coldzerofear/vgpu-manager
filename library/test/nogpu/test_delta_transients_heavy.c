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
/* Ratchet. BURST: on/off between 3 pools and idle at a period of twice the
 * measurement delay (on2/off2 at delay 2, ...) aliases: the readings arrive
 * exactly out of phase and the share settles ~4x too deep (util 100 on, 0
 * off, MAE 50 at target 50) where v15, pinned at one pool, read 33%. */
#define HEAVY_DROP_CELLS  204
#define HEAVY_DROP_WORST  2.23
#define HEAVY_RISE_CELLS  0
#define HEAVY_RISE_WORST  2.0
#define HEAVY_BURST_CELLS 3366
#define HEAVY_BURST_WORST 33.01
#include "test/nogpu/test_delta_transients.c"
