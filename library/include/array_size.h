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
 * How many device bytes a CUDA array descriptor asks for, as pure functions.
 *
 * Header-inline for the same reason as sm_delta.h: the hooks keep their
 * inlining AND the arithmetic is reachable from the no-GPU tests, so CI
 * exercises the shipped code instead of a replica that drifts from it. The
 * header deliberately pulls in nothing but the CUDA descriptor types -- no
 * logging, no driver, no shared region -- which is what makes it testable.
 *
 * EVERY RULE BELOW IS EITHER QUOTED FROM NVIDIA's cuda.h OR MEASURED AGAINST A
 * REAL DRIVER (docs/cuda_array_size_measurements.md). None of it is inferred:
 *
 * ELEMENT SIZE IS IN BYTES. The old table here returned 8/16/32, i.e. the
 * format's width in BITS, while the result is compared against a byte-
 * denominated budget (device_t.total_memory, which nvml_hook.c hands straight
 * to nvmlMemory_t.total). Every 2D/3D array was therefore charged 8x its real
 * size and containers hit a false CUDA_ERROR_OUT_OF_MEMORY at an eighth of
 * their limit. The driver API reference pins the byte sizes with two worked
 * descriptor examples:
 *
 *   "Description for a width x height CUDA array of 64-bit, 4x16-bit
 *    float16's:  Format = CU_AD_FORMAT_HALF; NumChannels = 4;"
 *        -> HALF x 4 channels == 64 bits, so HALF is 2 bytes.
 *
 *   "Description for a width x height CUDA array of 16-bit elements, each of
 *    which is two 8-bit unsigned chars:
 *    Format = CU_AD_FORMAT_UNSIGNED_INT8; NumChannels = 2;"
 *        -> UNSIGNED_INT8 x 2 channels == 16 bits, so INT8 is 1 byte.
 *
 * A ZERO EXTENT MEANS "ABSENT", WHICH IS A FACTOR OF ONE, NOT ZERO. The old
 * code multiplied Height (and Depth) in unconditionally, so every array with
 * a zero extent was sized at 0 bytes and skipped the budget check entirely --
 * the same unmetered path HAMi-core issue #372 reports, reached from the
 * opposite direction. cuda.h:
 *
 *   CUDA_ARRAY_DESCRIPTOR:   "the CUDA array is one-dimensional if height is
 *                             0, two-dimensional otherwise"
 *   CUDA_ARRAY3D_DESCRIPTOR: "A 1D array is allocated if Height and Depth
 *                             extents are both zero. A 2D array is allocated
 *                             if only Depth extent is zero."
 *
 * DEPTH ALWAYS MULTIPLIES, including under the flags that redefine it:
 * CUDA_ARRAY3D_LAYERED makes Depth "the number of layers, not the depth of a
 * 3D array", and CUDA_ARRAY3D_CUBEMAP requires Depth to be six (or a multiple
 * of six when layered). Layers and cube faces are each a full slice, so the
 * product is the total either way and Flags needs no special case -- measured:
 * a cubemap costs exactly six times its 2D face, a layered cubemap twelve.
 *
 * NUMCHANNELS IS 1, 2 OR 4 -- "it may be 1, 2, or 4". Anything else is a
 * descriptor the driver rejects.
 *
 * WHAT THESE FUNCTIONS DO NOT MODEL: the driver pads for its own tiling and
 * allocator -- rows to 64 bytes, 2D heights to 128 rows, a 3D array's Depth up
 * to the next power of two, a 1D array's per-layer stride by 8x, everything to
 * a 64 KiB granule -- so the product below is a LOWER bound on what a CUDA
 * array actually occupies. Those are architecture-dependent numbers and
 * deliberately stay out (docs/cuda_array_size_measurements.md explains the
 * sourcing rule). The lower bound is the right direction for a budget check
 * (it never invents usage), and the residue is picked up anyway -- the budget
 * is checked against live per-process usage read back from NVML, not a ledger we
 * maintain, so the driver's own accounting corrects any estimate on the very
 * next allocation. The same property is why an unsizable descriptor can be
 * forwarded unchecked: at worst one request slips through, and nothing
 * accumulates.
 */

#ifndef _VGPU_ARRAY_SIZE_H_
#define _VGPU_ARRAY_SIZE_H_

#include <stddef.h>
#include <stdint.h>

#include "include/cuda-subset.h"

/* Element size used for a format this table does not know.
 *
 * Deliberately an UPPER bound rather than a rejection or a zero: NVIDIA keeps
 * adding formats (cuda.h 13.4 defines 65, of which 35 are classified below),
 * and neither failing the allocation nor letting it through unmetered is
 * acceptable for one we have not classified yet.
 *
 *   - Rejecting is what HAMi-core PR #373 does (its table is 33 entries wide
 *     and anything outside it, or any zero entry, returns
 *     CUDA_ERROR_INVALID_VALUE). Every format introduced after CUDA 3.x --
 *     BC1..BC7 at 0x91-0x9e, NV12 at 0xb0, the UNORM/SNORM INTnXm family at
 *     0xc0-0xcb, the planar/packed YUV range at 0x9f-0xb5 and 0x50-0x5e --
 *     falls outside it, so that fix would break any application using a
 *     block-compressed or video-surface array outright. We do not copy that.
 *   - Charging zero is the bypass this header exists to close.
 *
 * 8 bytes is the widest element among every format cuda.h 13.4 defines
 * (UNORM_INT16X4 / SNORM_INT16X4 / Y416 are 4 x 16-bit = 8 bytes), so no
 * classified format is ever under-charged. It is a per-CHANNEL figure, so the
 * NumChannels further down multiplies it to as much as 32 bytes per texel --
 * more margin than any element needs, kept because for a format that does not
 * exist yet there is nothing to say whether its NumChannels multiplies.
 *
 * WHAT STILL LANDS HERE, after the measurements below:
 *   - the 30 planar/semi-planar/packed YUV formats. The driver refused every
 *     one -- 24 attempts each, every NumChannels and shape, on both probed
 *     architectures -- so they cannot reach these hooks at all; they arrive
 *     through external-memory import, which allocates nothing of ours. If some
 *     future driver does accept one, 8 bytes per channel OVER-charges it (NV12
 *     is 1.5 bytes per texel), which would OOM a workload that fits. We still
 *     do not guess: a semi-planar descriptor's Height may or may not cover the
 *     chroma planes, and that is unknowable without a driver that accepts it.
 *     Charge the bound, log the format (vgpu_array_format_is_known), measure it
 *     the day one shows up.
 *   - whatever NVIDIA adds next.
 */
#define VGPU_ARRAY_UNKNOWN_FORMAT_BYTES ((size_t)8)

/* Bytes one 4x4 block occupies, or 0 when the format is not block-compressed.
 *
 * MEASURED, not inferred: see docs/cuda_array_size_measurements.md. A block
 * size covers all of its format's channels, so NumChannels must NOT be
 * multiplied in on this path -- the driver confirmed it (BC1 with the
 * NumChannels=4 it demands still costs 8 bytes per block, not 32).
 *
 * BC6H is in the table on a measurement taken through OpenGL rather than CUDA:
 * cuArray3DCreate refuses BC6H on every shape, so the CUDA sweep cannot size
 * it, but GL_TEXTURE_COMPRESSED_IMAGE_SIZE on the same GPU and driver reports
 * 16 bytes per block for both halves of it. That path was calibrated first --
 * it returns 8/8/16/16/16/16 for BC1/BC4/BC2/BC3/BC5/BC7, matching the CUDA
 * numbers exactly. Leaving BC6H out would NOT have been the safe option: it
 * would fall through to VGPU_ARRAY_UNKNOWN_FORMAT_BYTES and be charged 8 bytes
 * per channel, i.e. 8x to 32x its real cost, the false-OOM direction.
 */
static inline size_t vgpu_array_block_bytes(int format) {
  switch (format) {
  /* 8 bytes per 4x4 block = 0.5 bytes per texel. */
  case CU_AD_FORMAT_BC1_UNORM:
  case CU_AD_FORMAT_BC1_UNORM_SRGB:
  case CU_AD_FORMAT_BC4_UNORM:
  case CU_AD_FORMAT_BC4_SNORM:
    return 8;
  /* 16 bytes per 4x4 block = 1 byte per texel. */
  case CU_AD_FORMAT_BC2_UNORM:
  case CU_AD_FORMAT_BC2_UNORM_SRGB:
  case CU_AD_FORMAT_BC3_UNORM:
  case CU_AD_FORMAT_BC3_UNORM_SRGB:
  case CU_AD_FORMAT_BC5_UNORM:
  case CU_AD_FORMAT_BC5_SNORM:
  case CU_AD_FORMAT_BC6H_UF16:
  case CU_AD_FORMAT_BC6H_SF16:
  case CU_AD_FORMAT_BC7_UNORM:
  case CU_AD_FORMAT_BC7_UNORM_SRGB:
    return 16;
  default:
    return 0;
  }
}

/* Channels the FORMAT ITSELF fixes, or 1 when the format leaves the count to
 * the descriptor's NumChannels.
 *
 * This exists so the size does not depend on what NumChannels a given driver
 * happens to accept. The probed driver requires NumChannels to equal the count
 * in the format name -- UNORM_INT8X4 takes only 4, X2 only 2, X1 only 1 -- so
 * "per-channel bytes x NumChannels" reproduced every measurement. But that is
 * an ACCEPTANCE rule, not a format definition: another driver, GPU generation
 * or OS could relax it, and if one accepted UNORM_INT8X4 with NumChannels=1 we
 * would charge a quarter of the real 4 bytes per texel. A quota bypass is
 * silent, so the arithmetic must not rest on a validation someone else
 * performs.
 *
 * The name states the element: "4 channel unsigned 8-bit normalized integer"
 * IS a 4-byte element. Taking the larger of the two counts therefore reproduces
 * the measurements exactly where the driver enforces the match, and still
 * charges the full element if some driver does not.
 *
 * The classic eight genuinely leave the count to the descriptor (cuda.h shows
 * UNSIGNED_INT8 used with 1, 2 and 4 channels), so they return 1 and the max
 * below is a no-op for them.
 */
static inline unsigned int vgpu_array_format_channels(int format) {
  switch (format) {
  case CU_AD_FORMAT_UNORM_INT8X2:
  case CU_AD_FORMAT_UNORM_INT16X2:
  case CU_AD_FORMAT_SNORM_INT8X2:
  case CU_AD_FORMAT_SNORM_INT16X2:
    return 2;
  case CU_AD_FORMAT_UNORM_INT8X4:
  case CU_AD_FORMAT_UNORM_INT16X4:
  case CU_AD_FORMAT_SNORM_INT8X4:
  case CU_AD_FORMAT_SNORM_INT16X4:
  /* "4 channel unorm R10G10B10A2": a packed 32-bit element, and the driver
   * requires NumChannels=4 for it. */
  case CU_AD_FORMAT_UNORM_INT_101010_2:
    return 4;
  default:
    /* Includes the X1 formats, whose one channel the max cannot change. */
    return 1;
  }
}

/* Whether this format's size is known exactly, as opposed to falling back to
 * VGPU_ARRAY_UNKNOWN_FORMAT_BYTES. */
static inline int vgpu_array_format_is_known(int format) {
  if (vgpu_array_block_bytes(format) != 0) {
    return 1;
  }
  switch (format) {
  case CU_AD_FORMAT_UNSIGNED_INT8:
  case CU_AD_FORMAT_SIGNED_INT8:
  case CU_AD_FORMAT_UNSIGNED_INT16:
  case CU_AD_FORMAT_SIGNED_INT16:
  case CU_AD_FORMAT_HALF:
  case CU_AD_FORMAT_UNSIGNED_INT32:
  case CU_AD_FORMAT_SIGNED_INT32:
  case CU_AD_FORMAT_FLOAT:
  case CU_AD_FORMAT_UNORM_INT8X1:
  case CU_AD_FORMAT_UNORM_INT8X2:
  case CU_AD_FORMAT_UNORM_INT8X4:
  case CU_AD_FORMAT_SNORM_INT8X1:
  case CU_AD_FORMAT_SNORM_INT8X2:
  case CU_AD_FORMAT_SNORM_INT8X4:
  case CU_AD_FORMAT_UNORM_INT_101010_2:
  case CU_AD_FORMAT_UNORM_INT16X1:
  case CU_AD_FORMAT_UNORM_INT16X2:
  case CU_AD_FORMAT_UNORM_INT16X4:
  case CU_AD_FORMAT_SNORM_INT16X1:
  case CU_AD_FORMAT_SNORM_INT16X2:
  case CU_AD_FORMAT_SNORM_INT16X4:
    return 1;
  default:
    return 0;
  }
}

/* Bytes per channel of one array element, for the formats sized as
 * bytes-per-channel x NumChannels x W x H x D. Block-compressed formats do not
 * go through here; vgpu_array_block_bytes() handles them.
 *
 * The classic eight come from cuda.h's own worked examples (file comment). The
 * normalised families are MEASURED -- the X-suffix names state a channel count
 * ("4 channel unsigned 8-bit") and the driver turned out to require
 * NumChannels to match it exactly, so the per-channel figure here times that
 * NumChannels reproduces the measured size. UNORM_INT_101010_2 is a packed
 * 32-bit element that likewise demands NumChannels=4, so 1 byte per channel
 * lands on the measured 4 bytes per texel. See
 * docs/cuda_array_size_measurements.md.
 */
static inline size_t vgpu_array_format_bytes(int format) {
  switch (format) {
  case CU_AD_FORMAT_UNSIGNED_INT8:
  case CU_AD_FORMAT_SIGNED_INT8:
  /* Measured: *INT8X{1,2,4} with NumChannels {1,2,4} respectively. */
  case CU_AD_FORMAT_UNORM_INT8X1:
  case CU_AD_FORMAT_UNORM_INT8X2:
  case CU_AD_FORMAT_UNORM_INT8X4:
  case CU_AD_FORMAT_SNORM_INT8X1:
  case CU_AD_FORMAT_SNORM_INT8X2:
  case CU_AD_FORMAT_SNORM_INT8X4:
  /* Measured: 4 bytes per texel at the NumChannels=4 the driver requires. */
  case CU_AD_FORMAT_UNORM_INT_101010_2:
    return 1;
  case CU_AD_FORMAT_UNSIGNED_INT16:
  case CU_AD_FORMAT_SIGNED_INT16:
  case CU_AD_FORMAT_HALF:
  /* Measured: *INT16X{1,2,4} with NumChannels {1,2,4} respectively. */
  case CU_AD_FORMAT_UNORM_INT16X1:
  case CU_AD_FORMAT_UNORM_INT16X2:
  case CU_AD_FORMAT_UNORM_INT16X4:
  case CU_AD_FORMAT_SNORM_INT16X1:
  case CU_AD_FORMAT_SNORM_INT16X2:
  case CU_AD_FORMAT_SNORM_INT16X4:
    return 2;
  case CU_AD_FORMAT_UNSIGNED_INT32:
  case CU_AD_FORMAT_SIGNED_INT32:
  case CU_AD_FORMAT_FLOAT:
    return 4;
  default:
    return VGPU_ARRAY_UNKNOWN_FORMAT_BYTES;
  }
}

/* *acc *= factor, refusing to wrap. Returns 0 on overflow, and on a zero
 * factor: every factor here is validated or normalised to at least one first,
 * so a zero reaching this is a bug in the caller rather than a free
 * allocation, and silently collapsing the product to zero would be the very
 * under-charge this header fixes. */
static inline int vgpu_array_mul(size_t *acc, size_t factor) {
  if (factor == 0 || *acc > SIZE_MAX / factor) {
    return 0;
  }
  *acc *= factor;
  return 1;
}

/* Shared by both descriptor shapes. depth is 1 for the 2D descriptor, which
 * has no Depth member. Returns 1 with *out set, or 0 when the descriptor
 * cannot be sized (malformed, or the product overflows) -- the caller then
 * forwards to the driver unchecked and lets it produce the canonical error. */
static inline int vgpu_array_size_from(size_t width, size_t height, size_t depth,
                                       int format, unsigned int num_channels,
                                       size_t *out) {
  size_t bytes;

  if (out == NULL) {
    return 0;
  }
  /* "NumChannels specifies the number of packed components per CUDA array
   * element; it may be 1, 2, or 4". */
  if (num_channels != 1 && num_channels != 2 && num_channels != 4) {
    return 0;
  }
  /* Width has no documented "absent" encoding: an array of zero elements is
   * not something the driver allocates, so hand it the descriptor and let it
   * answer. Height and Depth DO encode absence as zero, normalised below. */
  if (width == 0) {
    return 0;
  }
  if (height == 0) {
    height = 1;
  }
  if (depth == 0) {
    depth = 1;
  }

  /* Block-compressed formats are sized per 4x4 block, with Width and Height in
   * TEXELS and rounded up to whole blocks -- measured, not assumed: BC1 at
   * width 4097 costs the same as 4098/4099/4100 and one block column more than
   * 4096. NumChannels is deliberately absent from the product; a block size
   * already covers the format's channels, and the driver demands a specific
   * NumChannels per BC format anyway. Depth multiplies block planes as-is (a
   * 3D or layered BC array measured identical to the 2D one of equal
   * Height x Depth). See docs/cuda_array_size_measurements.md. */
  bytes = vgpu_array_block_bytes(format);
  if (bytes != 0) {
    /* ceil(extent / 4) written as a division plus a remainder test rather than
     * (extent + 3) / 4: the latter wraps for an extent within 3 of SIZE_MAX,
     * and a wrap there turns a colossal request into a block count of zero.
     * That happens to be caught today by vgpu_array_mul() refusing a zero
     * factor, but relying on a guard three calls away for the difference
     * between "refuse" and "charge nothing" is how a later cleanup of that
     * guard would silently reopen the bypass. This form cannot overflow. */
    size_t blocks_w = width / 4 + (width % 4 != 0 ? 1 : 0);
    size_t blocks_h = height / 4 + (height % 4 != 0 ? 1 : 0);
    if (!vgpu_array_mul(&bytes, blocks_w) || !vgpu_array_mul(&bytes, blocks_h) ||
        !vgpu_array_mul(&bytes, depth)) {
      return 0;
    }
    *out = bytes;
    return 1;
  }

  /* The larger of the descriptor's count and the one the format fixes, so the
   * element size does not depend on a driver's NumChannels validation. See
   * vgpu_array_format_channels(). */
  {
    unsigned int implied = vgpu_array_format_channels(format);
    if (implied > num_channels) {
      num_channels = implied;
    }
  }

  bytes = vgpu_array_format_bytes(format);
  if (!vgpu_array_mul(&bytes, (size_t)num_channels) ||
      !vgpu_array_mul(&bytes, width) ||
      !vgpu_array_mul(&bytes, height) ||
      !vgpu_array_mul(&bytes, depth)) {
    return 0;
  }
  *out = bytes;
  return 1;
}

/* cuArrayCreate / cuArrayCreate_v2 descriptors (1D or 2D). */
static inline int vgpu_array_size(const CUDA_ARRAY_DESCRIPTOR *desc, size_t *out) {
  if (desc == NULL) {
    return 0;
  }
  return vgpu_array_size_from(desc->Width, desc->Height, 1,
                              (int)desc->Format, desc->NumChannels, out);
}

/* cuArray3DCreate / cuArray3DCreate_v2 / cuMipmappedArrayCreate descriptors
 * (1D, 2D, 3D, layered or cubemap).
 *
 * For a mipmapped array this is the base level only, which is left out of the
 * charge on purpose: the tail depends on numMipmapLevels and on the driver's
 * per-level rounding, and guessing it would trade a bounded under-estimate for
 * an unbounded over-estimate. Bounded is measured, not assumed -- a full chain
 * saturates at 1.375x the base level and never grows past it, however many
 * levels are asked for (docs/cuda_array_size_measurements.md). The NVML
 * read-back covers the difference (see the file comment).
 */
static inline int vgpu_array3d_size(const CUDA_ARRAY3D_DESCRIPTOR *desc, size_t *out) {
  if (desc == NULL) {
    return 0;
  }
  return vgpu_array_size_from(desc->Width, desc->Height, desc->Depth,
                              (int)desc->Format, desc->NumChannels, out);
}

#endif /* _VGPU_ARRAY_SIZE_H_ */
