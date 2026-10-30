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
 * cuMipmappedArrayCreate. cuda.h pins the eight classic formats with two
 * worked descriptor examples. For every format added after CUDA 3.x --
 * BC1..BC7, the UNORM/SNORM INTnXm family, and the planar/semi-planar/packed
 * YUV range -- NVIDIA documents a one-line description and NOTHING about
 * sizing: not bytes per element, not whether Width/Height count texels or 4x4
 * blocks, not which NumChannels values are even legal.
 *
 * Those numbers can only come from the driver. Guessing from format names is
 * exactly what this tool exists to avoid: the open question that blocked the
 * table was whether UNORM_INT8X4 ("4 channel unsigned 8-bit") wants
 * NumChannels 1 or 4, and those two readings differ by 4x in a component that
 * enforces a memory limit.
 *
 * The table in array_size.h is now built from a run of this tool, so the tool's
 * standing job is the other direction: re-run it on new hardware, a new driver
 * or a new OS and replay the CSV through array_size_verify to confirm nothing
 * we ship has become an over-charge. Whatever stays outside the table falls
 * back to VGPU_ARRAY_UNKNOWN_FORMAT_BYTES.
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
 *   ./array_size_probe [device_index] [--csv] [--geom W[,H[,D]] | --extra]
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

/* Resolved at runtime; see the header comment. */
typedef CUresult (*array_mem_reqs_fn)(CUDA_ARRAY_MEMORY_REQUIREMENTS *, CUarray, CUdevice);
static array_mem_reqs_fn array_mem_reqs;

static int csv_mode;

/* --geom: one exact geometry instead of the shape sweep, for pinning a single
 * rule (see --help). */
static struct {
  int set;
  size_t width, height, depth;
} custom;

/* --extra: the handful of rules the format sweep cannot express, because they
 * are about Flags, Depth or mip levels rather than about a format. */
static int extra_mode;

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
    {"UNSIGNED_INT8", 0x01},
    {"UNSIGNED_INT16", 0x02},
    {"UNSIGNED_INT32", 0x03},
    {"SIGNED_INT8", 0x08},
    {"SIGNED_INT16", 0x09},
    {"SIGNED_INT32", 0x0a},
    {"HALF", 0x10},
    {"FLOAT", 0x20},
    {"NV12", 0xb0},
    {"UNORM_INT8X1", 0xc0},
    {"UNORM_INT8X2", 0xc1},
    {"UNORM_INT8X4", 0xc2},
    {"UNORM_INT16X1", 0xc3},
    {"UNORM_INT16X2", 0xc4},
    {"UNORM_INT16X4", 0xc5},
    {"SNORM_INT8X1", 0xc6},
    {"SNORM_INT8X2", 0xc7},
    {"SNORM_INT8X4", 0xc8},
    {"SNORM_INT16X1", 0xc9},
    {"SNORM_INT16X2", 0xca},
    {"SNORM_INT16X4", 0xcb},
    {"BC1_UNORM", 0x91},
    {"BC1_UNORM_SRGB", 0x92},
    {"BC2_UNORM", 0x93},
    {"BC2_UNORM_SRGB", 0x94},
    {"BC3_UNORM", 0x95},
    {"BC3_UNORM_SRGB", 0x96},
    {"BC4_UNORM", 0x97},
    {"BC4_SNORM", 0x98},
    {"BC5_UNORM", 0x99},
    {"BC5_SNORM", 0x9a},
    {"BC6H_UF16", 0x9b},
    {"BC6H_SF16", 0x9c},
    {"BC7_UNORM", 0x9d},
    {"BC7_UNORM_SRGB", 0x9e},
    {"P010", 0x9f},
    {"P016", 0xa1},
    {"NV16", 0xa2},
    {"P210", 0xa3},
    {"P216", 0xa4},
    {"YUY2", 0xa5},
    {"Y210", 0xa6},
    {"Y216", 0xa7},
    {"AYUV", 0xa8},
    {"Y410", 0xa9},
    {"Y416", 0xb1},
    {"Y444_PLANAR8", 0xb2},
    {"Y444_PLANAR10", 0xb3},
    {"YUV444_8bit_SemiPlanar", 0xb4},
    {"YUV444_16bit_SemiPlanar", 0xb5},
    {"UNORM_INT_101010_2", 0x50},
    {"UINT8_PACKED_422", 0x51},
    {"UINT8_PACKED_444", 0x52},
    {"UINT8_SEMIPLANAR_420", 0x53},
    {"UINT16_SEMIPLANAR_420", 0x54},
    {"UINT8_SEMIPLANAR_422", 0x55},
    {"UINT16_SEMIPLANAR_422", 0x56},
    {"UINT8_SEMIPLANAR_444", 0x57},
    {"UINT16_SEMIPLANAR_444", 0x58},
    {"UINT8_PLANAR_420", 0x59},
    {"UINT16_PLANAR_420", 0x5a},
    {"UINT8_PLANAR_422", 0x5b},
    {"UINT16_PLANAR_422", 0x5c},
    {"UINT8_PLANAR_444", 0x5d},
    {"UINT16_PLANAR_444", 0x5e},
};

/* The four descriptor shapes that matter, named the way cuda.h names them:
 * "A 1D array is allocated if Height and Depth extents are both zero. A 2D
 * array is allocated if only Depth extent is zero." */
struct shape {
  const char *name;
  size_t height; /* 0 leaves the extent at zero, i.e. the dimension is absent */
  size_t depth;
  unsigned flags;
};

static const struct shape shapes[] = {
    {"1D", 0, 0, 0},
    {"2D", PROBE_HEIGHT, 0, 0},
    {"3D", PROBE_HEIGHT3, PROBE_DEPTH, 0},
    /* Same extents as 3D, so the pair isolates what the LAYERED flag changes
     * from what the extents change. */
    {"2Dlayered", PROBE_HEIGHT3, PROBE_DEPTH, CUDA_ARRAY3D_LAYERED},
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
  unsigned flags = shape->flags;

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
    printf("%s,0x%x,%u,%s,%zu,%zu,%zu,%s,%zu,%zu,%zu\n", fmt->name, fmt->value,
           channels, shape->name, desc.Width, desc.Height, desc.Depth,
           result_name(create_res), exact, align, delta);
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

/* Rules that are not about a format and so have no place in the sweep: what
 * CUDA_ARRAY3D_CUBEMAP / _LAYERED do to the size, how the driver pads a 3D
 * Depth and a 1D array's layers, and how a mip chain grows. array_size.h makes
 * a claim about each one; these are the measurements behind them.
 *
 * Geometries are small and fixed so the numbers can be compared against the
 * ones quoted in array_size.h and test/nogpu/test_array_size.c verbatim.
 */
struct extra_case {
  struct format_entry fmt;
  unsigned channels;
  size_t width;
  struct shape shape;
};

static const struct extra_case extras[] = {
    /* A cubemap is six faces and a layered cubemap twelve, so Depth carries
     * the count and Flags needs no special case in the arithmetic. The plain
     * 2D face is here to divide by. */
    {{"FLOAT", 0x20}, 4, 64, {"2D", 64, 0, 0}},
    {{"FLOAT", 0x20}, 4, 64, {"cubemap", 64, 6, CUDA_ARRAY3D_CUBEMAP}},
    {{"FLOAT", 0x20}, 4, 64,
     {"cubeLayer", 64, 12, CUDA_ARRAY3D_CUBEMAP | CUDA_ARRAY3D_LAYERED}},
    /* 3D Depth is rounded up to the next power of two (6 costs 8 slices) while
     * a 2D layered Depth is not (6 costs 6). Same extents, so the difference
     * is the flag. We model neither: both round UP, which keeps us a lower
     * bound. */
    {{"UNSIGNED_INT8", 0x01}, 4, 512, {"2D", 512, 0, 0}},
    {{"UNSIGNED_INT8", 0x01}, 4, 512, {"3D", 512, 6, 0}},
    {{"UNSIGNED_INT8", 0x01}, 4, 512,
     {"2Dlayered", 512, 6, CUDA_ARRAY3D_LAYERED}},
    /* 1D layers: the per-layer stride is inflated (here 8x), which is NOT the
     * layer count being rounded up. The two rows below tell those apart --
     * rounding the layer count to 64 would predict 2048x64 and 16384x64, and
     * only the first of those matches. */
    {{"UNSIGNED_INT8", 0x01}, 1, 2048, {"1Dlayered", 0, 8, CUDA_ARRAY3D_LAYERED}},
    {{"UNSIGNED_INT8", 0x01}, 1, 16384, {"1Dlayered", 0, 4, CUDA_ARRAY3D_LAYERED}},
};

/* How much a mip chain adds over the base level. array_size.h charges the base
 * level only, so this is the size of the under-estimate it accepts; the point
 * of measuring it is that the ratio turns out to SATURATE, i.e. the
 * under-estimate is bounded no matter what numMipmapLevels asks for. */
typedef CUresult (*mip_mem_reqs_fn)(CUDA_ARRAY_MEMORY_REQUIREMENTS *,
                                    CUmipmappedArray, CUdevice);

static void probe_mipmap(CUdevice dev, FILE *out, mip_mem_reqs_fn mip_reqs) {
  static const unsigned levels[] = {1, 2, 4, 8, 12, 20};
  static const struct {
    const char *name;
    int format;
    unsigned channels;
  } cases[] = {
      {"UNSIGNED_INT8", 0x01, 1},
      {"UNSIGNED_INT8", 0x01, 4},
      {"FLOAT", 0x20, 1},
      {"BC1_UNORM", 0x91, 4},
  };

  if (mip_reqs == NULL) {
    fprintf(out, "\ncuMipmappedArrayGetMemoryRequirements unavailable; "
                 "skipping the mip chain\n");
    return;
  }
  fprintf(out, "\nmip chain over the base level, 2D 1024x1024:\n");
  for (size_t c = 0; c < sizeof(cases) / sizeof(cases[0]); c++) {
    size_t base = 0;
    fprintf(out, "  %-16s ch=%u", cases[c].name, cases[c].channels);
    for (size_t l = 0; l < sizeof(levels) / sizeof(levels[0]); l++) {
      CUDA_ARRAY3D_DESCRIPTOR desc;
      CUmipmappedArray mip = NULL;
      CUDA_ARRAY_MEMORY_REQUIREMENTS reqs;

      memset(&desc, 0, sizeof(desc));
      desc.Width = 1024;
      desc.Height = 1024;
      desc.Format = (CUarray_format)cases[c].format;
      desc.NumChannels = cases[c].channels;
      desc.Flags = CUDA_ARRAY3D_DEFERRED_MAPPING;
      if (cuMipmappedArrayCreate(&mip, &desc, levels[l]) != CUDA_SUCCESS) {
        fprintf(out, "  L%u=rejected", levels[l]);
        continue;
      }
      memset(&reqs, 0, sizeof(reqs));
      if (mip_reqs(&reqs, mip, dev) == CUDA_SUCCESS) {
        if (levels[l] == 1) {
          base = reqs.size;
        }
        fprintf(out, "  L%u=%zu", levels[l], reqs.size);
        if (base != 0) {
          fprintf(out, "(%.3fx)", (double)reqs.size / (double)base);
        }
      }
      cuMipmappedArrayDestroy(mip);
    }
    fprintf(out, "\n");
  }
}

static void probe_extra(CUdevice dev, mip_mem_reqs_fn mip_reqs) {
  /* The array rows go through probe(), so --csv feeds them to
   * array_size_verify like any other row. The mip table cannot: array_size.h
   * does not model the chain, so it is printed for reading and kept off the
   * CSV stream. */
  FILE *out = csv_mode ? stderr : stdout;

  for (size_t i = 0; i < sizeof(extras) / sizeof(extras[0]); i++) {
    probe(dev, &extras[i].fmt, extras[i].channels, &extras[i].shape,
          extras[i].width);
  }
  probe_mipmap(dev, out, mip_reqs);
}

int main(int argc, char **argv) {
  int device_index = 0;
  CUdevice dev;
  CUcontext ctx;
  char name[256];
  int driver_version = 0;
  size_t free_mem = 0, total_mem = 0;
  void *libcuda;
  mip_mem_reqs_fn mip_mem_reqs = NULL;

  for (int i = 1; i < argc; i++) {
    if (strcmp(argv[i], "--csv") == 0) {
      csv_mode = 1;
    } else if (strcmp(argv[i], "--geom") == 0 && i + 1 < argc) {
      /* Width 0 is rejected rather than passed through: every descriptor would
       * come back REJECTED and the run would look like a driver finding. */
      if (sscanf(argv[++i], "%zu,%zu,%zu", &custom.width, &custom.height,
                 &custom.depth) < 1 ||
          custom.width == 0) {
        fprintf(stderr, "--geom wants W[,H[,D]] with a non-zero W\n");
        return 1;
      }
      custom.set = 1;
    } else if (strcmp(argv[i], "--extra") == 0) {
      extra_mode = 1;
    } else if (strcmp(argv[i], "--help") == 0) {
      printf("usage: %s [device_index] [--csv] [--geom W[,H[,D]] | --extra]\n",
             argv[0]);
      printf("  --geom  replaces the shape sweep with one exact geometry and\n");
      printf("          does not double the width, for pinning a specific rule\n");
      printf("          (block rounding at a width not divisible by 4, row\n");
      printf("          padding at an awkward width, the allocation floor).\n");
      printf("  --extra replaces the sweep with the rules that are about Flags,\n");
      printf("          Depth or mip levels rather than about a format: cubemap\n");
      printf("          and layered sizing, 3D depth padding, 1D layer padding,\n");
      printf("          and how much a mip chain adds. The mip table goes to\n");
      printf("          stderr under --csv so it stays out of the CSV stream.\n");
      return 0;
    } else {
      device_index = atoi(argv[i]);
    }
  }

  if (extra_mode && custom.set) {
    fprintf(stderr, "--extra and --geom pick different probe sets; use one\n");
    return 1;
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
    mip_mem_reqs =
        (mip_mem_reqs_fn)dlsym(libcuda, "cuMipmappedArrayGetMemoryRequirements");
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
    if (extra_mode) {
      printf("probe geometry: --extra (Flags / Depth / mip rules, no sweep)\n\n");
    } else if (custom.set) {
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
           "exact_size,alignment,free_delta\n");
  }

  if (extra_mode) {
    probe_extra(dev, mip_mem_reqs);
    cuDevicePrimaryCtxRelease(dev);
    return 0;
  }

  for (size_t f = 0; f < sizeof(formats) / sizeof(formats[0]); f++) {
    int any = 0;
    if (!csv_mode) {
      printf("%s (0x%x)\n", formats[f].name, formats[f].value);
    }
    if (custom.set) {
      /* One exact geometry, no doubling: the caller is pinning a single rule
       * and wants the one number, not a sweep. */
      struct shape one = {"custom", custom.height, custom.depth, 0};
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
