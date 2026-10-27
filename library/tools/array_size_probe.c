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
 * Asks the driver how big a CUDA array of each CUarray_format really is.
 *
 * WHY THIS EXISTS
 *
 * include/array_size.h charges the budget for cuArrayCreate / cuArray3DCreate /
 * cuMipmappedArrayCreate. It is exact for the eight classic formats, because
 * NVIDIA's cuda.h pins those with two worked descriptor examples. For every
 * format added after CUDA 3.x -- BC1..BC7, the UNORM/SNORM INTnXm family, and
 * the planar/semi-planar/packed YUV range -- NVIDIA documents a one-line
 * description and NOTHING about sizing: not bytes per element, not whether
 * Width/Height count texels or 4x4 blocks, not which NumChannels values are
 * even legal. Those formats therefore fall back to a conservative upper bound
 * (VGPU_ARRAY_UNKNOWN_FORMAT_BYTES), which never under-charges but over-charges
 * the compressed ones.
 *
 * Narrowing the fallback needs real numbers, and the only authority that has
 * them is the driver. Guessing from format names is exactly what this tool
 * exists to avoid: the open question that blocks a table is whether
 * UNORM_INT8X4 ("4 channel unsigned 8-bit") wants NumChannels 1 or 4, and those
 * two readings differ by 4x in a component that enforces a memory limit.
 *
 * WHAT IT DOES
 *
 * Phase 1, acceptance: for every format, every NumChannels in {1,2,4} and each
 * dimensionality, call cuArray3DCreate and record the CUresult. This alone
 * answers the NumChannels question -- whatever the driver refuses is not a
 * descriptor we have to size.
 *
 * Phase 2, size: for each accepted combination, obtain the byte size two
 * independent ways and print both, so neither has to be trusted alone.
 *   a) cuArrayGetMemoryRequirements, which returns the exact size the driver
 *      reserves. It needs the array created with CUDA_ARRAY3D_DEFERRED_MAPPING
 *      (cuda.h: "If the CUDA array is not allocated with flag
 *      CUDA_ARRAY3D_DEFERRED_MAPPING CUDA_ERROR_INVALID_VALUE will be
 *      returned"), which not every format or driver supports -- hence (b).
 *   b) the cuMemGetInfo free-memory delta across the allocation, which works
 *      for anything that can be created but is quantised by the driver's own
 *      suballocator, so it reads as an upper bound rather than an exact figure.
 *
 * Each accepted combination is measured at one width and then at double the
 * width. The ratio separates the questions that matter: a cost that doubles is
 * linear in Width, one that does not reveals row padding or block rounding, and
 * comparing 2D against the same descriptor with Height=0 confirms on real
 * hardware what cuda.h only states in prose -- that a zero extent means the
 * dimension is absent rather than a factor of zero.
 *
 * HOW TO BUILD AND RUN
 *
 * Through the build tree, on a host that has libcuda:
 *
 *   CUDA_HOME=/usr/local/cuda make -C library build-tests   # or:
 *   cmake --build <builddir> --target array_size_probe
 *
 * Or standalone, which is usually easier on a GPU box that has the driver but
 * no checkout of this tree -- only this one .c file is needed:
 *
 *   gcc -O2 -o array_size_probe array_size_probe.c \
 *       -I/usr/local/cuda/include -l:libcuda.so.1 -ldl
 *
 * Then:
 *
 *   ./array_size_probe [device_index] [--csv]
 *
 * Run it on a bare GPU, NOT inside a vGPU container: with
 * libvgpu-control.so preloaded, cuMemGetInfo reports the container's quota and
 * the deltas measure our own accounting instead of the driver's. The tool
 * checks /proc/self/maps and says so if it finds the library loaded. Running
 * it a second time INSIDE a vGPU container is still useful -- as an end-to-end
 * check that what we charge matches what the driver reserves -- but the
 * ground-truth table has to come from the clean run.
 *
 * --csv prints one machine-readable row per probe for pasting back verbatim;
 * without it the output is an aligned table meant for reading.
 *
 * Needs a CUDA 11.6+ driver for cuArrayGetMemoryRequirements; older drivers
 * just leave that column empty and fall back to the delta. The symbol is
 * resolved with dlsym rather than linked so the tool still loads on them.
 */

#include <cuda.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* Resolved at runtime; see the header comment. */
typedef CUresult (*array_mem_reqs_fn)(CUDA_ARRAY_MEMORY_REQUIREMENTS *, CUarray, CUdevice);
static array_mem_reqs_fn array_mem_reqs;

static int csv_mode;

/* --geom: one exact geometry instead of the shape sweep, for pinning a single
 * rule (see --help). width 0 means "not set". */
static struct {
  int set;
  size_t width, height, depth;
} custom;

/* Base probe geometry. Big enough that the free-memory delta clears the
 * driver's allocation granularity, small enough that the whole sweep fits on
 * a small card: the widest case is the doubled 3D width, 4096 x 256 x 4 at
 * 8 bytes x 4 channels = 128 MiB, and only one array is live at a time. */
#define PROBE_WIDTH   2048
#define PROBE_HEIGHT  1024 /* 2D only */
#define PROBE_HEIGHT3 256  /* 3D and layered, to bound the footprint */
#define PROBE_DEPTH   4

struct format_entry {
  const char *name;
  int value;
  /* 1 when include/array_size.h sizes this format exactly today, so the run
   * doubles as a check of the values we already ship. */
  int exact_today;
};

/* Every CUarray_format cuda.h 13.4 defines, as literal values rather than the
 * CU_AD_FORMAT_* enumerators.
 *
 * Not a style choice: the first version of this table guarded each family with
 * #ifdef CU_AD_FORMAT_NV12 and friends so it would still compile against an
 * older cuda.h. Those names are ENUMERATORS, not macros, so every #ifdef was
 * false and the entire extended set compiled away -- the sweep probed only the
 * eight classic formats, which are the ones we already size exactly, making the
 * tool useless for the question it exists to answer. Caught by running it
 * against a stub driver and counting the rows. Literal values have no such
 * failure mode and let the tool probe a format the build host's header does not
 * know yet; the names are kept alongside purely for the report.
 *
 * Every pair below was extracted mechanically from the CUarray_format typedef
 * in NVIDIA's cuda.h, not transcribed by hand. CU_AD_FORMAT_MAX is excluded:
 * it is a sentinel, not a format. */
static const struct format_entry formats[] = {
    {"UNSIGNED_INT8", 0x01, 1},
    {"UNSIGNED_INT16", 0x02, 1},
    {"UNSIGNED_INT32", 0x03, 1},
    {"SIGNED_INT8", 0x08, 1},
    {"SIGNED_INT16", 0x09, 1},
    {"SIGNED_INT32", 0x0a, 1},
    {"HALF", 0x10, 1},
    {"FLOAT", 0x20, 1},
    {"NV12", 0xb0, 0},
    {"UNORM_INT8X1", 0xc0, 0},
    {"UNORM_INT8X2", 0xc1, 0},
    {"UNORM_INT8X4", 0xc2, 0},
    {"UNORM_INT16X1", 0xc3, 0},
    {"UNORM_INT16X2", 0xc4, 0},
    {"UNORM_INT16X4", 0xc5, 0},
    {"SNORM_INT8X1", 0xc6, 0},
    {"SNORM_INT8X2", 0xc7, 0},
    {"SNORM_INT8X4", 0xc8, 0},
    {"SNORM_INT16X1", 0xc9, 0},
    {"SNORM_INT16X2", 0xca, 0},
    {"SNORM_INT16X4", 0xcb, 0},
    {"BC1_UNORM", 0x91, 0},
    {"BC1_UNORM_SRGB", 0x92, 0},
    {"BC2_UNORM", 0x93, 0},
    {"BC2_UNORM_SRGB", 0x94, 0},
    {"BC3_UNORM", 0x95, 0},
    {"BC3_UNORM_SRGB", 0x96, 0},
    {"BC4_UNORM", 0x97, 0},
    {"BC4_SNORM", 0x98, 0},
    {"BC5_UNORM", 0x99, 0},
    {"BC5_SNORM", 0x9a, 0},
    {"BC6H_UF16", 0x9b, 0},
    {"BC6H_SF16", 0x9c, 0},
    {"BC7_UNORM", 0x9d, 0},
    {"BC7_UNORM_SRGB", 0x9e, 0},
    {"P010", 0x9f, 0},
    {"P016", 0xa1, 0},
    {"NV16", 0xa2, 0},
    {"P210", 0xa3, 0},
    {"P216", 0xa4, 0},
    {"YUY2", 0xa5, 0},
    {"Y210", 0xa6, 0},
    {"Y216", 0xa7, 0},
    {"AYUV", 0xa8, 0},
    {"Y410", 0xa9, 0},
    {"Y416", 0xb1, 0},
    {"Y444_PLANAR8", 0xb2, 0},
    {"Y444_PLANAR10", 0xb3, 0},
    {"YUV444_8bit_SemiPlanar", 0xb4, 0},
    {"YUV444_16bit_SemiPlanar", 0xb5, 0},
    {"UNORM_INT_101010_2", 0x50, 0},
    {"UINT8_PACKED_422", 0x51, 0},
    {"UINT8_PACKED_444", 0x52, 0},
    {"UINT8_SEMIPLANAR_420", 0x53, 0},
    {"UINT16_SEMIPLANAR_420", 0x54, 0},
    {"UINT8_SEMIPLANAR_422", 0x55, 0},
    {"UINT16_SEMIPLANAR_422", 0x56, 0},
    {"UINT8_SEMIPLANAR_444", 0x57, 0},
    {"UINT16_SEMIPLANAR_444", 0x58, 0},
    {"UINT8_PLANAR_420", 0x59, 0},
    {"UINT16_PLANAR_420", 0x5a, 0},
    {"UINT8_PLANAR_422", 0x5b, 0},
    {"UINT16_PLANAR_422", 0x5c, 0},
    {"UINT8_PLANAR_444", 0x5d, 0},
    {"UINT16_PLANAR_444", 0x5e, 0},
};

/* The four descriptor shapes that matter, named the way cuda.h names them:
 * "A 1D array is allocated if Height and Depth extents are both zero. A 2D
 * array is allocated if only Depth extent is zero." */
struct shape {
  const char *name;
  size_t height; /* 0 leaves the extent at zero, i.e. the dimension is absent */
  size_t depth;
};

static const struct shape shapes[] = {
    {"1D", 0, 0},
    {"2D", PROBE_HEIGHT, 0},
    {"3D", PROBE_HEIGHT3, PROBE_DEPTH},
    /* Same extents as 3D; the LAYERED flag is added in probe() by name, so the
     * pair isolates what the flag changes from what the extents change. */
    {"2Dlayered", PROBE_HEIGHT3, PROBE_DEPTH},
};

static const char *result_name(CUresult res) {
  const char *s = NULL;
  if (cuGetErrorName(res, &s) == CUDA_SUCCESS && s != NULL) {
    return s;
  }
  return "CUDA_ERROR_?";
}

/* One measurement. Returns 1 if the descriptor was accepted at all. */
static int probe(CUdevice dev, const struct format_entry *fmt, unsigned channels,
                 const struct shape *shape, size_t width) {
  CUDA_ARRAY3D_DESCRIPTOR desc;
  CUarray arr = NULL;
  CUresult create_res, reqs_res = CUDA_ERROR_NOT_SUPPORTED;
  size_t exact = 0, align = 0;
  size_t free_before = 0, free_after = 0, total = 0, delta = 0;
  unsigned flags = (strcmp(shape->name, "2Dlayered") == 0) ? CUDA_ARRAY3D_LAYERED : 0;

  memset(&desc, 0, sizeof(desc));
  desc.Width = width;
  desc.Height = shape->height;
  desc.Depth = shape->depth;
  desc.Format = (CUarray_format)fmt->value;
  desc.NumChannels = channels;
  desc.Flags = flags;

  /* (a) exact, via DEFERRED_MAPPING + cuArrayGetMemoryRequirements. */
  if (array_mem_reqs != NULL) {
    CUDA_ARRAY3D_DESCRIPTOR deferred = desc;
    CUarray darr = NULL;
    deferred.Flags = flags | CUDA_ARRAY3D_DEFERRED_MAPPING;
    if (cuArray3DCreate(&darr, &deferred) == CUDA_SUCCESS) {
      CUDA_ARRAY_MEMORY_REQUIREMENTS reqs;
      memset(&reqs, 0, sizeof(reqs));
      reqs_res = array_mem_reqs(&reqs, darr, dev);
      if (reqs_res == CUDA_SUCCESS) {
        exact = reqs.size;
        align = reqs.alignment;
      }
      cuArrayDestroy(darr);
    }
  }

  /* (b) upper bound, via the free-memory delta of a real allocation. Taken
   * around the create/destroy pair so an unrelated driver-side allocation
   * shows up as an obviously wrong delta rather than silently biasing it. */
  cuMemGetInfo(&free_before, &total);
  create_res = cuArray3DCreate(&arr, &desc);
  if (create_res == CUDA_SUCCESS) {
    cuMemGetInfo(&free_after, &total);
    delta = free_before > free_after ? free_before - free_after : 0;
    cuArrayDestroy(arr);
  }

  if (csv_mode) {
    printf("%s,0x%x,%u,%s,%zu,%zu,%zu,%s,%zu,%zu,%zu,%d\n", fmt->name, fmt->value,
           channels, shape->name, desc.Width, desc.Height, desc.Depth,
           result_name(create_res), exact, align, delta, fmt->exact_today);
    return create_res == CUDA_SUCCESS;
  }

  if (create_res != CUDA_SUCCESS) {
    printf("  %-24s ch=%u %-10s w=%-5zu REJECTED %s\n", fmt->name, channels,
           shape->name, desc.Width, result_name(create_res));
    return 0;
  }

  printf("  %-24s ch=%u %-10s w=%-5zu", fmt->name, channels, shape->name, desc.Width);
  if (exact) {
    size_t texels = desc.Width * (desc.Height ? desc.Height : 1) *
                    (desc.Depth ? desc.Depth : 1);
    printf(" exact=%-12zu align=%-7zu B/texel=%.4f", exact, align,
           (double)exact / (double)texels);
  } else {
    printf(" exact=%-12s align=%-7s B/texel=%-9s", "n/a", "n/a", "n/a");
  }
  printf(" free_delta=%zu\n", delta);
  return 1;
}

int main(int argc, char **argv) {
  int device_index = 0;
  CUdevice dev;
  CUcontext ctx;
  char name[256];
  int driver_version = 0;
  size_t free_mem = 0, total_mem = 0;
  void *libcuda;

  for (int i = 1; i < argc; i++) {
    if (strcmp(argv[i], "--csv") == 0) {
      csv_mode = 1;
    } else if (strcmp(argv[i], "--geom") == 0 && i + 1 < argc) {
      if (sscanf(argv[++i], "%zu,%zu,%zu", &custom.width, &custom.height,
                 &custom.depth) < 1) {
        fprintf(stderr, "--geom wants W[,H[,D]]\n");
        return 1;
      }
      custom.set = 1;
    } else if (strcmp(argv[i], "--help") == 0) {
      printf("usage: %s [device_index] [--csv] [--geom W[,H[,D]]]\n", argv[0]);
      printf("  --geom replaces the shape sweep with one exact geometry and\n");
      printf("         does not double the width, for pinning a specific rule\n");
      printf("         (block rounding at a width not divisible by 4, row\n");
      printf("         padding at an awkward width, the allocation floor).\n");
      return 0;
    } else {
      device_index = atoi(argv[i]);
    }
  }

  /* A run with our own library preloaded measures our accounting, not the
   * driver's. Say so loudly rather than letting a polluted table be collected
   * and trusted. */
  {
    FILE *maps = fopen("/proc/self/maps", "r");
    int found = 0;
    char line[512];
    if (maps != NULL) {
      while (fgets(line, sizeof(line), maps) != NULL) {
        if (strstr(line, "libvgpu-control.so") != NULL) {
          found = 1;
          break;
        }
      }
      fclose(maps);
    }
    if (found) {
      fprintf(stderr,
              "WARNING: libvgpu-control.so is loaded into this process. The\n"
              "         free-memory deltas below reflect the vGPU quota and our\n"
              "         own accounting, not the driver's. For the ground-truth\n"
              "         table, run this outside the container (or with\n"
              "         /etc/ld.so.preload and LD_PRELOAD cleared).\n\n");
    }
  }

  if (cuInit(0) != CUDA_SUCCESS) {
    fprintf(stderr, "cuInit failed\n");
    return 1;
  }
  if (cuDeviceGet(&dev, device_index) != CUDA_SUCCESS) {
    fprintf(stderr, "cuDeviceGet(%d) failed\n", device_index);
    return 1;
  }
  /* Primary context rather than cuCtxCreate: the latter is #defined to
   * cuCtxCreate_v2 up to CUDA 12 and to the four-argument cuCtxCreate_v4 from
   * CUDA 13 on, so a source that calls it only compiles against one era of
   * headers. Retain/SetCurrent has had the same signature throughout. */
  if (cuDevicePrimaryCtxRetain(&ctx, dev) != CUDA_SUCCESS) {
    fprintf(stderr, "cuDevicePrimaryCtxRetain failed\n");
    return 1;
  }
  if (cuCtxSetCurrent(ctx) != CUDA_SUCCESS) {
    fprintf(stderr, "cuCtxSetCurrent failed\n");
    cuDevicePrimaryCtxRelease(dev);
    return 1;
  }

  /* dlsym rather than a link-time reference: a pre-11.6 driver has no such
   * symbol, and the tool should still run and fall back to the delta. */
  libcuda = dlopen("libcuda.so.1", RTLD_LAZY | RTLD_NOLOAD);
  if (libcuda != NULL) {
    array_mem_reqs = (array_mem_reqs_fn)dlsym(libcuda, "cuArrayGetMemoryRequirements");
  }

  cuDriverGetVersion(&driver_version);
  cuDeviceGetName(name, sizeof(name), dev);
  cuMemGetInfo(&free_mem, &total_mem);

  if (!csv_mode) {
    printf("device %d: %s\n", device_index, name);
    printf("driver version: %d, built against CUDA %d\n", driver_version, CUDA_VERSION);
    printf("memory: %zu free / %zu total\n", free_mem, total_mem);
    printf("cuArrayGetMemoryRequirements: %s\n",
           array_mem_reqs ? "available" : "NOT available (delta only)");
    if (custom.set) {
      printf("probe geometry: --geom %zu,%zu,%zu (single, no sweep)\n\n",
             custom.width, custom.height, custom.depth);
    } else {
      printf("probe geometry: width %d and %d, height %d (2D) / %d (3D), depth %d\n\n",
             PROBE_WIDTH, PROBE_WIDTH * 2, PROBE_HEIGHT, PROBE_HEIGHT3, PROBE_DEPTH);
    }
    printf("exact      = cuArrayGetMemoryRequirements size (authoritative)\n");
    printf("free_delta = cuMemGetInfo drop across the allocation (upper bound,\n");
    printf("             quantised by the driver's suballocator)\n");
    printf("B/texel    = exact / (Width * Height * Depth), zero extents as 1\n\n");
  } else {
    printf("format,value,channels,shape,width,height,depth,create_result,"
           "exact_size,alignment,free_delta,exact_in_library_today\n");
  }

  for (size_t f = 0; f < sizeof(formats) / sizeof(formats[0]); f++) {
    int any = 0;
    if (!csv_mode) {
      printf("%s (0x%x)%s\n", formats[f].name, formats[f].value,
             formats[f].exact_today ? "  [sized exactly by the library today]" : "");
    }
    if (custom.set) {
      /* One exact geometry, no doubling: the caller is pinning a single rule
       * and wants the one number, not a sweep. */
      struct shape one = {"custom", custom.height, custom.depth};
      for (unsigned ch = 1; ch <= 4; ch *= 2) {
        any |= probe(dev, &formats[f], ch, &one, custom.width);
      }
    } else {
      for (size_t s = 0; s < sizeof(shapes) / sizeof(shapes[0]); s++) {
        for (unsigned ch = 1; ch <= 4; ch *= 2) {
          /* Both widths, so the caller can tell linear-in-Width apart from row
           * padding or 4x4 block rounding. */
          any |= probe(dev, &formats[f], ch, &shapes[s], PROBE_WIDTH);
          any |= probe(dev, &formats[f], ch, &shapes[s], PROBE_WIDTH * 2);
        }
      }
    }
    if (!csv_mode && !any) {
      printf("  (no accepted descriptor -- cuArray3DCreate refuses this format)\n");
    }
    if (!csv_mode) {
      printf("\n");
    }
  }

  cuDevicePrimaryCtxRelease(dev);
  return 0;
}
