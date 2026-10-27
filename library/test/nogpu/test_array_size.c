/* CUDA array descriptor sizing (include/array_size.h).
 *
 * Two bugs lived in the old get_array_base_size/get_array_request_size pair,
 * pulling the budget check in opposite directions, and nothing in the suite
 * touched either:
 *
 *   1. the format table returned the width in BITS (8/16/32) while the result
 *      is compared against a byte-denominated budget, so every 2D/3D array was
 *      charged 8x its real size -> false CUDA_ERROR_OUT_OF_MEMORY at an eighth
 *      of the container's limit;
 *   2. Height and Depth were multiplied in unconditionally, but cuda.h encodes
 *      an absent dimension as zero, so every 1D array -- and every 2D array
 *      created through the 3D descriptor -- was sized at 0 bytes and skipped
 *      the budget check entirely.
 *
 * The expected byte sizes below are taken from the worked descriptor examples
 * in NVIDIA's cuda.h, quoted in array_size.h, NOT from another implementation.
 * Needs no GPU, no driver and no CUDA toolkit: the arithmetic is pure.
 */
#include <stdint.h>
#include <stdio.h>

#include "include/array_size.h"

static int failures = 0;

#define CHECK(cond, ...)                                                       \
  do {                                                                         \
    if (!(cond)) {                                                             \
      printf("FAIL %s:%d: ", __func__, __LINE__);                              \
      printf(__VA_ARGS__);                                                     \
      printf("\n");                                                            \
      failures++;                                                              \
    }                                                                          \
  } while (0)

static CUDA_ARRAY_DESCRIPTOR desc2d(size_t w, size_t h, int fmt, unsigned ch) {
  CUDA_ARRAY_DESCRIPTOR d;
  d.Width = w;
  d.Height = h;
  d.Format = (CUarray_format)fmt;
  d.NumChannels = ch;
  return d;
}

static CUDA_ARRAY3D_DESCRIPTOR desc3d(size_t w, size_t h, size_t dp, int fmt,
                                      unsigned ch, unsigned flags) {
  CUDA_ARRAY3D_DESCRIPTOR d;
  d.Width = w;
  d.Height = h;
  d.Depth = dp;
  d.Format = (CUarray_format)fmt;
  d.NumChannels = ch;
  d.Flags = flags;
  return d;
}

/* Bytes per channel, straight from the two cuda.h examples:
 *   HALF x 4 channels == "64-bit"          -> HALF  is 2 bytes
 *   UNSIGNED_INT8 x 2 channels == "16-bit" -> INT8  is 1 byte
 * and the format names themselves for the rest ("Unsigned 32-bit integers"). */
static void test_format_bytes(void) {
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_UNSIGNED_INT8) == 1, "int8");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_SIGNED_INT8) == 1, "sint8");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_UNSIGNED_INT16) == 2, "int16");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_SIGNED_INT16) == 2, "sint16");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_HALF) == 2, "half");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_UNSIGNED_INT32) == 4, "int32");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_SIGNED_INT32) == 4, "sint32");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_FLOAT) == 4, "float");

  /* The regression guard proper: these were 8/16/32 (bits) and every one of
   * them over-charged by exactly 8x. */
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_UNSIGNED_INT8) != 8, "int8 still bits");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_HALF) != 16, "half still bits");
  CHECK(vgpu_array_format_bytes(CU_AD_FORMAT_FLOAT) != 32, "float still bits");

  CHECK(vgpu_array_format_is_known(CU_AD_FORMAT_FLOAT), "float known");
  CHECK(vgpu_array_format_is_known(CU_AD_FORMAT_BC7_UNORM), "bc7 measured");
  /* Still unmeasured: the driver refused both, so there is no number to use. */
  CHECK(!vgpu_array_format_is_known(CU_AD_FORMAT_BC6H_UF16), "bc6h not known");
  CHECK(!vgpu_array_format_is_known(CU_AD_FORMAT_NV12), "nv12 not known");
}

/* The cuda.h examples, as sizes. */
static void test_documented_examples(void) {
  size_t got;
  CUDA_ARRAY_DESCRIPTOR d;

  /* "Description for a CUDA array of 2048 floats" (Width 2048, Height 1). */
  d = desc2d(2048, 1, CU_AD_FORMAT_FLOAT, 1);
  CHECK(vgpu_array_size(&d, &got) && got == 2048 * 4, "2048 floats = %zu", got);

  /* "Description for a 64 x 64 CUDA array of floats". */
  d = desc2d(64, 64, CU_AD_FORMAT_FLOAT, 1);
  CHECK(vgpu_array_size(&d, &got) && got == 64 * 64 * 4, "64x64 float = %zu", got);

  /* "a width x height CUDA array of 64-bit, 4x16-bit float16's" -- 8 bytes
   * per element, which is the example that pins HALF at 2 bytes. */
  d = desc2d(100, 50, CU_AD_FORMAT_HALF, 4);
  CHECK(vgpu_array_size(&d, &got) && got == 100 * 50 * 8, "half x4 = %zu", got);

  /* "16-bit elements, each of which is two 8-bit unsigned chars". */
  d = desc2d(100, 50, CU_AD_FORMAT_UNSIGNED_INT8, 2);
  CHECK(vgpu_array_size(&d, &got) && got == 100 * 50 * 2, "int8 x2 = %zu", got);
}

/* "the CUDA array is one-dimensional if height is 0, two-dimensional
 * otherwise" -- an absent dimension is a factor of one. Height 0 and Height 1
 * therefore cost the same, and neither costs nothing. */
static void test_2d_zero_height(void) {
  size_t zero, one;
  CUDA_ARRAY_DESCRIPTOR h0 = desc2d(1024, 0, CU_AD_FORMAT_FLOAT, 4);
  CUDA_ARRAY_DESCRIPTOR h1 = desc2d(1024, 1, CU_AD_FORMAT_FLOAT, 4);

  CHECK(vgpu_array_size(&h0, &zero), "1D must be sizable");
  CHECK(vgpu_array_size(&h1, &one), "Height 1 must be sizable");
  CHECK(zero == one, "Height 0 (%zu) must equal Height 1 (%zu)", zero, one);
  CHECK(zero == 1024 * 4 * 4, "1D float x4 = %zu", zero);
  /* The old formula returned 0 here, which skipped the budget check. */
  CHECK(zero != 0, "a 1D array must not be free");
}

/* "A 1D array is allocated if Height and Depth extents are both zero. A 2D
 * array is allocated if only Depth extent is zero." */
static void test_3d_zero_extents(void) {
  size_t got;
  CUDA_ARRAY3D_DESCRIPTOR d;

  d = desc3d(256, 0, 0, CU_AD_FORMAT_UNSIGNED_INT8, 1, 0);
  CHECK(vgpu_array3d_size(&d, &got) && got == 256, "3D-desc 1D = %zu", got);

  d = desc3d(256, 128, 0, CU_AD_FORMAT_UNSIGNED_INT8, 1, 0);
  CHECK(vgpu_array3d_size(&d, &got) && got == 256 * 128, "3D-desc 2D = %zu", got);

  d = desc3d(256, 128, 64, CU_AD_FORMAT_UNSIGNED_INT8, 1, 0);
  CHECK(vgpu_array3d_size(&d, &got) && got == 256 * 128 * 64, "3D = %zu", got);

  /* Depth is the layer count under LAYERED and six under CUBEMAP; both are
   * full slices, so the product is unchanged and Flags needs no special case. */
  d = desc3d(256, 0, 8, CU_AD_FORMAT_UNSIGNED_INT8, 1, CUDA_ARRAY3D_LAYERED);
  CHECK(vgpu_array3d_size(&d, &got) && got == 256 * 8, "1D layered = %zu", got);

  d = desc3d(64, 64, 6, CU_AD_FORMAT_FLOAT, 4, CUDA_ARRAY3D_CUBEMAP);
  CHECK(vgpu_array3d_size(&d, &got) && got == 64 * 64 * 6 * 16, "cubemap = %zu", got);
}

/* A malformed descriptor reports "cannot size" so the hook forwards it to the
 * driver, which owns the verdict. It must never report a size of zero, which
 * would pass the budget check as free. */
static void test_malformed(void) {
  size_t got = 12345;
  CUDA_ARRAY_DESCRIPTOR d;

  CHECK(!vgpu_array_size(NULL, &got), "NULL desc");
  d = desc2d(64, 64, CU_AD_FORMAT_FLOAT, 1);
  CHECK(!vgpu_array_size(&d, NULL), "NULL out");

  /* "NumChannels ... may be 1, 2, or 4". */
  for (unsigned ch = 0; ch <= 8; ch++) {
    int want = (ch == 1 || ch == 2 || ch == 4);
    d = desc2d(64, 64, CU_AD_FORMAT_FLOAT, ch);
    CHECK(vgpu_array_size(&d, &got) == want, "NumChannels %u -> %d", ch, want);
  }

  /* Width has no "absent" encoding; leave a zero-element array to the driver. */
  d = desc2d(0, 64, CU_AD_FORMAT_FLOAT, 1);
  CHECK(!vgpu_array_size(&d, &got), "Width 0");
}

/* The product must refuse to wrap. Wrapping would turn a colossal request into
 * a small one and hand back a size that passes the budget check -- the exact
 * shape of bypass this sizing exists to prevent. */
static void test_overflow(void) {
  size_t got;
  CUDA_ARRAY_DESCRIPTOR d;
  CUDA_ARRAY3D_DESCRIPTOR d3;

  d = desc2d(SIZE_MAX, SIZE_MAX, CU_AD_FORMAT_FLOAT, 4);
  CHECK(!vgpu_array_size(&d, &got), "2D overflow");

  /* 2^32 x 2^32 x 4 bytes overflows 64-bit; each factor alone does not. */
  d = desc2d((size_t)1 << 32, (size_t)1 << 32, CU_AD_FORMAT_FLOAT, 1);
  CHECK(!vgpu_array_size(&d, &got), "2D overflow via product");

  d3 = desc3d((size_t)1 << 22, (size_t)1 << 22, (size_t)1 << 22,
              CU_AD_FORMAT_FLOAT, 4, 0);
  CHECK(!vgpu_array3d_size(&d3, &got), "3D overflow");

  /* Just below the edge still sizes, so the guard is not simply rejecting
   * everything large. */
  d = desc2d((size_t)1 << 20, (size_t)1 << 20, CU_AD_FORMAT_UNSIGNED_INT8, 1);
  CHECK(vgpu_array_size(&d, &got) && got == (size_t)1 << 40, "1Ti = %zu", got);
}

/* An unclassified format is charged the documented upper bound, never zero and
 * never a rejection: the hook has to stay usable for formats we have not
 * measured. BC6H and the planar YUV range are the ones still in that bucket --
 * the driver refused both on the probed hardware, so there is no number for
 * them (docs/cuda_array_size_measurements.md). */
static void test_unknown_format_is_conservative(void) {
  size_t bc6h, nv12;
  CUDA_ARRAY_DESCRIPTOR d;

  d = desc2d(1024, 1024, CU_AD_FORMAT_BC6H_UF16, 1);
  CHECK(vgpu_array_size(&d, &bc6h), "BC6H must still be sizable, not rejected");
  d = desc2d(1024, 1024, CU_AD_FORMAT_NV12, 1);
  CHECK(vgpu_array_size(&d, &nv12), "NV12 must still be sizable, not rejected");
  CHECK(bc6h != 0 && nv12 != 0, "unknown formats must not be free");
  CHECK(!vgpu_array_format_is_known(CU_AD_FORMAT_BC6H_UF16), "BC6H unmeasured");
  CHECK(!vgpu_array_format_is_known(CU_AD_FORMAT_NV12), "NV12 unmeasured");
}

/* The normalised families, against sizes measured on a real driver at
 * 1024x512 (docs/cuda_array_size_measurements.md). The driver requires
 * NumChannels to equal the count in the format name, which is what makes
 * bytes-per-channel x NumChannels reproduce the measurement -- the question
 * that blocked this table, since reading the name as "1 channel of 4 bytes"
 * instead would be off by 4x. */
static void test_measured_normalised_formats(void) {
  struct {
    int format;
    unsigned channels;
    size_t want;
  } cases[] = {
      {CU_AD_FORMAT_UNORM_INT8X1, 1, 524288},
      {CU_AD_FORMAT_UNORM_INT8X2, 2, 1048576},
      {CU_AD_FORMAT_UNORM_INT8X4, 4, 2097152},
      {CU_AD_FORMAT_UNORM_INT16X1, 1, 1048576},
      {CU_AD_FORMAT_UNORM_INT16X2, 2, 2097152},
      {CU_AD_FORMAT_UNORM_INT16X4, 4, 4194304},
      {CU_AD_FORMAT_SNORM_INT8X1, 1, 524288},
      {CU_AD_FORMAT_SNORM_INT8X2, 2, 1048576},
      {CU_AD_FORMAT_SNORM_INT8X4, 4, 2097152},
      {CU_AD_FORMAT_SNORM_INT16X1, 1, 1048576},
      {CU_AD_FORMAT_SNORM_INT16X2, 2, 2097152},
      {CU_AD_FORMAT_SNORM_INT16X4, 4, 4194304},
      {CU_AD_FORMAT_UNORM_INT_101010_2, 4, 2097152},
  };

  for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    size_t got;
    CUDA_ARRAY_DESCRIPTOR d = desc2d(1024, 512, cases[i].format, cases[i].channels);
    CHECK(vgpu_array_size(&d, &got) && got == cases[i].want,
          "format 0x%x ch=%u: got %zu want %zu", cases[i].format,
          cases[i].channels, got, cases[i].want);
    CHECK(vgpu_array_format_is_known(cases[i].format), "0x%x measured",
          cases[i].format);
  }
}

/* Block-compressed sizing, against the same measured run. Width and Height are
 * in texels and round up to whole 4x4 blocks; NumChannels does not multiply in,
 * even though the driver demands a specific one per format. */
static void test_measured_block_compressed(void) {
  struct {
    int format;
    unsigned channels; /* the one the driver accepts, to prove it is ignored */
    size_t want;       /* at 1024x512 */
  } cases[] = {
      {CU_AD_FORMAT_BC1_UNORM, 4, 262144},
      {CU_AD_FORMAT_BC1_UNORM_SRGB, 4, 262144},
      {CU_AD_FORMAT_BC4_UNORM, 1, 262144},
      {CU_AD_FORMAT_BC4_SNORM, 1, 262144},
      {CU_AD_FORMAT_BC2_UNORM, 4, 524288},
      {CU_AD_FORMAT_BC2_UNORM_SRGB, 4, 524288},
      {CU_AD_FORMAT_BC3_UNORM, 4, 524288},
      {CU_AD_FORMAT_BC3_UNORM_SRGB, 4, 524288},
      {CU_AD_FORMAT_BC5_UNORM, 2, 524288},
      {CU_AD_FORMAT_BC5_SNORM, 2, 524288},
      {CU_AD_FORMAT_BC7_UNORM, 4, 524288},
      {CU_AD_FORMAT_BC7_UNORM_SRGB, 4, 524288},
  };
  size_t got, at4096, at4097, at4100;
  CUDA_ARRAY_DESCRIPTOR d;

  for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    d = desc2d(1024, 512, cases[i].format, cases[i].channels);
    CHECK(vgpu_array_size(&d, &got) && got == cases[i].want,
          "BC 0x%x: got %zu want %zu", cases[i].format, got, cases[i].want);

    /* NumChannels must not enter the product. Every legal value gives the same
     * size, so a future refactor cannot quietly start multiplying by it. */
    for (unsigned ch = 1; ch <= 4; ch *= 2) {
      size_t same;
      CUDA_ARRAY_DESCRIPTOR e = desc2d(1024, 512, cases[i].format, ch);
      CHECK(vgpu_array_size(&e, &same) && same == cases[i].want,
            "BC 0x%x ch=%u must still be %zu, got %zu", cases[i].format, ch,
            cases[i].want, same);
    }
  }

  /* Block rounding, measured on BC1 at H=4096: 4097 costs one block column
   * more than 4096, and 4098/4099/4100 cost the same as 4097. */
  d = desc2d(4096, 4096, CU_AD_FORMAT_BC1_UNORM, 4);
  CHECK(vgpu_array_size(&d, &at4096) && at4096 == 8388608, "BC1 4096 = %zu", at4096);
  d = desc2d(4097, 4096, CU_AD_FORMAT_BC1_UNORM, 4);
  CHECK(vgpu_array_size(&d, &at4097) && at4097 == at4096 + 1024 * 8,
        "BC1 4097 must add one block column: %zu vs %zu", at4097, at4096);
  d = desc2d(4100, 4096, CU_AD_FORMAT_BC1_UNORM, 4);
  CHECK(vgpu_array_size(&d, &at4100) && at4100 == at4097,
        "BC1 4100 must match 4097: %zu vs %zu", at4100, at4097);

  /* Depth multiplies block planes as-is: 3D(H=256,D=4) measured identical to
   * 2D(H=1024). */
  {
    CUDA_ARRAY3D_DESCRIPTOR flat = desc3d(2048, 1024, 0, CU_AD_FORMAT_BC1_UNORM, 4, 0);
    CUDA_ARRAY3D_DESCRIPTOR deep = desc3d(2048, 256, 4, CU_AD_FORMAT_BC1_UNORM, 4, 0);
    size_t a, b;
    CHECK(vgpu_array3d_size(&flat, &a) && vgpu_array3d_size(&deep, &b) && a == b &&
              a == 1048576,
          "BC1 2D %zu vs 3D %zu, want 1048576", a, b);
  }
}

int main(void) {
  test_format_bytes();
  test_documented_examples();
  test_2d_zero_height();
  test_3d_zero_extents();
  test_malformed();
  test_overflow();
  test_measured_normalised_formats();
  test_measured_block_compressed();
  test_unknown_format_is_conservative();

  if (failures != 0) {
    printf("array size: %d failure(s)\n", failures);
    return 1;
  }
  printf("array size: all checks passed\n");
  return 0;
}
