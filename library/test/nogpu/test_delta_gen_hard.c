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
 * test_delta_gen_campaign.c through the hard-limit start path -- the
 * watcher's bypass, taken by the default Fixed compute policy. A separate
 * binary rather than a second pass so each stays inside make test-nogpu's
 * per-test timeout.
 */
#define GEN_START_HARD 1
#include "test/nogpu/test_delta_gen_campaign.c"
