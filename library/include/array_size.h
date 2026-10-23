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
 * EVERY RULE BELOW IS QUOTED FROM NVIDIA's cuda.h, not inferred:
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
 * product is the total either way and Flags needs no special case.
 *
 * NUMCHANNELS IS 1, 2 OR 4 -- "it may be 1, 2, or 4". Anything else is a
 * descriptor the driver rejects.
 *
 * WHAT THESE FUNCTIONS DO NOT MODEL: the driver is free to pad rows and tile
 * the allocation, so the product below is a LOWER bound on what a CUDA array
 * actually occupies. That is the right direction for a budget check (it never
 * invents usage), and the residue is picked up anyway -- the budget is checked
 * against live per-process usage read back from NVML, not against a ledger we
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
 * adding formats (cuda.h 13.4 defines ~50, of which the eight classic ones
 * below are the only ones whose element size is stated unambiguously), and
 * neither failing the allocation nor letting it through unmetered is
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
 * known format is ever under-charged. It does over-charge the compressed
 * ones -- a BC1 block is 8 bytes per 4x4 texels, i.e. half a byte per texel --
 * which is the conservative direction, and narrowing it needs a per-format
 * table whose every entry has to be verified the way the eight below were.
 * Until then vgpu_array_format_is_known() lets the call site log the gap so
 * it is visible rather than silent.
 */
#define VGPU_ARRAY_UNKNOWN_FORMAT_BYTES ((size_t)8)

/* Whether vgpu_array_format_bytes() knows this format exactly, as opposed to
 * falling back to VGPU_ARRAY_UNKNOWN_FORMAT_BYTES. */
static inline int vgpu_array_format_is_known(int format) {
  switch (format) {
  case CU_AD_FORMAT_UNSIGNED_INT8:
  case CU_AD_FORMAT_SIGNED_INT8:
  case CU_AD_FORMAT_UNSIGNED_INT16:
  case CU_AD_FORMAT_SIGNED_INT16:
  case CU_AD_FORMAT_HALF:
  case CU_AD_FORMAT_UNSIGNED_INT32:
  case CU_AD_FORMAT_SIGNED_INT32:
  case CU_AD_FORMAT_FLOAT:
    return 1;
  default:
    return 0;
  }
}

/* Bytes per channel of one array element. See the file comment for where each
 * value comes from; they match HAMi-core's cuarray_format_bytes[] table, which
 * was cross-checked against cuda.h rather than taken on trust. */
static inline size_t vgpu_array_format_bytes(int format) {
  switch (format) {
  case CU_AD_FORMAT_UNSIGNED_INT8:
  case CU_AD_FORMAT_SIGNED_INT8:
    return 1;
  case CU_AD_FORMAT_UNSIGNED_INT16:
  case CU_AD_FORMAT_SIGNED_INT16:
  case CU_AD_FORMAT_HALF:
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
 * For a mipmapped array this is the base level only. A full mip chain adds
 * roughly a third on top, which is left out on purpose: the exact tail depends
 * on numMipmapLevels and on the driver's per-level rounding, and guessing it
 * would trade a bounded under-estimate for an unbounded one in the other
 * direction. The NVML read-back covers the difference (see the file comment).
 */
static inline int vgpu_array3d_size(const CUDA_ARRAY3D_DESCRIPTOR *desc, size_t *out) {
  if (desc == NULL) {
    return 0;
  }
  return vgpu_array_size_from(desc->Width, desc->Height, desc->Depth,
                              (int)desc->Format, desc->NumChannels, out);
}

#endif /* _VGPU_ARRAY_SIZE_H_ */
