/*
Copyright 2026 coldzerofear

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
 * Checks include/array_size.h against what the driver actually reserved.
 *
 * Reads the CSV that tools/array_size_probe.c emits on a GPU host, feeds every
 * accepted descriptor through the SHIPPED sizing code, and classifies the
 * result. Deliberately a separate program from the probe: the probe must stay
 * independent of the thing it measures, or the comparison proves nothing.
 *
 * Needs no GPU and no CUDA toolkit -- the sizing is pure arithmetic, so this
 * runs anywhere the library builds and makes "verified against a real driver"
 * a command anyone can re-run rather than a claim in a commit message.
 *
 *   gcc -O2 -I library -I library/include \
 *       -o array_size_verify library/tools/array_size_verify.c
 *   ./array_size_verify < probe.csv
 *
 * Exit status is non-zero if any descriptor came out OVER the driver's figure
 * or could not be sized at all. Those are the two failures that matter:
 *
 *   OVER      -- we would charge more than the driver reserves, i.e. invent
 *                usage and OOM a workload that fits. Must be zero.
 *   UNSIZABLE -- the driver accepted a descriptor our arithmetic refuses, so
 *                the allocation would skip the budget check. Must be zero.
 *   under     -- expected and safe. The driver rounds every array up to a
 *                64 KiB granule and pads 2D rows/heights for its own tiling;
 *                we model neither (see array_size.h), so our figure is a lower
 *                bound. Reported with the ratio so a regression that turns a
 *                1% gap into a 10x one is visible.
 */

#include <stdio.h>
#include <string.h>

#include "include/array_size.h"

int main(void) {
  char line[1024];
  long accepted = 0, exact_match = 0, under = 0, over = 0, unsizable = 0;
  double worst_ratio = 1.0;
  char worst[256] = "";

  /* The probe's header row, and any header repeated by concatenating several
   * runs into one file. */
  while (fgets(line, sizeof(line), stdin) != NULL) {
    char name[96], result[64], shape[48];
    unsigned int format = 0, channels = 0;
    size_t width = 0, height = 0, depth = 0;
    /* alignment and delta are parsed but unused: only driver_size matters, and
     * requiring the trailing columns is what makes a truncated row get skipped
     * instead of half-read. */
    size_t driver_size = 0, alignment = 0, delta = 0;
    CUDA_ARRAY3D_DESCRIPTOR desc;
    size_t ours = 0;

    /* A row the probe emitted with extra trailing columns still parses: sscanf
     * stops once the format is exhausted. */
    if (sscanf(line, "%95[^,],0x%x,%u,%47[^,],%zu,%zu,%zu,%63[^,],%zu,%zu,%zu",
               name, &format, &channels, shape, &width, &height, &depth, result,
               &driver_size, &alignment, &delta) != 11) {
      continue; /* header or malformed line */
    }
    if (strcmp(result, "CUDA_SUCCESS") != 0) {
      continue; /* the driver refused it; nothing to compare */
    }
    accepted++;

    memset(&desc, 0, sizeof(desc));
    desc.Width = width;
    desc.Height = height;
    desc.Depth = depth;
    desc.Format = (CUarray_format)format;
    desc.NumChannels = channels;

    if (!vgpu_array3d_size(&desc, &ours)) {
      unsizable++;
      printf("UNSIZABLE %-24s ch=%u %-10s %zux%zux%zu\n", name, channels, shape,
             width, height, depth);
      continue;
    }
    if (ours == driver_size) {
      exact_match++;
    } else if (ours > driver_size) {
      over++;
      printf("OVER      %-24s ch=%u %-10s %zux%zux%zu ours=%zu driver=%zu\n", name,
             channels, shape, width, height, depth, ours, driver_size);
    } else {
      double ratio = ours ? (double)driver_size / (double)ours : 0.0;
      under++;
      if (ratio > worst_ratio) {
        worst_ratio = ratio;
        snprintf(worst, sizeof(worst), "%s ch=%u %s %zux%zux%zu (%zu vs %zu)",
                 name, channels, shape, width, height, depth, ours, driver_size);
      }
    }
  }

  printf("\naccepted descriptors : %ld\n", accepted);
  printf("  exact match        : %ld\n", exact_match);
  printf("  under (safe)       : %ld", under);
  if (under > 0) {
    printf("   worst ratio %.3f at %s", worst_ratio, worst);
  }
  printf("\n  OVER  (must be 0)  : %ld\n", over);
  printf("  UNSIZABLE (must be 0): %ld\n", unsizable);

  if (accepted == 0) {
    printf("\nno accepted descriptor in the input -- nothing was verified\n");
    return 1;
  }
  return (over == 0 && unsizable == 0) ? 0 : 1;
}
