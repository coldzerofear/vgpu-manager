/*
 * Copyright 1993-2018 NVIDIA Corporation.  All rights reserved.
 *
 * NOTICE TO LICENSEE:
 *
 * This source code and/or documentation ("Licensed Deliverables") are
 * subject to NVIDIA intellectual property rights under U.S. and
 * international Copyright laws.
 *
 * These Licensed Deliverables contained herein is PROPRIETARY and
 * CONFIDENTIAL to NVIDIA and is being provided under the terms and
 * conditions of a form of NVIDIA software license agreement by and
 * between NVIDIA and Licensee ("License Agreement") or electronically
 * accepted by Licensee.  Notwithstanding any terms or conditions to
 * the contrary in the License Agreement, reproduction or disclosure
 * of the Licensed Deliverables to any third party without the express
 * written consent of NVIDIA is prohibited.
 *
 * NOTWITHSTANDING ANY TERMS OR CONDITIONS TO THE CONTRARY IN THE
 * LICENSE AGREEMENT, NVIDIA MAKES NO REPRESENTATION ABOUT THE
 * SUITABILITY OF THESE LICENSED DELIVERABLES FOR ANY PURPOSE.  IT IS
 * PROVIDED "AS IS" WITHOUT EXPRESS OR IMPLIED WARRANTY OF ANY KIND.
 * NVIDIA DISCLAIMS ALL WARRANTIES WITH REGARD TO THESE LICENSED
 * DELIVERABLES, INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY,
 * NONINFRINGEMENT, AND FITNESS FOR A PARTICULAR PURPOSE.
 * NOTWITHSTANDING ANY TERMS OR CONDITIONS TO THE CONTRARY IN THE
 * LICENSE AGREEMENT, IN NO EVENT SHALL NVIDIA BE LIABLE FOR ANY
 * SPECIAL, INDIRECT, INCIDENTAL, OR CONSEQUENTIAL DAMAGES, OR ANY
 * DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS,
 * WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS
 * ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE
 * OF THESE LICENSED DELIVERABLES.
 *
 * U.S. Government End Users.  These Licensed Deliverables are a
 * "commercial item" as that term is defined at 48 C.F.R. 2.101 (OCT
 * 1995), consisting of "commercial computer software" and "commercial
 * computer software documentation" as such terms are used in 48
 * C.F.R. 12.212 (SEPT 1995) and is provided to the U.S. Government
 * only as a commercial end item.  Consistent with 48 C.F.R.12.212 and
 * 48 C.F.R. 227.7202-1 through 227.7202-4 (JUNE 1995), all
 * U.S. Government End Users acquire the Licensed Deliverables with
 * only those rights set forth herein.
 *
 * Any use of the Licensed Deliverables in individual and commercial
 * software must include, in the user documentation and internal
 * comments to the code, the above Disclaimer and U.S. Government End
 * Users Notice.
 */
#ifndef HIJACK_CUDA_SUBSET_H
#define HIJACK_CUDA_SUBSET_H

/**
 * CUDA API version number
 */
#define CUDA_VERSION 12020

#ifdef __cplusplus
extern "C" {
#endif

#include <stdlib.h>
#include <stddef.h>
#include <stdint.h>
#include "vdpau/vdpau.h"

typedef uint32_t cuuint32_t;
typedef uint64_t cuuint64_t;

typedef unsigned int GLuint; /* 4-byte unsigned */
typedef unsigned int GLenum;

/**
 * CUDA device pointer
 * CUdeviceptr is defined as an unsigned integer type whose size matches the size of a pointer on the target platform.
 */
#if defined(_WIN64) || defined(__LP64__)
typedef unsigned long long CUdeviceptr_v2;
#else
typedef unsigned int CUdeviceptr_v2;
#endif
typedef CUdeviceptr_v2 CUdeviceptr;                          /**< CUDA device pointer */

typedef int CUdevice_v1;                                     /**< CUDA device */
typedef CUdevice_v1 CUdevice;                                /**< CUDA device */
typedef struct CUctx_st *CUcontext;                          /**< CUDA context */
typedef struct CUmod_st *CUmodule;                           /**< CUDA module */
typedef struct CUfunc_st *CUfunction;                        /**< CUDA function */
typedef struct CUlib_st *CUlibrary;                          /**< CUDA library */
typedef struct CUkern_st *CUkernel;                          /**< CUDA kernel */
typedef struct CUarray_st *CUarray;                          /**< CUDA array */
typedef struct CUmipmappedArray_st *CUmipmappedArray;        /**< CUDA mipmapped array */
typedef struct CUtexref_st *CUtexref;                        /**< CUDA texture reference */
typedef struct CUsurfref_st *CUsurfref;                      /**< CUDA surface reference */
typedef struct CUevent_st *CUevent;                          /**< CUDA event */
typedef struct CUstream_st *CUstream;                        /**< CUDA stream */
typedef struct CUgraphicsResource_st *CUgraphicsResource;    /**< CUDA graphics interop resource */
typedef unsigned long long CUtexObject_v1;                   /**< An opaque value that represents a CUDA texture object */
typedef CUtexObject_v1 CUtexObject;                          /**< An opaque value that represents a CUDA texture object */
typedef unsigned long long CUsurfObject_v1;                  /**< An opaque value that represents a CUDA surface object */
typedef CUsurfObject_v1 CUsurfObject;                        /**< An opaque value that represents a CUDA surface object */ 
typedef struct CUextMemory_st *CUexternalMemory;             /**< CUDA external memory */
typedef struct CUextSemaphore_st *CUexternalSemaphore;       /**< CUDA external semaphore */
typedef struct CUgraph_st *CUgraph;                          /**< CUDA graph */
typedef struct CUgraphNode_st *CUgraphNode;                  /**< CUDA graph node */
typedef struct CUgraphExec_st *CUgraphExec;                  /**< CUDA executable graph */
typedef struct CUmemPoolHandle_st *CUmemoryPool;             /**< CUDA memory pool */
typedef struct CUuserObject_st *CUuserObject;                /**< CUDA user object for graphs */

#ifndef CU_UUID_HAS_BEEN_DEFINED
#define CU_UUID_HAS_BEEN_DEFINED
typedef struct CUuuid_st { /**< CUDA definition of UUID */
  char bytes[16];
} CUuuid;
#endif

/**
 * CUDA IPC handle size
 */
#define CU_IPC_HANDLE_SIZE 64

/**
 * CUDA IPC event handle
 */
typedef struct CUipcEventHandle_st {
    char reserved[CU_IPC_HANDLE_SIZE];
} CUipcEventHandle_v1;
typedef CUipcEventHandle_v1 CUipcEventHandle;


/**
 * CUDA IPC mem handle
 */
typedef struct CUipcMemHandle_st {
    char reserved[CU_IPC_HANDLE_SIZE];
} CUipcMemHandle_v1;
typedef CUipcMemHandle_v1 CUipcMemHandle;


/**
 * CUDA Ipc Mem Flags
 */
typedef enum CUipcMem_flags_enum {
    CU_IPC_MEM_LAZY_ENABLE_PEER_ACCESS = 0x1 /**< Automatically enable peer access between remote devices as needed */
} CUipcMem_flags;

/**
 * CUDA Mem Attach Flags
 */
typedef enum CUmemAttach_flags_enum {
    CU_MEM_ATTACH_GLOBAL = 0x1, /**< Memory can be accessed by any stream on any device */
    CU_MEM_ATTACH_HOST   = 0x2, /**< Memory cannot be accessed by any stream on any device */
    CU_MEM_ATTACH_SINGLE = 0x4  /**< Memory can only be accessed by a single stream on the associated device */
} CUmemAttach_flags;

/**
 * Context creation flags
 */
typedef enum CUctx_flags_enum {
    CU_CTX_SCHED_AUTO          = 0x00, /**< Automatic scheduling */
    CU_CTX_SCHED_SPIN          = 0x01, /**< Set spin as default scheduling */
    CU_CTX_SCHED_YIELD         = 0x02, /**< Set yield as default scheduling */
    CU_CTX_SCHED_BLOCKING_SYNC = 0x04, /**< Set blocking synchronization as default scheduling */
    CU_CTX_BLOCKING_SYNC       = 0x04, /**< Set blocking synchronization as default scheduling
                                         *  \deprecated This flag was deprecated as of CUDA 4.0
                                         *  and was replaced with ::CU_CTX_SCHED_BLOCKING_SYNC. */
    CU_CTX_SCHED_MASK          = 0x07,
    CU_CTX_MAP_HOST            = 0x08, /**< \deprecated This flag was deprecated as of CUDA 11.0 
                                         *  and it no longer has any effect. All contexts 
                                         *  as of CUDA 3.2 behave as though the flag is enabled. */
    CU_CTX_LMEM_RESIZE_TO_MAX  = 0x10, /**< Keep local memory allocation after launch */
    CU_CTX_FLAGS_MASK          = 0x1f
} CUctx_flags;

/**
 * Stream creation flags
 */
typedef enum CUstream_flags_enum {
    CU_STREAM_DEFAULT             = 0x0, /**< Default stream flag */
    CU_STREAM_NON_BLOCKING        = 0x1  /**< Stream does not synchronize with stream 0 (the NULL stream) */
} CUstream_flags;

/**
 * Legacy stream handle
 *
 * Stream handle that can be passed as a CUstream to use an implicit stream
 * with legacy synchronization behavior.
 *
 * See details of the \link_sync_behavior
 */
#define CU_STREAM_LEGACY     ((CUstream)0x1)

/**
 * Per-thread stream handle
 *
 * Stream handle that can be passed as a CUstream to use an implicit stream
 * with per-thread synchronization behavior.
 *
 * See details of the \link_sync_behavior
 */
#define CU_STREAM_PER_THREAD ((CUstream)0x2)

/**
 * Event creation flags
 */
typedef enum CUevent_flags_enum {
    CU_EVENT_DEFAULT        = 0x0, /**< Default event flag */
    CU_EVENT_BLOCKING_SYNC  = 0x1, /**< Event uses blocking synchronization */
    CU_EVENT_DISABLE_TIMING = 0x2, /**< Event will not record timing data */
    CU_EVENT_INTERPROCESS   = 0x4  /**< Event is suitable for interprocess use. CU_EVENT_DISABLE_TIMING must be set */
} CUevent_flags;

/**
 * Event record flags
 */
typedef enum CUevent_record_flags_enum {
    CU_EVENT_RECORD_DEFAULT  = 0x0, /**< Default event record flag */
    CU_EVENT_RECORD_EXTERNAL = 0x1  /**< When using stream capture, create an event record node
                                      *  instead of the default behavior.  This flag is invalid
                                      *  when used outside of capture. */
} CUevent_record_flags;

/**
 * Event wait flags
 */
typedef enum CUevent_wait_flags_enum {
    CU_EVENT_WAIT_DEFAULT  = 0x0, /**< Default event wait flag */
    CU_EVENT_WAIT_EXTERNAL = 0x1  /**< When using stream capture, create an event wait node
                                    *  instead of the default behavior.  This flag is invalid
                                    *  when used outside of capture.*/
} CUevent_wait_flags;

/**
 * Flags for ::cuStreamWaitValue32 and ::cuStreamWaitValue64
 */
typedef enum CUstreamWaitValue_flags_enum {
    CU_STREAM_WAIT_VALUE_GEQ   = 0x0,   /**< Wait until (int32_t)(*addr - value) >= 0 (or int64_t for 64 bit
                                             values). Note this is a cyclic comparison which ignores wraparound.
                                             (Default behavior.) */
    CU_STREAM_WAIT_VALUE_EQ    = 0x1,   /**< Wait until *addr == value. */
    CU_STREAM_WAIT_VALUE_AND   = 0x2,   /**< Wait until (*addr & value) != 0. */
    CU_STREAM_WAIT_VALUE_NOR   = 0x3,   /**< Wait until ~(*addr | value) != 0. Support for this operation can be
                                             queried with ::cuDeviceGetAttribute() and
                                             ::CU_DEVICE_ATTRIBUTE_CAN_USE_STREAM_WAIT_VALUE_NOR.*/
    CU_STREAM_WAIT_VALUE_FLUSH = 1<<30  /**< Follow the wait operation with a flush of outstanding remote writes. This
                                             means that, if a remote write operation is guaranteed to have reached the
                                             device before the wait can be satisfied, that write is guaranteed to be
                                             visible to downstream device work. The device is permitted to reorder
                                             remote writes internally. For example, this flag would be required if
                                             two remote writes arrive in a defined order, the wait is satisfied by the
                                             second write, and downstream work needs to observe the first write.
                                             Support for this operation is restricted to selected platforms and can be
                                             queried with ::CU_DEVICE_ATTRIBUTE_CAN_FLUSH_REMOTE_WRITES.*/
} CUstreamWaitValue_flags;

/**
 * Flags for ::cuStreamWriteValue32
 */
typedef enum CUstreamWriteValue_flags_enum {
    CU_STREAM_WRITE_VALUE_DEFAULT           = 0x0, /**< Default behavior */
    CU_STREAM_WRITE_VALUE_NO_MEMORY_BARRIER = 0x1  /**< Permits the write to be reordered with writes which were issued
                                                        before it, as a performance optimization. Normally,
                                                        ::cuStreamWriteValue32 will provide a memory fence before the
                                                        write, which has similar semantics to
                                                        __threadfence_system() but is scoped to the stream
                                                        rather than a CUDA thread.
                                                        This flag is not supported in the v2 API. */
} CUstreamWriteValue_flags;

/**
 * Operations for ::cuStreamBatchMemOp
 */
typedef enum CUstreamBatchMemOpType_enum {
    CU_STREAM_MEM_OP_WAIT_VALUE_32  = 1,     /**< Represents a ::cuStreamWaitValue32 operation */
    CU_STREAM_MEM_OP_WRITE_VALUE_32 = 2,     /**< Represents a ::cuStreamWriteValue32 operation */
    CU_STREAM_MEM_OP_WAIT_VALUE_64  = 4,     /**< Represents a ::cuStreamWaitValue64 operation */
    CU_STREAM_MEM_OP_WRITE_VALUE_64 = 5,     /**< Represents a ::cuStreamWriteValue64 operation */
    CU_STREAM_MEM_OP_BARRIER = 6,            /**< Insert a memory barrier of the specified type */ 
    CU_STREAM_MEM_OP_FLUSH_REMOTE_WRITES = 3 /**< This has the same effect as ::CU_STREAM_WAIT_VALUE_FLUSH, but as a
                                                  standalone operation. */
} CUstreamBatchMemOpType;

/**
 * Flags for ::cuStreamMemoryBarrier
 */
typedef enum CUstreamMemoryBarrier_flags_enum {
    CU_STREAM_MEMORY_BARRIER_TYPE_SYS = 0x0, /**< System-wide memory barrier. */
    CU_STREAM_MEMORY_BARRIER_TYPE_GPU = 0x1 /**< Limit memory barrier scope to the GPU. */
} CUstreamMemoryBarrier_flags;


/**
 * Per-operation parameters for ::cuStreamBatchMemOp
 */
typedef union CUstreamBatchMemOpParams_union {
    CUstreamBatchMemOpType operation;
    struct CUstreamMemOpWaitValueParams_st {
        CUstreamBatchMemOpType operation;
        CUdeviceptr address;
        union {
            cuuint32_t value;
            cuuint64_t value64;
        };
        unsigned int flags;
        CUdeviceptr alias; /**< For driver internal use. Initial value is unimportant. */
    } waitValue;
    struct CUstreamMemOpWriteValueParams_st {
        CUstreamBatchMemOpType operation;
        CUdeviceptr address;
        union {
            cuuint32_t value;
            cuuint64_t value64;
        };
        unsigned int flags;
        CUdeviceptr alias; /**< For driver internal use. Initial value is unimportant. */
    } writeValue;
    struct CUstreamMemOpFlushRemoteWritesParams_st {
        CUstreamBatchMemOpType operation;
        unsigned int flags;
    } flushRemoteWrites;

    struct CUstreamMemOpMemoryBarrierParams_st { /**< Only supported in the _v2 API */
        CUstreamBatchMemOpType operation;
        unsigned int flags;
    } memoryBarrier;

    cuuint64_t pad[6];
} CUstreamBatchMemOpParams_v1;
typedef CUstreamBatchMemOpParams_v1 CUstreamBatchMemOpParams;

/**
 * Occupancy calculator flag
 */
typedef enum CUoccupancy_flags_enum {
    CU_OCCUPANCY_DEFAULT                  = 0x0, /**< Default behavior */
    CU_OCCUPANCY_DISABLE_CACHING_OVERRIDE = 0x1  /**< Assume global caching is enabled and cannot be automatically turned off */
} CUoccupancy_flags;

/**
 * Flags for ::cuStreamUpdateCaptureDependencies
 */
typedef enum CUstreamUpdateCaptureDependencies_flags_enum {
    CU_STREAM_ADD_CAPTURE_DEPENDENCIES = 0x0, /**< Add new nodes to the dependency set */
    CU_STREAM_SET_CAPTURE_DEPENDENCIES = 0x1  /**< Replace the dependency set with the new nodes */
} CUstreamUpdateCaptureDependencies_flags;

/**
 * Array formats
 */
typedef enum CUarray_format_enum {
    CU_AD_FORMAT_UNSIGNED_INT8            = 0x01, /**< Unsigned 8-bit integers */
    CU_AD_FORMAT_UNSIGNED_INT16           = 0x02, /**< Unsigned 16-bit integers */
    CU_AD_FORMAT_UNSIGNED_INT32           = 0x03, /**< Unsigned 32-bit integers */
    CU_AD_FORMAT_SIGNED_INT8              = 0x08, /**< Signed 8-bit integers */
    CU_AD_FORMAT_SIGNED_INT16             = 0x09, /**< Signed 16-bit integers */
    CU_AD_FORMAT_SIGNED_INT32             = 0x0a, /**< Signed 32-bit integers */
    CU_AD_FORMAT_HALF                     = 0x10, /**< 16-bit floating point */
    CU_AD_FORMAT_FLOAT                    = 0x20, /**< 32-bit floating point */
    CU_AD_FORMAT_NV12                     = 0xb0, /**< 8-bit YUV planar format, with 4:2:0 sampling */
    CU_AD_FORMAT_UNORM_INT8X1             = 0xc0, /**< 1 channel unsigned 8-bit normalized integer */
    CU_AD_FORMAT_UNORM_INT8X2             = 0xc1, /**< 2 channel unsigned 8-bit normalized integer */
    CU_AD_FORMAT_UNORM_INT8X4             = 0xc2, /**< 4 channel unsigned 8-bit normalized integer */
    CU_AD_FORMAT_UNORM_INT16X1            = 0xc3, /**< 1 channel unsigned 16-bit normalized integer */
    CU_AD_FORMAT_UNORM_INT16X2            = 0xc4, /**< 2 channel unsigned 16-bit normalized integer */
    CU_AD_FORMAT_UNORM_INT16X4            = 0xc5, /**< 4 channel unsigned 16-bit normalized integer */
    CU_AD_FORMAT_SNORM_INT8X1             = 0xc6, /**< 1 channel signed 8-bit normalized integer */
    CU_AD_FORMAT_SNORM_INT8X2             = 0xc7, /**< 2 channel signed 8-bit normalized integer */
    CU_AD_FORMAT_SNORM_INT8X4             = 0xc8, /**< 4 channel signed 8-bit normalized integer */
    CU_AD_FORMAT_SNORM_INT16X1            = 0xc9, /**< 1 channel signed 16-bit normalized integer */
    CU_AD_FORMAT_SNORM_INT16X2            = 0xca, /**< 2 channel signed 16-bit normalized integer */
    CU_AD_FORMAT_SNORM_INT16X4            = 0xcb, /**< 4 channel signed 16-bit normalized integer */
    CU_AD_FORMAT_BC1_UNORM                = 0x91, /**< 4 channel unsigned normalized block-compressed (BC1 compression) format */
    CU_AD_FORMAT_BC1_UNORM_SRGB           = 0x92, /**< 4 channel unsigned normalized block-compressed (BC1 compression) format with sRGB encoding*/
    CU_AD_FORMAT_BC2_UNORM                = 0x93, /**< 4 channel unsigned normalized block-compressed (BC2 compression) format */
    CU_AD_FORMAT_BC2_UNORM_SRGB           = 0x94, /**< 4 channel unsigned normalized block-compressed (BC2 compression) format with sRGB encoding*/
    CU_AD_FORMAT_BC3_UNORM                = 0x95, /**< 4 channel unsigned normalized block-compressed (BC3 compression) format */
    CU_AD_FORMAT_BC3_UNORM_SRGB           = 0x96, /**< 4 channel unsigned normalized block-compressed (BC3 compression) format with sRGB encoding*/
    CU_AD_FORMAT_BC4_UNORM                = 0x97, /**< 1 channel unsigned normalized block-compressed (BC4 compression) format */
    CU_AD_FORMAT_BC4_SNORM                = 0x98, /**< 1 channel signed normalized block-compressed (BC4 compression) format */
    CU_AD_FORMAT_BC5_UNORM                = 0x99, /**< 2 channel unsigned normalized block-compressed (BC5 compression) format */
    CU_AD_FORMAT_BC5_SNORM                = 0x9a, /**< 2 channel signed normalized block-compressed (BC5 compression) format */
    CU_AD_FORMAT_BC6H_UF16                = 0x9b, /**< 3 channel unsigned half-float block-compressed (BC6H compression) format */
    CU_AD_FORMAT_BC6H_SF16                = 0x9c, /**< 3 channel signed half-float block-compressed (BC6H compression) format */
    CU_AD_FORMAT_BC7_UNORM                = 0x9d, /**< 4 channel unsigned normalized block-compressed (BC7 compression) format */
    CU_AD_FORMAT_BC7_UNORM_SRGB           = 0x9e, /**< 4 channel unsigned normalized block-compressed (BC7 compression) format with sRGB encoding */
    CU_AD_FORMAT_P010                     = 0x9f, /**< 10-bit YUV planar format, with 4:2:0 sampling */
    CU_AD_FORMAT_P016                     = 0xa1, /**< 16-bit YUV planar format, with 4:2:0 sampling */
    CU_AD_FORMAT_NV16                     = 0xa2, /**< 8-bit YUV planar format, with 4:2:2 sampling */
    CU_AD_FORMAT_P210                     = 0xa3, /**< 10-bit YUV planar format, with 4:2:2 sampling */
    CU_AD_FORMAT_P216                     = 0xa4, /**< 16-bit YUV planar format, with 4:2:2 sampling */
    CU_AD_FORMAT_YUY2                     = 0xa5, /**< 2 channel, 8-bit YUV packed planar format, with 4:2:2 sampling */
    CU_AD_FORMAT_Y210                     = 0xa6, /**< 2 channel, 10-bit YUV packed planar format, with 4:2:2 sampling */
    CU_AD_FORMAT_Y216                     = 0xa7, /**< 2 channel, 16-bit YUV packed planar format, with 4:2:2 sampling */
    CU_AD_FORMAT_AYUV                     = 0xa8, /**< 4 channel, 8-bit YUV packed planar format, with 4:4:4 sampling */
    CU_AD_FORMAT_Y410                     = 0xa9, /**< 10-bit YUV packed planar format, with 4:4:4 sampling */
    CU_AD_FORMAT_Y416                     = 0xb1, /**< 4 channel, 12-bit YUV packed planar format, with 4:4:4 sampling */
    CU_AD_FORMAT_Y444_PLANAR8             = 0xb2, /**< 3 channel 8-bit YUV planar format, with 4:4:4 sampling */
    CU_AD_FORMAT_Y444_PLANAR10            = 0xb3, /**< 3 channel 10-bit YUV planar format, with 4:4:4 sampling */
    CU_AD_FORMAT_YUV444_8bit_SemiPlanar   = 0xb4, /**< 3 channel 8-bit YUV semi-planar format, with 4:4:4 sampling */
    CU_AD_FORMAT_YUV444_16bit_SemiPlanar  = 0xb5, /**< 3 channel 16-bit YUV semi-planar format, with 4:4:4 sampling */
    CU_AD_FORMAT_UNORM_INT_101010_2       = 0x50, /**< 4 channel unorm R10G10B10A2 RGB format */
    CU_AD_FORMAT_MAX                      = 0x7FFFFFFF
} CUarray_format;

/**
 * Texture reference addressing modes
 */
typedef enum CUaddress_mode_enum {
    CU_TR_ADDRESS_MODE_WRAP   = 0, /**< Wrapping address mode */
    CU_TR_ADDRESS_MODE_CLAMP  = 1, /**< Clamp to edge address mode */
    CU_TR_ADDRESS_MODE_MIRROR = 2, /**< Mirror address mode */
    CU_TR_ADDRESS_MODE_BORDER = 3  /**< Border address mode */
} CUaddress_mode;

/**
 * Texture reference filtering modes
 */
typedef enum CUfilter_mode_enum {
    CU_TR_FILTER_MODE_POINT  = 0, /**< Point filter mode */
    CU_TR_FILTER_MODE_LINEAR = 1  /**< Linear filter mode */
} CUfilter_mode;

/**
 * Device properties
 */
typedef enum CUdevice_attribute_enum {
    CU_DEVICE_ATTRIBUTE_MAX_THREADS_PER_BLOCK = 1,                          /**< Maximum number of threads per block */
    CU_DEVICE_ATTRIBUTE_MAX_BLOCK_DIM_X = 2,                                /**< Maximum block dimension X */
    CU_DEVICE_ATTRIBUTE_MAX_BLOCK_DIM_Y = 3,                                /**< Maximum block dimension Y */
    CU_DEVICE_ATTRIBUTE_MAX_BLOCK_DIM_Z = 4,                                /**< Maximum block dimension Z */
    CU_DEVICE_ATTRIBUTE_MAX_GRID_DIM_X = 5,                                 /**< Maximum grid dimension X */
    CU_DEVICE_ATTRIBUTE_MAX_GRID_DIM_Y = 6,                                 /**< Maximum grid dimension Y */
    CU_DEVICE_ATTRIBUTE_MAX_GRID_DIM_Z = 7,                                 /**< Maximum grid dimension Z */
    CU_DEVICE_ATTRIBUTE_MAX_SHARED_MEMORY_PER_BLOCK = 8,                    /**< Maximum shared memory available per block in bytes */
    CU_DEVICE_ATTRIBUTE_SHARED_MEMORY_PER_BLOCK = 8,                        /**< Deprecated, use CU_DEVICE_ATTRIBUTE_MAX_SHARED_MEMORY_PER_BLOCK */
    CU_DEVICE_ATTRIBUTE_TOTAL_CONSTANT_MEMORY = 9,                          /**< Memory available on device for __constant__ variables in a CUDA C kernel in bytes */
    CU_DEVICE_ATTRIBUTE_WARP_SIZE = 10,                                     /**< Warp size in threads */
    CU_DEVICE_ATTRIBUTE_MAX_PITCH = 11,                                     /**< Maximum pitch in bytes allowed by memory copies */
    CU_DEVICE_ATTRIBUTE_MAX_REGISTERS_PER_BLOCK = 12,                       /**< Maximum number of 32-bit registers available per block */
    CU_DEVICE_ATTRIBUTE_REGISTERS_PER_BLOCK = 12,                           /**< Deprecated, use CU_DEVICE_ATTRIBUTE_MAX_REGISTERS_PER_BLOCK */
    CU_DEVICE_ATTRIBUTE_CLOCK_RATE = 13,                                    /**< Typical clock frequency in kilohertz */
    CU_DEVICE_ATTRIBUTE_TEXTURE_ALIGNMENT = 14,                             /**< Alignment requirement for textures */
    CU_DEVICE_ATTRIBUTE_GPU_OVERLAP = 15,                                   /**< Device can possibly copy memory and execute a kernel concurrently. Deprecated. Use instead CU_DEVICE_ATTRIBUTE_ASYNC_ENGINE_COUNT. */
    CU_DEVICE_ATTRIBUTE_MULTIPROCESSOR_COUNT = 16,                          /**< Number of multiprocessors on device */
    CU_DEVICE_ATTRIBUTE_KERNEL_EXEC_TIMEOUT = 17,                           /**< Specifies whether there is a run time limit on kernels */
    CU_DEVICE_ATTRIBUTE_INTEGRATED = 18,                                    /**< Device is integrated with host memory */
    CU_DEVICE_ATTRIBUTE_CAN_MAP_HOST_MEMORY = 19,                           /**< Device can map host memory into CUDA address space */
    CU_DEVICE_ATTRIBUTE_COMPUTE_MODE = 20,                                  /**< Compute mode (See ::CUcomputemode for details) */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE1D_WIDTH = 21,                       /**< Maximum 1D texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_WIDTH = 22,                       /**< Maximum 2D texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_HEIGHT = 23,                      /**< Maximum 2D texture height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE3D_WIDTH = 24,                       /**< Maximum 3D texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE3D_HEIGHT = 25,                      /**< Maximum 3D texture height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE3D_DEPTH = 26,                       /**< Maximum 3D texture depth */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LAYERED_WIDTH = 27,               /**< Maximum 2D layered texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LAYERED_HEIGHT = 28,              /**< Maximum 2D layered texture height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LAYERED_LAYERS = 29,              /**< Maximum layers in a 2D layered texture */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_ARRAY_WIDTH = 27,                 /**< Deprecated, use CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LAYERED_WIDTH */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_ARRAY_HEIGHT = 28,                /**< Deprecated, use CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LAYERED_HEIGHT */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_ARRAY_NUMSLICES = 29,             /**< Deprecated, use CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LAYERED_LAYERS */
    CU_DEVICE_ATTRIBUTE_SURFACE_ALIGNMENT = 30,                             /**< Alignment requirement for surfaces */
    CU_DEVICE_ATTRIBUTE_CONCURRENT_KERNELS = 31,                            /**< Device can possibly execute multiple kernels concurrently */
    CU_DEVICE_ATTRIBUTE_ECC_ENABLED = 32,                                   /**< Device has ECC support enabled */
    CU_DEVICE_ATTRIBUTE_PCI_BUS_ID = 33,                                    /**< PCI bus ID of the device */
    CU_DEVICE_ATTRIBUTE_PCI_DEVICE_ID = 34,                                 /**< PCI device ID of the device */
    CU_DEVICE_ATTRIBUTE_TCC_DRIVER = 35,                                    /**< Device is using TCC driver model */
    CU_DEVICE_ATTRIBUTE_MEMORY_CLOCK_RATE = 36,                             /**< Peak memory clock frequency in kilohertz */
    CU_DEVICE_ATTRIBUTE_GLOBAL_MEMORY_BUS_WIDTH = 37,                       /**< Global memory bus width in bits */
    CU_DEVICE_ATTRIBUTE_L2_CACHE_SIZE = 38,                                 /**< Size of L2 cache in bytes */
    CU_DEVICE_ATTRIBUTE_MAX_THREADS_PER_MULTIPROCESSOR = 39,                /**< Maximum resident threads per multiprocessor */
    CU_DEVICE_ATTRIBUTE_ASYNC_ENGINE_COUNT = 40,                            /**< Number of asynchronous engines */
    CU_DEVICE_ATTRIBUTE_UNIFIED_ADDRESSING = 41,                            /**< Device shares a unified address space with the host */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE1D_LAYERED_WIDTH = 42,               /**< Maximum 1D layered texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE1D_LAYERED_LAYERS = 43,              /**< Maximum layers in a 1D layered texture */
    CU_DEVICE_ATTRIBUTE_CAN_TEX2D_GATHER = 44,                              /**< Deprecated, do not use. */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_GATHER_WIDTH = 45,                /**< Maximum 2D texture width if CUDA_ARRAY3D_TEXTURE_GATHER is set */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_GATHER_HEIGHT = 46,               /**< Maximum 2D texture height if CUDA_ARRAY3D_TEXTURE_GATHER is set */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE3D_WIDTH_ALTERNATE = 47,             /**< Alternate maximum 3D texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE3D_HEIGHT_ALTERNATE = 48,            /**< Alternate maximum 3D texture height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE3D_DEPTH_ALTERNATE = 49,             /**< Alternate maximum 3D texture depth */
    CU_DEVICE_ATTRIBUTE_PCI_DOMAIN_ID = 50,                                 /**< PCI domain ID of the device */
    CU_DEVICE_ATTRIBUTE_TEXTURE_PITCH_ALIGNMENT = 51,                       /**< Pitch alignment requirement for textures */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURECUBEMAP_WIDTH = 52,                  /**< Maximum cubemap texture width/height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURECUBEMAP_LAYERED_WIDTH = 53,          /**< Maximum cubemap layered texture width/height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURECUBEMAP_LAYERED_LAYERS = 54,         /**< Maximum layers in a cubemap layered texture */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE1D_WIDTH = 55,                       /**< Maximum 1D surface width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE2D_WIDTH = 56,                       /**< Maximum 2D surface width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE2D_HEIGHT = 57,                      /**< Maximum 2D surface height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE3D_WIDTH = 58,                       /**< Maximum 3D surface width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE3D_HEIGHT = 59,                      /**< Maximum 3D surface height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE3D_DEPTH = 60,                       /**< Maximum 3D surface depth */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE1D_LAYERED_WIDTH = 61,               /**< Maximum 1D layered surface width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE1D_LAYERED_LAYERS = 62,              /**< Maximum layers in a 1D layered surface */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE2D_LAYERED_WIDTH = 63,               /**< Maximum 2D layered surface width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE2D_LAYERED_HEIGHT = 64,              /**< Maximum 2D layered surface height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACE2D_LAYERED_LAYERS = 65,              /**< Maximum layers in a 2D layered surface */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACECUBEMAP_WIDTH = 66,                  /**< Maximum cubemap surface width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACECUBEMAP_LAYERED_WIDTH = 67,          /**< Maximum cubemap layered surface width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_SURFACECUBEMAP_LAYERED_LAYERS = 68,         /**< Maximum layers in a cubemap layered surface */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE1D_LINEAR_WIDTH = 69,                /**< Deprecated, do not use. Use cudaDeviceGetTexture1DLinearMaxWidth() or cuDeviceGetTexture1DLinearMaxWidth() instead. */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LINEAR_WIDTH = 70,                /**< Maximum 2D linear texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LINEAR_HEIGHT = 71,               /**< Maximum 2D linear texture height */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_LINEAR_PITCH = 72,                /**< Maximum 2D linear texture pitch in bytes */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_MIPMAPPED_WIDTH = 73,             /**< Maximum mipmapped 2D texture width */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE2D_MIPMAPPED_HEIGHT = 74,            /**< Maximum mipmapped 2D texture height */
    CU_DEVICE_ATTRIBUTE_COMPUTE_CAPABILITY_MAJOR = 75,                      /**< Major compute capability version number */
    CU_DEVICE_ATTRIBUTE_COMPUTE_CAPABILITY_MINOR = 76,                      /**< Minor compute capability version number */
    CU_DEVICE_ATTRIBUTE_MAXIMUM_TEXTURE1D_MIPMAPPED_WIDTH = 77,             /**< Maximum mipmapped 1D texture width */
    CU_DEVICE_ATTRIBUTE_STREAM_PRIORITIES_SUPPORTED = 78,                   /**< Device supports stream priorities */
    CU_DEVICE_ATTRIBUTE_GLOBAL_L1_CACHE_SUPPORTED = 79,                     /**< Device supports caching globals in L1 */
    CU_DEVICE_ATTRIBUTE_LOCAL_L1_CACHE_SUPPORTED = 80,                      /**< Device supports caching locals in L1 */
    CU_DEVICE_ATTRIBUTE_MAX_SHARED_MEMORY_PER_MULTIPROCESSOR = 81,          /**< Maximum shared memory available per multiprocessor in bytes */
    CU_DEVICE_ATTRIBUTE_MAX_REGISTERS_PER_MULTIPROCESSOR = 82,              /**< Maximum number of 32-bit registers available per multiprocessor */
    CU_DEVICE_ATTRIBUTE_MANAGED_MEMORY = 83,                                /**< Device can allocate managed memory on this system */
    CU_DEVICE_ATTRIBUTE_MULTI_GPU_BOARD = 84,                               /**< Device is on a multi-GPU board */
    CU_DEVICE_ATTRIBUTE_MULTI_GPU_BOARD_GROUP_ID = 85,                      /**< Unique id for a group of devices on the same multi-GPU board */
    CU_DEVICE_ATTRIBUTE_HOST_NATIVE_ATOMIC_SUPPORTED = 86,                  /**< Link between the device and the host supports native atomic operations (this is a placeholder attribute, and is not supported on any current hardware)*/
    CU_DEVICE_ATTRIBUTE_SINGLE_TO_DOUBLE_PRECISION_PERF_RATIO = 87,         /**< Ratio of single precision performance (in floating-point operations per second) to double precision performance */
    CU_DEVICE_ATTRIBUTE_PAGEABLE_MEMORY_ACCESS = 88,                        /**< Device supports coherently accessing pageable memory without calling cudaHostRegister on it */
    CU_DEVICE_ATTRIBUTE_CONCURRENT_MANAGED_ACCESS = 89,                     /**< Device can coherently access managed memory concurrently with the CPU */
    CU_DEVICE_ATTRIBUTE_COMPUTE_PREEMPTION_SUPPORTED = 90,                  /**< Device supports compute preemption. */
    CU_DEVICE_ATTRIBUTE_CAN_USE_HOST_POINTER_FOR_REGISTERED_MEM = 91,       /**< Device can access host registered memory at the same virtual address as the CPU */
    CU_DEVICE_ATTRIBUTE_CAN_USE_STREAM_MEM_OPS_V1 = 92,                     /**< Deprecated, along with v1 MemOps API, ::cuStreamBatchMemOp and related APIs are supported. */
    CU_DEVICE_ATTRIBUTE_CAN_USE_64_BIT_STREAM_MEM_OPS_V1 = 93,              /**< Deprecated, along with v1 MemOps API, 64-bit operations are supported in ::cuStreamBatchMemOp and related APIs. */
    CU_DEVICE_ATTRIBUTE_CAN_USE_STREAM_WAIT_VALUE_NOR_V1 = 94,              /**< Deprecated, along with v1 MemOps API, ::CU_STREAM_WAIT_VALUE_NOR is supported. */
    CU_DEVICE_ATTRIBUTE_COOPERATIVE_LAUNCH = 95,                            /**< Device supports launching cooperative kernels via ::cuLaunchCooperativeKernel */
    CU_DEVICE_ATTRIBUTE_COOPERATIVE_MULTI_DEVICE_LAUNCH = 96,               /**< Deprecated, ::cuLaunchCooperativeKernelMultiDevice is deprecated. */
    CU_DEVICE_ATTRIBUTE_MAX_SHARED_MEMORY_PER_BLOCK_OPTIN = 97,             /**< Maximum optin shared memory per block */
    CU_DEVICE_ATTRIBUTE_CAN_FLUSH_REMOTE_WRITES = 98,                       /**< The ::CU_STREAM_WAIT_VALUE_FLUSH flag and the ::CU_STREAM_MEM_OP_FLUSH_REMOTE_WRITES MemOp are supported on the device. See \ref CUDA_MEMOP for additional details. */
    CU_DEVICE_ATTRIBUTE_HOST_REGISTER_SUPPORTED = 99,                       /**< Device supports host memory registration via ::cudaHostRegister. */
    CU_DEVICE_ATTRIBUTE_PAGEABLE_MEMORY_ACCESS_USES_HOST_PAGE_TABLES = 100, /**< Device accesses pageable memory via the host's page tables. */
    CU_DEVICE_ATTRIBUTE_DIRECT_MANAGED_MEM_ACCESS_FROM_HOST = 101,          /**< The host can directly access managed memory on the device without migration. */
    CU_DEVICE_ATTRIBUTE_VIRTUAL_ADDRESS_MANAGEMENT_SUPPORTED = 102,         /**< Deprecated, Use CU_DEVICE_ATTRIBUTE_VIRTUAL_MEMORY_MANAGEMENT_SUPPORTED*/
    CU_DEVICE_ATTRIBUTE_VIRTUAL_MEMORY_MANAGEMENT_SUPPORTED = 102,         /**< Device supports virtual memory management APIs like ::cuMemAddressReserve, ::cuMemCreate, ::cuMemMap and related APIs */
    CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR_SUPPORTED = 103,  /**< Device supports exporting memory to a posix file descriptor with ::cuMemExportToShareableHandle, if requested via ::cuMemCreate */
    CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_WIN32_HANDLE_SUPPORTED = 104,           /**< Device supports exporting memory to a Win32 NT handle with ::cuMemExportToShareableHandle, if requested via ::cuMemCreate */
    CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_WIN32_KMT_HANDLE_SUPPORTED = 105,       /**< Device supports exporting memory to a Win32 KMT handle with ::cuMemExportToShareableHandle, if requested via ::cuMemCreate */
    CU_DEVICE_ATTRIBUTE_MAX_BLOCKS_PER_MULTIPROCESSOR = 106,                /**< Maximum number of blocks per multiprocessor */
    CU_DEVICE_ATTRIBUTE_GENERIC_COMPRESSION_SUPPORTED = 107,                /**< Device supports compression of memory */
    CU_DEVICE_ATTRIBUTE_MAX_PERSISTING_L2_CACHE_SIZE = 108,                 /**< Maximum L2 persisting lines capacity setting in bytes. */
    CU_DEVICE_ATTRIBUTE_MAX_ACCESS_POLICY_WINDOW_SIZE = 109,                /**< Maximum value of CUaccessPolicyWindow::num_bytes. */
    CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WITH_CUDA_VMM_SUPPORTED = 110,      /**< Device supports specifying the GPUDirect RDMA flag with ::cuMemCreate */
    CU_DEVICE_ATTRIBUTE_RESERVED_SHARED_MEMORY_PER_BLOCK = 111,             /**< Shared memory reserved by CUDA driver per block in bytes */
    CU_DEVICE_ATTRIBUTE_SPARSE_CUDA_ARRAY_SUPPORTED = 112,                  /**< Device supports sparse CUDA arrays and sparse CUDA mipmapped arrays */
    CU_DEVICE_ATTRIBUTE_READ_ONLY_HOST_REGISTER_SUPPORTED = 113,            /**< Device supports using the ::cuMemHostRegister flag ::CU_MEMHOSTERGISTER_READ_ONLY to register memory that must be mapped as read-only to the GPU */
    CU_DEVICE_ATTRIBUTE_TIMELINE_SEMAPHORE_INTEROP_SUPPORTED = 114,         /**< External timeline semaphore interop is supported on the device */
    CU_DEVICE_ATTRIBUTE_MEMORY_POOLS_SUPPORTED = 115,                       /**< Device supports using the ::cuMemAllocAsync and ::cuMemPool family of APIs */
    CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED = 116,                    /**< Device supports GPUDirect RDMA APIs, like nvidia_p2p_get_pages (see https://docs.nvidia.com/cuda/gpudirect-rdma for more information) */
    CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_FLUSH_WRITES_OPTIONS = 117,         /**< The returned attribute shall be interpreted as a bitmask, where the individual bits are described by the ::CUflushGPUDirectRDMAWritesOptions enum */
    CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WRITES_ORDERING = 118,              /**< GPUDirect RDMA writes to the device do not need to be flushed for consumers within the scope indicated by the returned attribute. See ::CUGPUDirectRDMAWritesOrdering for the numerical values returned here. */
    CU_DEVICE_ATTRIBUTE_MEMPOOL_SUPPORTED_HANDLE_TYPES = 119,               /**< Handle types supported with mempool based IPC */
    CU_DEVICE_ATTRIBUTE_CLUSTER_LAUNCH = 120,                               /**< Indicates device supports cluster launch */
    CU_DEVICE_ATTRIBUTE_DEFERRED_MAPPING_CUDA_ARRAY_SUPPORTED = 121,        /**< Device supports deferred mapping CUDA arrays and CUDA mipmapped arrays */
    CU_DEVICE_ATTRIBUTE_CAN_USE_64_BIT_STREAM_MEM_OPS = 122,                /**< 64-bit operations are supported in ::cuStreamBatchMemOp and related MemOp APIs. */
    CU_DEVICE_ATTRIBUTE_CAN_USE_STREAM_WAIT_VALUE_NOR = 123,                /**< ::CU_STREAM_WAIT_VALUE_NOR is supported by MemOp APIs. */
    CU_DEVICE_ATTRIBUTE_DMA_BUF_SUPPORTED = 124,                            /**< Device supports buffer sharing with dma_buf mechanism. */
    CU_DEVICE_ATTRIBUTE_IPC_EVENT_SUPPORTED = 125,                          /**< Device supports IPC Events. */
    CU_DEVICE_ATTRIBUTE_MEM_SYNC_DOMAIN_COUNT = 126,                        /**< Number of memory domains the device supports. */
    CU_DEVICE_ATTRIBUTE_TENSOR_MAP_ACCESS_SUPPORTED = 127,                  /**< Device supports accessing memory using Tensor Map. */
    CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_FABRIC_SUPPORTED = 128,                 /**< Device supports exporting memory to a fabric handle with cuMemExportToShareableHandle() or requested with cuMemCreate() */
    CU_DEVICE_ATTRIBUTE_UNIFIED_FUNCTION_POINTERS = 129,                    /**< Device supports unified function pointers. */
    CU_DEVICE_ATTRIBUTE_NUMA_CONFIG = 130,                                  /**< NUMA configuration of a device: value is of type ::CUdeviceNumaConfig enum */
    CU_DEVICE_ATTRIBUTE_NUMA_ID = 131,                                      /**< NUMA node ID of the GPU memory */
    CU_DEVICE_ATTRIBUTE_MULTICAST_SUPPORTED = 132,                          /**< Device supports switch multicast and reduction operations. */
    CU_DEVICE_ATTRIBUTE_MPS_ENABLED = 133,                                  /**< Indicates if contexts created on this device will be shared via MPS */
    CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID = 134,                                 /**< NUMA ID of the host node closest to the device. Returns -1 when system does not support NUMA. */
    CU_DEVICE_ATTRIBUTE_D3D12_CIG_SUPPORTED = 135,                          /**< Device supports CIG with D3D12. */
    CU_DEVICE_ATTRIBUTE_MEM_DECOMPRESS_ALGORITHM_MASK = 136,                /**< The returned valued shall be interpreted as a bitmask, where the individual bits are described by the ::CUmemDecompressAlgorithm enum. */
    CU_DEVICE_ATTRIBUTE_MEM_DECOMPRESS_MAXIMUM_LENGTH = 137,                /**< The returned valued is the maximum length in bytes of a single decompress operation that is allowed. */
    CU_DEVICE_ATTRIBUTE_GPU_PCI_DEVICE_ID    = 139, /**< The combined 16-bit PCI device ID and 16-bit PCI vendor ID. */
    CU_DEVICE_ATTRIBUTE_GPU_PCI_SUBSYSTEM_ID = 140, /**< The combined 16-bit PCI subsystem ID and 16-bit PCI subsystem vendor ID. */
    CU_DEVICE_ATTRIBUTE_HOST_NUMA_MULTINODE_IPC_SUPPORTED = 143,             /**< Device supports HOST_NUMA location IPC between nodes in a multi-node system. */
    CU_DEVICE_ATTRIBUTE_MAX
} CUdevice_attribute;

/**
 * Legacy device properties
 */
typedef struct CUdevprop_st {
    int maxThreadsPerBlock;     /**< Maximum number of threads per block */
    int maxThreadsDim[3];       /**< Maximum size of each dimension of a block */
    int maxGridSize[3];         /**< Maximum size of each dimension of a grid */
    int sharedMemPerBlock;      /**< Shared memory available per block in bytes */
    int totalConstantMemory;    /**< Constant memory available on device in bytes */
    int SIMDWidth;              /**< Warp size in threads */
    int memPitch;               /**< Maximum pitch in bytes allowed by memory copies */
    int regsPerBlock;           /**< 32-bit registers available per block */
    int clockRate;              /**< Clock frequency in kilohertz */
    int textureAlign;           /**< Alignment requirement for textures */
} CUdevprop_v1;
typedef CUdevprop_v1 CUdevprop;

/**
 * Pointer information
 */
typedef enum CUpointer_attribute_enum {
    CU_POINTER_ATTRIBUTE_CONTEXT = 1,                     /**< The ::CUcontext on which a pointer was allocated or registered */
    CU_POINTER_ATTRIBUTE_MEMORY_TYPE = 2,                 /**< The ::CUmemorytype describing the physical location of a pointer */
    CU_POINTER_ATTRIBUTE_DEVICE_POINTER = 3,              /**< The address at which a pointer's memory may be accessed on the device */
    CU_POINTER_ATTRIBUTE_HOST_POINTER = 4,                /**< The address at which a pointer's memory may be accessed on the host */
    CU_POINTER_ATTRIBUTE_P2P_TOKENS = 5,                  /**< A pair of tokens for use with the nv-p2p.h Linux kernel interface */
    CU_POINTER_ATTRIBUTE_SYNC_MEMOPS = 6,                 /**< Synchronize every synchronous memory operation initiated on this region */
    CU_POINTER_ATTRIBUTE_BUFFER_ID = 7,                   /**< A process-wide unique ID for an allocated memory region*/
    CU_POINTER_ATTRIBUTE_IS_MANAGED = 8,                  /**< Indicates if the pointer points to managed memory */
    CU_POINTER_ATTRIBUTE_DEVICE_ORDINAL = 9,              /**< A device ordinal of a device on which a pointer was allocated or registered */
    CU_POINTER_ATTRIBUTE_IS_LEGACY_CUDA_IPC_CAPABLE = 10, /**< 1 if this pointer maps to an allocation that is suitable for ::cudaIpcGetMemHandle, 0 otherwise **/
    CU_POINTER_ATTRIBUTE_RANGE_START_ADDR = 11,           /**< Starting address for this requested pointer */
    CU_POINTER_ATTRIBUTE_RANGE_SIZE = 12,                 /**< Size of the address range for this requested pointer */
    CU_POINTER_ATTRIBUTE_MAPPED = 13,                     /**< 1 if this pointer is in a valid address range that is mapped to a backing allocation, 0 otherwise **/
    CU_POINTER_ATTRIBUTE_ALLOWED_HANDLE_TYPES = 14,       /**< Bitmask of allowed ::CUmemAllocationHandleType for this allocation **/
    CU_POINTER_ATTRIBUTE_IS_GPU_DIRECT_RDMA_CAPABLE = 15, /**< 1 if the memory this pointer is referencing can be used with the GPUDirect RDMA API **/
    CU_POINTER_ATTRIBUTE_ACCESS_FLAGS = 16,               /**< Returns the access flags the device associated with the current context has on the corresponding memory referenced by the pointer given */
    CU_POINTER_ATTRIBUTE_MEMPOOL_HANDLE = 17              /**< Returns the mempool handle for the allocation if it was allocated from a mempool. Otherwise returns NULL. **/

    ,
    CU_POINTER_ATTRIBUTE_MAPPING_SIZE = 18,               /**< Size of the actual underlying mapping that the pointer belongs to **/
    CU_POINTER_ATTRIBUTE_MAPPING_BASE_ADDR = 19,          /**< The start address of the mapping that the pointer belongs to **/
    CU_POINTER_ATTRIBUTE_MEMORY_BLOCK_ID = 20             /**< A process-wide unique id corresponding to the physical allocation the pointer belongs to **/

} CUpointer_attribute;

/**
 * Function properties
 */
typedef enum CUfunction_attribute_enum {
    /**
     * The maximum number of threads per block, beyond which a launch of the
     * function would fail. This number depends on both the function and the
     * device on which the function is currently loaded.
     */
    CU_FUNC_ATTRIBUTE_MAX_THREADS_PER_BLOCK = 0,

    /**
     * The size in bytes of statically-allocated shared memory required by
     * this function. This does not include dynamically-allocated shared
     * memory requested by the user at runtime.
     */
    CU_FUNC_ATTRIBUTE_SHARED_SIZE_BYTES = 1,

    /**
     * The size in bytes of user-allocated constant memory required by this
     * function.
     */
    CU_FUNC_ATTRIBUTE_CONST_SIZE_BYTES = 2,

    /**
     * The size in bytes of local memory used by each thread of this function.
     */
    CU_FUNC_ATTRIBUTE_LOCAL_SIZE_BYTES = 3,

    /**
     * The number of registers used by each thread of this function.
     */
    CU_FUNC_ATTRIBUTE_NUM_REGS = 4,

    /**
     * The PTX virtual architecture version for which the function was
     * compiled. This value is the major PTX version * 10 + the minor PTX
     * version, so a PTX version 1.3 function would return the value 13.
     * Note that this may return the undefined value of 0 for cubins
     * compiled prior to CUDA 3.0.
     */
    CU_FUNC_ATTRIBUTE_PTX_VERSION = 5,

    /**
     * The binary architecture version for which the function was compiled.
     * This value is the major binary version * 10 + the minor binary version,
     * so a binary version 1.3 function would return the value 13. Note that
     * this will return a value of 10 for legacy cubins that do not have a
     * properly-encoded binary architecture version.
     */
    CU_FUNC_ATTRIBUTE_BINARY_VERSION = 6,

    /**
     * The attribute to indicate whether the function has been compiled with
     * user specified option "-Xptxas --dlcm=ca" set .
     */
    CU_FUNC_ATTRIBUTE_CACHE_MODE_CA = 7,

    /**
     * The maximum size in bytes of dynamically-allocated shared memory that can be used by
     * this function. If the user-specified dynamic shared memory size is larger than this
     * value, the launch will fail.
     * See ::cuFuncSetAttribute
     */
    CU_FUNC_ATTRIBUTE_MAX_DYNAMIC_SHARED_SIZE_BYTES = 8,

    /**
     * On devices where the L1 cache and shared memory use the same hardware resources, 
     * this sets the shared memory carveout preference, in percent of the total shared memory.
     * Refer to ::CU_DEVICE_ATTRIBUTE_MAX_SHARED_MEMORY_PER_MULTIPROCESSOR.
     * This is only a hint, and the driver can choose a different ratio if required to execute the function.
     * See ::cuFuncSetAttribute
     */
    CU_FUNC_ATTRIBUTE_PREFERRED_SHARED_MEMORY_CARVEOUT = 9,

    CU_FUNC_ATTRIBUTE_MAX
} CUfunction_attribute;

/**
 * Function cache configurations
 */
typedef enum CUfunc_cache_enum {
    CU_FUNC_CACHE_PREFER_NONE    = 0x00, /**< no preference for shared memory or L1 (default) */
    CU_FUNC_CACHE_PREFER_SHARED  = 0x01, /**< prefer larger shared memory and smaller L1 cache */
    CU_FUNC_CACHE_PREFER_L1      = 0x02, /**< prefer larger L1 cache and smaller shared memory */
    CU_FUNC_CACHE_PREFER_EQUAL   = 0x03  /**< prefer equal sized L1 cache and shared memory */
} CUfunc_cache;

/**
 * Shared memory configurations
 */
typedef enum CUsharedconfig_enum {
    CU_SHARED_MEM_CONFIG_DEFAULT_BANK_SIZE    = 0x00, /**< set default shared memory bank size */
    CU_SHARED_MEM_CONFIG_FOUR_BYTE_BANK_SIZE  = 0x01, /**< set shared memory bank width to four bytes */
    CU_SHARED_MEM_CONFIG_EIGHT_BYTE_BANK_SIZE = 0x02  /**< set shared memory bank width to eight bytes */
} CUsharedconfig;

/**
 * Shared memory carveout configurations. These may be passed to ::cuFuncSetAttribute
 */
typedef enum CUshared_carveout_enum {
    CU_SHAREDMEM_CARVEOUT_DEFAULT       = -1,  /**< No preference for shared memory or L1 (default) */
    CU_SHAREDMEM_CARVEOUT_MAX_SHARED    = 100, /**< Prefer maximum available shared memory, minimum L1 cache */
    CU_SHAREDMEM_CARVEOUT_MAX_L1        = 0    /**< Prefer maximum available L1 cache, minimum shared memory */
} CUshared_carveout;

/**
 * Memory types
 */
typedef enum CUmemorytype_enum {
    CU_MEMORYTYPE_HOST    = 0x01,    /**< Host memory */
    CU_MEMORYTYPE_DEVICE  = 0x02,    /**< Device memory */
    CU_MEMORYTYPE_ARRAY   = 0x03,    /**< Array memory */
    CU_MEMORYTYPE_UNIFIED = 0x04     /**< Unified device or host memory */
} CUmemorytype;

/**
 * Compute Modes
 */
typedef enum CUcomputemode_enum {
    CU_COMPUTEMODE_DEFAULT           = 0, /**< Default compute mode (Multiple contexts allowed per device) */
    CU_COMPUTEMODE_PROHIBITED        = 2, /**< Compute-prohibited mode (No contexts can be created on this device at this time) */
    CU_COMPUTEMODE_EXCLUSIVE_PROCESS = 3  /**< Compute-exclusive-process mode (Only one context used by a single process can be present on this device at a time) */
} CUcomputemode;

/**
 * Memory advise values
 */
typedef enum CUmem_advise_enum {
    CU_MEM_ADVISE_SET_READ_MOSTLY          = 1, /**< Data will mostly be read and only occassionally be written to */
    CU_MEM_ADVISE_UNSET_READ_MOSTLY        = 2, /**< Undo the effect of ::CU_MEM_ADVISE_SET_READ_MOSTLY */
    CU_MEM_ADVISE_SET_PREFERRED_LOCATION   = 3, /**< Set the preferred location for the data as the specified device */
    CU_MEM_ADVISE_UNSET_PREFERRED_LOCATION = 4, /**< Clear the preferred location for the data */
    CU_MEM_ADVISE_SET_ACCESSED_BY          = 5, /**< Data will be accessed by the specified device, so prevent page faults as much as possible */
    CU_MEM_ADVISE_UNSET_ACCESSED_BY        = 6  /**< Let the Unified Memory subsystem decide on the page faulting policy for the specified device */
} CUmem_advise;

typedef enum CUmem_range_attribute_enum {
    CU_MEM_RANGE_ATTRIBUTE_READ_MOSTLY            = 1, /**< Whether the range will mostly be read and only occassionally be written to */
    CU_MEM_RANGE_ATTRIBUTE_PREFERRED_LOCATION     = 2, /**< The preferred location of the range */
    CU_MEM_RANGE_ATTRIBUTE_ACCESSED_BY            = 3, /**< Memory range has ::CU_MEM_ADVISE_SET_ACCESSED_BY set for specified device */
    CU_MEM_RANGE_ATTRIBUTE_LAST_PREFETCH_LOCATION = 4  /**< The last location to which the range was prefetched */
} CUmem_range_attribute;

/**
 * Online compiler and linker options
 */
typedef enum CUjit_option_enum
{
    /**
     * Max number of registers that a thread may use.\n
     * Option type: unsigned int\n
     * Applies to: compiler only
     */
    CU_JIT_MAX_REGISTERS = 0,

    /**
     * IN: Specifies minimum number of threads per block to target compilation
     * for\n
     * OUT: Returns the number of threads the compiler actually targeted.
     * This restricts the resource utilization fo the compiler (e.g. max
     * registers) such that a block with the given number of threads should be
     * able to launch based on register limitations. Note, this option does not
     * currently take into account any other resource limitations, such as
     * shared memory utilization.\n
     * Cannot be combined with ::CU_JIT_TARGET.\n
     * Option type: unsigned int\n
     * Applies to: compiler only
     */
    CU_JIT_THREADS_PER_BLOCK,

    /**
     * Overwrites the option value with the total wall clock time, in
     * milliseconds, spent in the compiler and linker\n
     * Option type: float\n
     * Applies to: compiler and linker
     */
    CU_JIT_WALL_TIME,

    /**
     * Pointer to a buffer in which to print any log messages
     * that are informational in nature (the buffer size is specified via
     * option ::CU_JIT_INFO_LOG_BUFFER_SIZE_BYTES)\n
     * Option type: char *\n
     * Applies to: compiler and linker
     */
    CU_JIT_INFO_LOG_BUFFER,

    /**
     * IN: Log buffer size in bytes.  Log messages will be capped at this size
     * (including null terminator)\n
     * OUT: Amount of log buffer filled with messages\n
     * Option type: unsigned int\n
     * Applies to: compiler and linker
     */
    CU_JIT_INFO_LOG_BUFFER_SIZE_BYTES,

    /**
     * Pointer to a buffer in which to print any log messages that
     * reflect errors (the buffer size is specified via option
     * ::CU_JIT_ERROR_LOG_BUFFER_SIZE_BYTES)\n
     * Option type: char *\n
     * Applies to: compiler and linker
     */
    CU_JIT_ERROR_LOG_BUFFER,

    /**
     * IN: Log buffer size in bytes.  Log messages will be capped at this size
     * (including null terminator)\n
     * OUT: Amount of log buffer filled with messages\n
     * Option type: unsigned int\n
     * Applies to: compiler and linker
     */
    CU_JIT_ERROR_LOG_BUFFER_SIZE_BYTES,

    /**
     * Level of optimizations to apply to generated code (0 - 4), with 4
     * being the default and highest level of optimizations.\n
     * Option type: unsigned int\n
     * Applies to: compiler only
     */
    CU_JIT_OPTIMIZATION_LEVEL,

    /**
     * No option value required. Determines the target based on the current
     * attached context (default)\n
     * Option type: No option value needed\n
     * Applies to: compiler and linker
     */
    CU_JIT_TARGET_FROM_CUCONTEXT,

    /**
     * Target is chosen based on supplied ::CUjit_target.  Cannot be
     * combined with ::CU_JIT_THREADS_PER_BLOCK.\n
     * Option type: unsigned int for enumerated type ::CUjit_target\n
     * Applies to: compiler and linker
     */
    CU_JIT_TARGET,

    /**
     * Specifies choice of fallback strategy if matching cubin is not found.
     * Choice is based on supplied ::CUjit_fallback.  This option cannot be
     * used with cuLink* APIs as the linker requires exact matches.\n
     * Option type: unsigned int for enumerated type ::CUjit_fallback\n
     * Applies to: compiler only
     */
    CU_JIT_FALLBACK_STRATEGY,

    /**
     * Specifies whether to create debug information in output (-g)
     * (0: false, default)\n
     * Option type: int\n
     * Applies to: compiler and linker
     */
    CU_JIT_GENERATE_DEBUG_INFO,

    /**
     * Generate verbose log messages (0: false, default)\n
     * Option type: int\n
     * Applies to: compiler and linker
     */
    CU_JIT_LOG_VERBOSE,

    /**
     * Generate line number information (-lineinfo) (0: false, default)\n
     * Option type: int\n
     * Applies to: compiler only
     */
    CU_JIT_GENERATE_LINE_INFO,

    /**
     * Specifies whether to enable caching explicitly (-dlcm) \n
     * Choice is based on supplied ::CUjit_cacheMode_enum.\n
     * Option type: unsigned int for enumerated type ::CUjit_cacheMode_enum\n
     * Applies to: compiler only
     */
    CU_JIT_CACHE_MODE,

    /**
     * \deprecated
     * This jit option is deprecated and should not be used.
     */
    CU_JIT_NEW_SM3X_OPT,

    /**
     * This jit option is used for internal purpose only.
     */
    CU_JIT_FAST_COMPILE,

    /**
     * Array of device symbol names that will be relocated to the corresponing
     * host addresses stored in ::CU_JIT_GLOBAL_SYMBOL_ADDRESSES.\n
     * Must contain ::CU_JIT_GLOBAL_SYMBOL_COUNT entries.\n
     * When loding a device module, driver will relocate all encountered
     * unresolved symbols to the host addresses.\n
     * It is only allowed to register symbols that correspond to unresolved
     * global variables.\n
     * It is illegal to register the same device symbol at multiple addresses.\n
     * Option type: const char **\n
     * Applies to: dynamic linker only
     */
    CU_JIT_GLOBAL_SYMBOL_NAMES,

    /**
     * Array of host addresses that will be used to relocate corresponding
     * device symbols stored in ::CU_JIT_GLOBAL_SYMBOL_NAMES.\n
     * Must contain ::CU_JIT_GLOBAL_SYMBOL_COUNT entries.\n
     * Option type: void **\n
     * Applies to: dynamic linker only
     */
    CU_JIT_GLOBAL_SYMBOL_ADDRESSES,

    /**
     * Number of entries in ::CU_JIT_GLOBAL_SYMBOL_NAMES and
     * ::CU_JIT_GLOBAL_SYMBOL_ADDRESSES arrays.\n
     * Option type: unsigned int\n
     * Applies to: dynamic linker only
     */
    CU_JIT_GLOBAL_SYMBOL_COUNT,

    /**
     * Enable link-time optimization (-dlto) for device code (Disabled by default).\n
     * This option is not supported on 32-bit platforms.\n
     * Option type: int\n
     * Applies to: compiler and linker
     */
    CU_JIT_LTO,

    /**
     * Control single-precision denormals (-ftz) support (0: false, default).
     * 1 : flushes denormal values to zero
     * 0 : preserves denormal values
     * Option type: int\n
     * Applies to: link-time optimization specified with CU_JIT_LTO
     */
    CU_JIT_FTZ,

    /**
     * Control single-precision floating-point division and reciprocals
     * (-prec-div) support (1: true, default).
     * 1 : Enables the IEEE round-to-nearest mode
     * 0 : Enables the fast approximation mode
     * Option type: int\n
     * Applies to: link-time optimization specified with CU_JIT_LTO
     */
    CU_JIT_PREC_DIV,

    /**
     * Control single-precision floating-point square root
     * (-prec-sqrt) support (1: true, default).
     * 1 : Enables the IEEE round-to-nearest mode
     * 0 : Enables the fast approximation mode
     * Option type: int\n
     * Applies to: link-time optimization specified with CU_JIT_LTO
     */
    CU_JIT_PREC_SQRT,

    /**
     * Enable/Disable the contraction of floating-point multiplies
     * and adds/subtracts into floating-point multiply-add (-fma)
     * operations (1: Enable, default; 0: Disable).
     * Option type: int\n
     * Applies to: link-time optimization specified with CU_JIT_LTO
     */
    CU_JIT_FMA,


    /**
     * Array of kernel names that should be preserved at link time while others
     * can be removed.\n
     * Must contain ::CU_JIT_REFERENCED_KERNEL_COUNT entries.\n
     * Note that kernel names can be mangled by the compiler in which case the
     * mangled name needs to be specified.\n
     * Wildcard "*" can be used to represent zero or more characters instead of
     * specifying the full or mangled name.\n
     * It is important to note that the wildcard "*" is also added implicitly.
     * For example, specifying "foo" will match "foobaz", "barfoo", "barfoobaz" and
     * thus preserve all kernels with those names. This can be avoided by providing
     * a more specific name like "barfoobaz".\n
     * Option type: const char **\n
     * Applies to: dynamic linker only
     */
    CU_JIT_REFERENCED_KERNEL_NAMES,

    /**
     * Number of entries in ::CU_JIT_REFERENCED_KERNEL_NAMES array.\n
     * Option type: unsigned int\n
     * Applies to: dynamic linker only
     */
    CU_JIT_REFERENCED_KERNEL_COUNT,

    /**
     * Array of variable names (__device__ and/or __constant__) that should be
     * preserved at link time while others can be removed.\n
     * Must contain ::CU_JIT_REFERENCED_VARIABLE_COUNT entries.\n
     * Note that variable names can be mangled by the compiler in which case the
     * mangled name needs to be specified.\n
     * Wildcard "*" can be used to represent zero or more characters instead of
     * specifying the full or mangled name.\n
     * It is important to note that the wildcard "*" is also added implicitly.
     * For example, specifying "foo" will match "foobaz", "barfoo", "barfoobaz" and
     * thus preserve all variables with those names. This can be avoided by providing
     * a more specific name like "barfoobaz".\n
     * Option type: const char **\n
     * Applies to: link-time optimization specified with CU_JIT_LTO
     */
    CU_JIT_REFERENCED_VARIABLE_NAMES,

    /**
     * Number of entries in ::CU_JIT_REFERENCED_VARIABLE_NAMES array.\n
     * Option type: unsigned int\n
     * Applies to: link-time optimization specified with CU_JIT_LTO
     */
    CU_JIT_REFERENCED_VARIABLE_COUNT,

    /**
     * This option serves as a hint to enable the JIT compiler/linker
     * to remove constant (__constant__) and device (__device__) variables
     * unreferenced in device code (Disabled by default).\n
     * Note that host references to constant and device variables using APIs like
     * ::cuModuleGetGlobal() with this option specified may result in undefined behavior unless
     * the variables are explicitly specified using ::CU_JIT_REFERENCED_VARIABLE_NAMES.\n
     * Option type: int\n
     * Applies to: link-time optimization specified with CU_JIT_LTO
     */
    CU_JIT_OPTIMIZE_UNUSED_DEVICE_VARIABLES,


    CU_JIT_NUM_OPTIONS

} CUjit_option;

/**
 * Online compilation targets
 */
typedef enum CUjit_target_enum
{

    CU_TARGET_COMPUTE_20 = 20,       /**< Compute device class 2.0 */
    CU_TARGET_COMPUTE_21 = 21,       /**< Compute device class 2.1 */


    CU_TARGET_COMPUTE_30 = 30,       /**< Compute device class 3.0 */
    CU_TARGET_COMPUTE_32 = 32,       /**< Compute device class 3.2 */
    CU_TARGET_COMPUTE_35 = 35,       /**< Compute device class 3.5 */
    CU_TARGET_COMPUTE_37 = 37,       /**< Compute device class 3.7 */


    CU_TARGET_COMPUTE_50 = 50,       /**< Compute device class 5.0 */
    CU_TARGET_COMPUTE_52 = 52,       /**< Compute device class 5.2 */
    CU_TARGET_COMPUTE_53 = 53,       /**< Compute device class 5.3 */


    CU_TARGET_COMPUTE_60 = 60,       /**< Compute device class 6.0.*/
    CU_TARGET_COMPUTE_61 = 61,       /**< Compute device class 6.1.*/
    CU_TARGET_COMPUTE_62 = 62,       /**< Compute device class 6.2.*/


    CU_TARGET_COMPUTE_70 = 70,       /**< Compute device class 7.0.*/
    CU_TARGET_COMPUTE_72 = 72,       /**< Compute device class 7.2.*/

    CU_TARGET_COMPUTE_75 = 75,       /**< Compute device class 7.5.*/

    CU_TARGET_COMPUTE_80 = 80,       /**< Compute device class 8.0.*/
    CU_TARGET_COMPUTE_86 = 86,       /**< Compute device class 8.6.*/
    CU_TARGET_COMPUTE_87 = 87,       /**< Compute device class 8.7.*/

} CUjit_target;

/**
 * Cubin matching fallback strategies
 */
typedef enum CUjit_fallback_enum
{
    CU_PREFER_PTX = 0,  /**< Prefer to compile ptx if exact binary match not found */

    CU_PREFER_BINARY    /**< Prefer to fall back to compatible binary code if exact match not found */

} CUjit_fallback;

/**
 * Caching modes for dlcm
 */
typedef enum CUjit_cacheMode_enum
{
    CU_JIT_CACHE_OPTION_NONE = 0, /**< Compile with no -dlcm flag specified */
    CU_JIT_CACHE_OPTION_CG,       /**< Compile with L1 cache disabled */
    CU_JIT_CACHE_OPTION_CA        /**< Compile with L1 cache enabled */
} CUjit_cacheMode;

/**
 * Device code formats
 */
typedef enum CUjitInputType_enum
{
    /**
     * Compiled device-class-specific device code\n
     * Applicable options: none
     */
    CU_JIT_INPUT_CUBIN = 0,

    /**
     * PTX source code\n
     * Applicable options: PTX compiler options
     */
    CU_JIT_INPUT_PTX,

    /**
     * Bundle of multiple cubins and/or PTX of some device code\n
     * Applicable options: PTX compiler options, ::CU_JIT_FALLBACK_STRATEGY
     */
    CU_JIT_INPUT_FATBINARY,

    /**
     * Host object with embedded device code\n
     * Applicable options: PTX compiler options, ::CU_JIT_FALLBACK_STRATEGY
     */
    CU_JIT_INPUT_OBJECT,

    /**
     * Archive of host objects with embedded device code\n
     * Applicable options: PTX compiler options, ::CU_JIT_FALLBACK_STRATEGY
     */
    CU_JIT_INPUT_LIBRARY,

    /**
     * High-level intermediate code for link-time optimization\n
     * Applicable options: NVVM compiler options, PTX compiler options
     */
    CU_JIT_INPUT_NVVM,

    CU_JIT_NUM_INPUT_TYPES
} CUjitInputType;

typedef struct CUlinkState_st *CUlinkState;

/**
 * Flags to register a graphics resource
 */
typedef enum CUgraphicsRegisterFlags_enum {
    CU_GRAPHICS_REGISTER_FLAGS_NONE           = 0x00,
    CU_GRAPHICS_REGISTER_FLAGS_READ_ONLY      = 0x01,
    CU_GRAPHICS_REGISTER_FLAGS_WRITE_DISCARD  = 0x02,
    CU_GRAPHICS_REGISTER_FLAGS_SURFACE_LDST   = 0x04,
    CU_GRAPHICS_REGISTER_FLAGS_TEXTURE_GATHER = 0x08
} CUgraphicsRegisterFlags;

/**
 * Flags for mapping and unmapping interop resources
 */
typedef enum CUgraphicsMapResourceFlags_enum {
    CU_GRAPHICS_MAP_RESOURCE_FLAGS_NONE          = 0x00,
    CU_GRAPHICS_MAP_RESOURCE_FLAGS_READ_ONLY     = 0x01,
    CU_GRAPHICS_MAP_RESOURCE_FLAGS_WRITE_DISCARD = 0x02
} CUgraphicsMapResourceFlags;

/**
 * Array indices for cube faces
 */
typedef enum CUarray_cubemap_face_enum {
    CU_CUBEMAP_FACE_POSITIVE_X  = 0x00, /**< Positive X face of cubemap */
    CU_CUBEMAP_FACE_NEGATIVE_X  = 0x01, /**< Negative X face of cubemap */
    CU_CUBEMAP_FACE_POSITIVE_Y  = 0x02, /**< Positive Y face of cubemap */
    CU_CUBEMAP_FACE_NEGATIVE_Y  = 0x03, /**< Negative Y face of cubemap */
    CU_CUBEMAP_FACE_POSITIVE_Z  = 0x04, /**< Positive Z face of cubemap */
    CU_CUBEMAP_FACE_NEGATIVE_Z  = 0x05  /**< Negative Z face of cubemap */
} CUarray_cubemap_face;

/**
 * Limits
 */
typedef enum CUlimit_enum {
    CU_LIMIT_STACK_SIZE                       = 0x00, /**< GPU thread stack size */
    CU_LIMIT_PRINTF_FIFO_SIZE                 = 0x01, /**< GPU printf FIFO size */
    CU_LIMIT_MALLOC_HEAP_SIZE                 = 0x02, /**< GPU malloc heap size */
    CU_LIMIT_DEV_RUNTIME_SYNC_DEPTH           = 0x03, /**< GPU device runtime launch synchronize depth */
    CU_LIMIT_DEV_RUNTIME_PENDING_LAUNCH_COUNT = 0x04, /**< GPU device runtime pending launch count */
    CU_LIMIT_MAX_L2_FETCH_GRANULARITY         = 0x05, /**< A value between 0 and 128 that indicates the maximum fetch granularity of L2 (in Bytes). This is a hint */
    CU_LIMIT_PERSISTING_L2_CACHE_SIZE         = 0x06, /**< A size in bytes for L2 persisting lines cache size */
    CU_LIMIT_MAX
} CUlimit;

/**
 * Resource types
 */
typedef enum CUresourcetype_enum {
    CU_RESOURCE_TYPE_ARRAY           = 0x00, /**< Array resoure */
    CU_RESOURCE_TYPE_MIPMAPPED_ARRAY = 0x01, /**< Mipmapped array resource */
    CU_RESOURCE_TYPE_LINEAR          = 0x02, /**< Linear resource */
    CU_RESOURCE_TYPE_PITCH2D         = 0x03  /**< Pitch 2D resource */
} CUresourcetype;

#ifdef _WIN32
#define CUDA_CB __stdcall
#else
#define CUDA_CB
#endif

/**
 * CUDA host function
 * \param userData Argument value passed to the function
 */
typedef void (CUDA_CB *CUhostFn)(void *userData);

/**
 * Specifies performance hint with ::CUaccessPolicyWindow for hitProp and missProp members.
 */
typedef enum CUaccessProperty_enum {
    CU_ACCESS_PROPERTY_NORMAL           = 0,    /**< Normal cache persistence. */
    CU_ACCESS_PROPERTY_STREAMING        = 1,    /**< Streaming access is less likely to persit from cache. */
    CU_ACCESS_PROPERTY_PERSISTING       = 2     /**< Persisting access is more likely to persist in cache.*/
} CUaccessProperty;

/**
 * Specifies an access policy for a window, a contiguous extent of memory
 * beginning at base_ptr and ending at base_ptr + num_bytes.
 * num_bytes is limited by CU_DEVICE_ATTRIBUTE_MAX_ACCESS_POLICY_WINDOW_SIZE.
 * Partition into many segments and assign segments such that:
 * sum of "hit segments" / window == approx. ratio.
 * sum of "miss segments" / window == approx 1-ratio.
 * Segments and ratio specifications are fitted to the capabilities of
 * the architecture.
 * Accesses in a hit segment apply the hitProp access policy.
 * Accesses in a miss segment apply the missProp access policy.
 */
typedef struct CUaccessPolicyWindow_st {
    void *base_ptr;                     /**< Starting address of the access policy window. CUDA driver may align it. */
    size_t num_bytes;                   /**< Size in bytes of the window policy. CUDA driver may restrict the maximum size and alignment. */
    float hitRatio;                     /**< hitRatio specifies percentage of lines assigned hitProp, rest are assigned missProp. */
    CUaccessProperty hitProp;           /**< ::CUaccessProperty set for hit. */
    CUaccessProperty missProp;          /**< ::CUaccessProperty set for miss. Must be either NORMAL or STREAMING */
} CUaccessPolicyWindow_v1;
typedef CUaccessPolicyWindow_v1 CUaccessPolicyWindow;

/**
 * GPU kernel node parameters
 */
typedef struct CUDA_KERNEL_NODE_PARAMS_st {
    CUfunction func;             /**< Kernel to launch */
    unsigned int gridDimX;       /**< Width of grid in blocks */
    unsigned int gridDimY;       /**< Height of grid in blocks */
    unsigned int gridDimZ;       /**< Depth of grid in blocks */
    unsigned int blockDimX;      /**< X dimension of each thread block */
    unsigned int blockDimY;      /**< Y dimension of each thread block */
    unsigned int blockDimZ;      /**< Z dimension of each thread block */
    unsigned int sharedMemBytes; /**< Dynamic shared-memory size per thread block in bytes */
    void **kernelParams;         /**< Array of pointers to kernel parameters */
    void **extra;                /**< Extra options */
} CUDA_KERNEL_NODE_PARAMS_v1;
typedef CUDA_KERNEL_NODE_PARAMS_v1 CUDA_KERNEL_NODE_PARAMS;

/**
 * Memset node parameters
 */
typedef struct CUDA_MEMSET_NODE_PARAMS_st {
    CUdeviceptr dst;                        /**< Destination device pointer */
    size_t pitch;                           /**< Pitch of destination device pointer. Unused if height is 1 */
    unsigned int value;                     /**< Value to be set */
    unsigned int elementSize;               /**< Size of each element in bytes. Must be 1, 2, or 4. */
    size_t width;                           /**< Width of the row in elements */
    size_t height;                          /**< Number of rows */
} CUDA_MEMSET_NODE_PARAMS_v1;
typedef CUDA_MEMSET_NODE_PARAMS_v1 CUDA_MEMSET_NODE_PARAMS;

/**
 * Memset node parameters
 */
typedef struct CUDA_MEMSET_NODE_PARAMS_v2_st {
    CUdeviceptr dst;                        /**< Destination device pointer */
    size_t pitch;                           /**< Pitch of destination device pointer. Unused if height is 1 */
    unsigned int value;                     /**< Value to be set */
    unsigned int elementSize;               /**< Size of each element in bytes. Must be 1, 2, or 4. */
    size_t width;                           /**< Width of the row in elements */
    size_t height;                          /**< Number of rows */
    CUcontext ctx;                          /**< Context on which to run the node */
} CUDA_MEMSET_NODE_PARAMS_v2;

/**
 * Host node parameters
 */
typedef struct CUDA_HOST_NODE_PARAMS_st {
    CUhostFn fn;    /**< The function to call when the node executes */
    void* userData; /**< Argument to pass to the function */
} CUDA_HOST_NODE_PARAMS_v1;
typedef CUDA_HOST_NODE_PARAMS_v1 CUDA_HOST_NODE_PARAMS;

/**
 * Host node parameters
 */
typedef struct CUDA_HOST_NODE_PARAMS_v2_st {
    CUhostFn fn;    /**< The function to call when the node executes */
    void* userData; /**< Argument to pass to the function */
} CUDA_HOST_NODE_PARAMS_v2;

/**
 * Child graph node parameters
 */
typedef struct CUDA_CHILD_GRAPH_NODE_PARAMS_st {
    CUgraph graph; /**< The child graph to clone into the node for node creation, or
                        a handle to the graph owned by the node for node query */
} CUDA_CHILD_GRAPH_NODE_PARAMS;


/**
 * Memory free node parameters
 */
typedef struct CUDA_MEM_FREE_NODE_PARAMS_st {
    CUdeviceptr dptr; /**< in: the pointer to free */
} CUDA_MEM_FREE_NODE_PARAMS;

/**
 * GPU kernel node parameters
 */
typedef struct CUDA_KERNEL_NODE_PARAMS_v3_st {
    CUfunction func;             /**< Kernel to launch */
    unsigned int gridDimX;       /**< Width of grid in blocks */
    unsigned int gridDimY;       /**< Height of grid in blocks */
    unsigned int gridDimZ;       /**< Depth of grid in blocks */
    unsigned int blockDimX;      /**< X dimension of each thread block */
    unsigned int blockDimY;      /**< Y dimension of each thread block */
    unsigned int blockDimZ;      /**< Z dimension of each thread block */
    unsigned int sharedMemBytes; /**< Dynamic shared-memory size per thread block in bytes */
    void **kernelParams;         /**< Array of pointers to kernel parameters */
    void **extra;                /**< Extra options */
    CUkernel kern;               /**< Kernel to launch, will only be referenced if func is NULL */
    CUcontext ctx;               /**< Context for the kernel task to run in. The value NULL will indicate the current context should be used by the api. This field is ignored if func is set. */
} CUDA_KERNEL_NODE_PARAMS_v3;

/**
 * Graph node types
 */
typedef enum CUgraphNodeType_enum {
    CU_GRAPH_NODE_TYPE_KERNEL           = 0, /**< GPU kernel node */
    CU_GRAPH_NODE_TYPE_MEMCPY           = 1, /**< Memcpy node */
    CU_GRAPH_NODE_TYPE_MEMSET           = 2, /**< Memset node */
    CU_GRAPH_NODE_TYPE_HOST             = 3, /**< Host (executable) node */
    CU_GRAPH_NODE_TYPE_GRAPH            = 4, /**< Node which executes an embedded graph */
    CU_GRAPH_NODE_TYPE_EMPTY            = 5, /**< Empty (no-op) node */
    CU_GRAPH_NODE_TYPE_WAIT_EVENT       = 6, /**< External event wait node */
    CU_GRAPH_NODE_TYPE_EVENT_RECORD     = 7, /**< External event record node */
    CU_GRAPH_NODE_TYPE_EXT_SEMAS_SIGNAL = 8, /**< External semaphore signal node */
    CU_GRAPH_NODE_TYPE_EXT_SEMAS_WAIT   = 9, /**< External semaphore wait node */
    CU_GRAPH_NODE_TYPE_MEM_ALLOC        = 10,/**< Memory Allocation Node */
    CU_GRAPH_NODE_TYPE_MEM_FREE         = 11, /**< Memory Free Node */
    CU_GRAPH_NODE_TYPE_BATCH_MEM_OP     = 12, /**< Batch MemOp Node */
    CU_GRAPH_NODE_TYPE_CONDITIONAL      = 13, /**Conditional NodeMay be used to implement a conditional execution path or loop inside of a graph. The graph(s) contained within the body of the conditional node can be selectively executed or iterated upon based on the value of a conditional variable.Handles must be created in advance of creating the node using cuGraphConditionalHandleCreate.The following restrictions apply to graphs which contain conditional nodes: The graph cannot be used in a child node. Only one instantiation of the graph may exist at any point in time. The graph cannot be cloned.To set the control value:In a kernel or kernels at appropriate locations in the graph, insert a call to void cudaGraphSetConditional(CUgraphConditionalHandle handle, unsigned int value). Supply a default value when creating the handle.*/
} CUgraphNodeType;

/**
 * Event wait node parameters
 */
typedef struct CUDA_EVENT_WAIT_NODE_PARAMS_st {
    CUevent event; /**< The event to wait on from the node */
} CUDA_EVENT_WAIT_NODE_PARAMS;

/**
 * Event record node parameters
 */
typedef struct CUDA_EVENT_RECORD_NODE_PARAMS_st {
    CUevent event; /**< The event to record when the node executes */
} CUDA_EVENT_RECORD_NODE_PARAMS;

typedef struct CUDA_BATCH_MEM_OP_NODE_PARAMS_v1_st {
    CUcontext ctx;
    unsigned int count;
    CUstreamBatchMemOpParams *paramArray;
    unsigned int flags;
} CUDA_BATCH_MEM_OP_NODE_PARAMS_v1;
typedef CUDA_BATCH_MEM_OP_NODE_PARAMS_v1 CUDA_BATCH_MEM_OP_NODE_PARAMS;

typedef struct CUDA_BATCH_MEM_OP_NODE_PARAMS_v2_st {
    CUcontext ctx;
    unsigned int count;
    CUstreamBatchMemOpParams *paramArray;
    unsigned int flags;
} CUDA_BATCH_MEM_OP_NODE_PARAMS_v2;

typedef struct CUgraphEdgeData_st {
    unsigned char  from_port;
    unsigned char  reserved[5];
    unsigned char  to_port;
    unsigned char  type;
} CUgraphEdgeData;

/**
 * Graph instantiation results
*/
typedef enum CUgraphInstantiateResult_enum
{
    CUDA_GRAPH_INSTANTIATE_SUCCESS = 0,                          /**< Instantiation succeeded */
    CUDA_GRAPH_INSTANTIATE_ERROR = 1,                            /**< Instantiation failed for an unexpected reason which is described in the return value of the function */
    CUDA_GRAPH_INSTANTIATE_INVALID_STRUCTURE = 2,                /**< Instantiation failed due to invalid structure, such as cycles */
    CUDA_GRAPH_INSTANTIATE_NODE_OPERATION_NOT_SUPPORTED = 3,     /**< Instantiation for device launch failed because the graph contained an unsupported operation */
    CUDA_GRAPH_INSTANTIATE_MULTIPLE_CTXS_NOT_SUPPORTED = 4       /**< Instantiation for device launch failed due to the nodes belonging to different contexts */
} CUgraphInstantiateResult;

/**
 * Graph instantiation parameters
 */
typedef struct CUDA_GRAPH_INSTANTIATE_PARAMS_st
{
	cuuint64_t flags;                    /**< Instantiation flags */
	CUstream hUploadStream;              /**< Upload stream */
	CUgraphNode hErrNode_out;            /**< The node which caused instantiation to fail, if any */
	CUgraphInstantiateResult result_out; /**< Whether instantiation was successful.  If it failed, the reason why */
} CUDA_GRAPH_INSTANTIATE_PARAMS;

typedef enum CUsynchronizationPolicy_enum {
    CU_SYNC_POLICY_AUTO = 1,
    CU_SYNC_POLICY_SPIN = 2,
    CU_SYNC_POLICY_YIELD = 3,
    CU_SYNC_POLICY_BLOCKING_SYNC = 4
} CUsynchronizationPolicy;

/**
 * Graph kernel node Attributes
 */

typedef enum CUkernelNodeAttrID_enum {
    CU_KERNEL_NODE_ATTRIBUTE_ACCESS_POLICY_WINDOW       = 1    /**< Identifier for ::CUkernelNodeAttrValue::accessPolicyWindow. */
  , CU_KERNEL_NODE_ATTRIBUTE_COOPERATIVE                = 2    /**< Allows a kernel node to be cooperative (see ::cuLaunchCooperativeKernel). */

  , CU_KERNEL_NODE_ATTRIBUTE_PRIORITY                   = 8    /**< Sets the priority of the kernel. */

} CUkernelNodeAttrID;

/**
 * Graph kernel node attributes union, used with ::cuKernelNodeSetAttribute/::cuKernelNodeGetAttribute
 */

typedef union CUkernelNodeAttrValue_union {
    CUaccessPolicyWindow accessPolicyWindow;    /**< Attribute ::CUaccessPolicyWindow. */
    int cooperative;                            /**< Nonzero indicates a cooperative kernel (see ::cuLaunchCooperativeKernel). */
    int priority;                               /**< Execution priority of the kernel. */
} CUkernelNodeAttrValue_v1;

typedef CUkernelNodeAttrValue_v1 CUkernelNodeAttrValue;

/**
 * Possible stream capture statuses returned by ::cuStreamIsCapturing
 */
typedef enum CUstreamCaptureStatus_enum {
    CU_STREAM_CAPTURE_STATUS_NONE        = 0, /**< Stream is not capturing */
    CU_STREAM_CAPTURE_STATUS_ACTIVE      = 1, /**< Stream is actively capturing */
    CU_STREAM_CAPTURE_STATUS_INVALIDATED = 2  /**< Stream is part of a capture sequence that
                                                   has been invalidated, but not terminated */
} CUstreamCaptureStatus;

/**
 * Possible modes for stream capture thread interactions. For more details see
 * ::cuStreamBeginCapture and ::cuThreadExchangeStreamCaptureMode
 */
typedef enum CUstreamCaptureMode_enum {
    CU_STREAM_CAPTURE_MODE_GLOBAL       = 0,
    CU_STREAM_CAPTURE_MODE_THREAD_LOCAL = 1,
    CU_STREAM_CAPTURE_MODE_RELAXED      = 2
} CUstreamCaptureMode;

/**
 * Stream Attributes 
 */

typedef enum CUstreamAttrID_enum {
    CU_STREAM_ATTRIBUTE_ACCESS_POLICY_WINDOW    = 1,   /**< Identifier for ::CUstreamAttrValue::accessPolicyWindow. */
    CU_STREAM_ATTRIBUTE_SYNCHRONIZATION_POLICY  = 3    /**< ::CUsynchronizationPolicy for work queued up in this stream */
} CUstreamAttrID;


/**
 * Stream attributes union, used with ::cuStreamSetAttribute/::cuStreamGetAttribute
 */

typedef union CUstreamAttrValue_union {
    CUaccessPolicyWindow accessPolicyWindow;   /**< Attribute ::CUaccessPolicyWindow. */
    CUsynchronizationPolicy syncPolicy;        /**< Value for ::CU_STREAM_ATTRIBUTE_SYNCHRONIZATION_POLICY. */
} CUstreamAttrValue_v1;

typedef CUstreamAttrValue_v1 CUstreamAttrValue;

/**
 * Flags to specify search options. For more details see ::cuGetProcAddress
 */
typedef enum CUdriverProcAddress_flags_enum {
    CU_GET_PROC_ADDRESS_DEFAULT = 0,                        /**< Default search mode for driver symbols. */
    CU_GET_PROC_ADDRESS_LEGACY_STREAM = 1 << 0,             /**< Search for legacy versions of driver symbols. */
    CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM = 1 << 1  /**< Search for per-thread versions of driver symbols. */ 
} CUdriverProcAddress_flags;

/**
 * Execution Affinity Types 
 */
typedef enum CUexecAffinityType_enum {
    CU_EXEC_AFFINITY_TYPE_SM_COUNT = 0,  /**< Create a context with limited SMs. */
    CU_EXEC_AFFINITY_TYPE_MAX
} CUexecAffinityType;

/**
 * Value for ::CU_EXEC_AFFINITY_TYPE_SM_COUNT
 */
typedef struct CUexecAffinitySmCount_st {
    unsigned int val;    /**< The number of SMs the context is limited to use. */
} CUexecAffinitySmCount_v1;
typedef CUexecAffinitySmCount_v1 CUexecAffinitySmCount;

/**
 * Execution Affinity Parameters 
 */
typedef struct CUexecAffinityParam_st {
    CUexecAffinityType type;
    union {
        CUexecAffinitySmCount smCount;    /** Value for ::CU_EXEC_AFFINITY_TYPE_SM_COUNT */
    } param;
} CUexecAffinityParam_v1;
typedef CUexecAffinityParam_v1 CUexecAffinityParam;

typedef enum CUcigDataType_enum {
    CIG_DATA_TYPE_D3D12_COMMAND_QUEUE = 0x1,    /** D3D12 Command Queue Handle */
} CUcigDataType;

/**
* CIG Context Create Params
*/
typedef struct CUctxCigParam_st {
    CUcigDataType sharedDataType;
    void* sharedData;
} CUctxCigParam;

/**
* Params for creating CUDA context
* Exactly one of execAffinityParams and cigParams
* must be non-NULL.
*/
typedef struct CUctxCreateParams_st {
    CUexecAffinityParam *execAffinityParams;
    int                  numExecAffinityParams;
    CUctxCigParam       *cigParams;
} CUctxCreateParams;

/**
 * Error codes
 */
typedef enum cudaError_enum {
    /**
     * The API call returned with no errors. In the case of query calls, this
     * also means that the operation being queried is complete (see
     * ::cuEventQuery() and ::cuStreamQuery()).
     */
    CUDA_SUCCESS                              = 0,

    /**
     * This indicates that one or more of the parameters passed to the API call
     * is not within an acceptable range of values.
     */
    CUDA_ERROR_INVALID_VALUE                  = 1,

    /**
     * The API call failed because it was unable to allocate enough memory to
     * perform the requested operation.
     */
    CUDA_ERROR_OUT_OF_MEMORY                  = 2,

    /**
     * This indicates that the CUDA driver has not been initialized with
     * ::cuInit() or that initialization has failed.
     */
    CUDA_ERROR_NOT_INITIALIZED                = 3,

    /**
     * This indicates that the CUDA driver is in the process of shutting down.
     */
    CUDA_ERROR_DEINITIALIZED                  = 4,

    /**
     * This indicates profiler is not initialized for this run. This can
     * happen when the application is running with external profiling tools
     * like visual profiler.
     */
    CUDA_ERROR_PROFILER_DISABLED              = 5,

    /**
     * \deprecated
     * This error return is deprecated as of CUDA 5.0. It is no longer an error
     * to attempt to enable/disable the profiling via ::cuProfilerStart or
     * ::cuProfilerStop without initialization.
     */
    CUDA_ERROR_PROFILER_NOT_INITIALIZED       = 6,

    /**
     * \deprecated
     * This error return is deprecated as of CUDA 5.0. It is no longer an error
     * to call cuProfilerStart() when profiling is already enabled.
     */
    CUDA_ERROR_PROFILER_ALREADY_STARTED       = 7,

    /**
     * \deprecated
     * This error return is deprecated as of CUDA 5.0. It is no longer an error
     * to call cuProfilerStop() when profiling is already disabled.
     */
    CUDA_ERROR_PROFILER_ALREADY_STOPPED       = 8,

    /**
     * This indicates that the CUDA driver that the application has loaded is a
     * stub library. Applications that run with the stub rather than a real
     * driver loaded will result in CUDA API returning this error.
     */
    CUDA_ERROR_STUB_LIBRARY                   = 34,

    /**  
     * This indicates that requested CUDA device is unavailable at the current
     * time. Devices are often unavailable due to use of
     * ::CU_COMPUTEMODE_EXCLUSIVE_PROCESS or ::CU_COMPUTEMODE_PROHIBITED.
     */
    CUDA_ERROR_DEVICE_UNAVAILABLE            = 46,

    /**
     * This indicates that no CUDA-capable devices were detected by the installed
     * CUDA driver.
     */
    CUDA_ERROR_NO_DEVICE                      = 100,

    /**
     * This indicates that the device ordinal supplied by the user does not
     * correspond to a valid CUDA device or that the action requested is
     * invalid for the specified device.
     */
    CUDA_ERROR_INVALID_DEVICE                 = 101,

    /**
     * This error indicates that the Grid license is not applied.
     */
    CUDA_ERROR_DEVICE_NOT_LICENSED            = 102,

    /**
     * This indicates that the device kernel image is invalid. This can also
     * indicate an invalid CUDA module.
     */
    CUDA_ERROR_INVALID_IMAGE                  = 200,

    /**
     * This most frequently indicates that there is no context bound to the
     * current thread. This can also be returned if the context passed to an
     * API call is not a valid handle (such as a context that has had
     * ::cuCtxDestroy() invoked on it). This can also be returned if a user
     * mixes different API versions (i.e. 3010 context with 3020 API calls).
     * See ::cuCtxGetApiVersion() for more details.
     */
    CUDA_ERROR_INVALID_CONTEXT                = 201,

    /**
     * This indicated that the context being supplied as a parameter to the
     * API call was already the active context.
     * \deprecated
     * This error return is deprecated as of CUDA 3.2. It is no longer an
     * error to attempt to push the active context via ::cuCtxPushCurrent().
     */
    CUDA_ERROR_CONTEXT_ALREADY_CURRENT        = 202,

    /**
     * This indicates that a map or register operation has failed.
     */
    CUDA_ERROR_MAP_FAILED                     = 205,

    /**
     * This indicates that an unmap or unregister operation has failed.
     */
    CUDA_ERROR_UNMAP_FAILED                   = 206,

    /**
     * This indicates that the specified array is currently mapped and thus
     * cannot be destroyed.
     */
    CUDA_ERROR_ARRAY_IS_MAPPED                = 207,

    /**
     * This indicates that the resource is already mapped.
     */
    CUDA_ERROR_ALREADY_MAPPED                 = 208,

    /**
     * This indicates that there is no kernel image available that is suitable
     * for the device. This can occur when a user specifies code generation
     * options for a particular CUDA source file that do not include the
     * corresponding device configuration.
     */
    CUDA_ERROR_NO_BINARY_FOR_GPU              = 209,

    /**
     * This indicates that a resource has already been acquired.
     */
    CUDA_ERROR_ALREADY_ACQUIRED               = 210,

    /**
     * This indicates that a resource is not mapped.
     */
    CUDA_ERROR_NOT_MAPPED                     = 211,

    /**
     * This indicates that a mapped resource is not available for access as an
     * array.
     */
    CUDA_ERROR_NOT_MAPPED_AS_ARRAY            = 212,

    /**
     * This indicates that a mapped resource is not available for access as a
     * pointer.
     */
    CUDA_ERROR_NOT_MAPPED_AS_POINTER          = 213,

    /**
     * This indicates that an uncorrectable ECC error was detected during
     * execution.
     */
    CUDA_ERROR_ECC_UNCORRECTABLE              = 214,

    /**
     * This indicates that the ::CUlimit passed to the API call is not
     * supported by the active device.
     */
    CUDA_ERROR_UNSUPPORTED_LIMIT              = 215,

    /**
     * This indicates that the ::CUcontext passed to the API call can
     * only be bound to a single CPU thread at a time but is already
     * bound to a CPU thread.
     */
    CUDA_ERROR_CONTEXT_ALREADY_IN_USE         = 216,

    /**
     * This indicates that peer access is not supported across the given
     * devices.
     */
    CUDA_ERROR_PEER_ACCESS_UNSUPPORTED        = 217,

    /**
     * This indicates that a PTX JIT compilation failed.
     */
    CUDA_ERROR_INVALID_PTX                    = 218,

    /**
     * This indicates an error with OpenGL or DirectX context.
     */
    CUDA_ERROR_INVALID_GRAPHICS_CONTEXT       = 219,

    /**
    * This indicates that an uncorrectable NVLink error was detected during the
    * execution.
    */
    CUDA_ERROR_NVLINK_UNCORRECTABLE           = 220,

    /**
    * This indicates that the PTX JIT compiler library was not found.
    */
    CUDA_ERROR_JIT_COMPILER_NOT_FOUND         = 221,

    /**
     * This indicates that the provided PTX was compiled with an unsupported toolchain.
     */

    CUDA_ERROR_UNSUPPORTED_PTX_VERSION        = 222,

    /**
     * This indicates that the PTX JIT compilation was disabled.
     */
    CUDA_ERROR_JIT_COMPILATION_DISABLED       = 223,

    /**
     * This indicates that the ::CUexecAffinityType passed to the API call is not
     * supported by the active device.
     */ 
    CUDA_ERROR_UNSUPPORTED_EXEC_AFFINITY      = 224,

    /**
     * This indicates that the device kernel source is invalid. This includes
     * compilation/linker errors encountered in device code or user error.
     */
    CUDA_ERROR_INVALID_SOURCE                 = 300,

    /**
     * This indicates that the file specified was not found.
     */
    CUDA_ERROR_FILE_NOT_FOUND                 = 301,

    /**
     * This indicates that a link to a shared object failed to resolve.
     */
    CUDA_ERROR_SHARED_OBJECT_SYMBOL_NOT_FOUND = 302,

    /**
     * This indicates that initialization of a shared object failed.
     */
    CUDA_ERROR_SHARED_OBJECT_INIT_FAILED      = 303,

    /**
     * This indicates that an OS call failed.
     */
    CUDA_ERROR_OPERATING_SYSTEM               = 304,

    /**
     * This indicates that a resource handle passed to the API call was not
     * valid. Resource handles are opaque types like ::CUstream and ::CUevent.
     */
    CUDA_ERROR_INVALID_HANDLE                 = 400,

    /**
     * This indicates that a resource required by the API call is not in a
     * valid state to perform the requested operation.
     */
    CUDA_ERROR_ILLEGAL_STATE                  = 401,

    /**
     * This indicates that a named symbol was not found. Examples of symbols
     * are global/constant variable names, driver function names, texture names,
     * and surface names.
     */
    CUDA_ERROR_NOT_FOUND                      = 500,

    /**
     * This indicates that asynchronous operations issued previously have not
     * completed yet. This result is not actually an error, but must be indicated
     * differently than ::CUDA_SUCCESS (which indicates completion). Calls that
     * may return this value include ::cuEventQuery() and ::cuStreamQuery().
     */
    CUDA_ERROR_NOT_READY                      = 600,

    /**
     * While executing a kernel, the device encountered a
     * load or store instruction on an invalid memory address.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_ILLEGAL_ADDRESS                = 700,

    /**
     * This indicates that a launch did not occur because it did not have
     * appropriate resources. This error usually indicates that the user has
     * attempted to pass too many arguments to the device kernel, or the
     * kernel launch specifies too many threads for the kernel's register
     * count. Passing arguments of the wrong size (i.e. a 64-bit pointer
     * when a 32-bit int is expected) is equivalent to passing too many
     * arguments and can also result in this error.
     */
    CUDA_ERROR_LAUNCH_OUT_OF_RESOURCES        = 701,

    /**
     * This indicates that the device kernel took too long to execute. This can
     * only occur if timeouts are enabled - see the device attribute
     * ::CU_DEVICE_ATTRIBUTE_KERNEL_EXEC_TIMEOUT for more information.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_LAUNCH_TIMEOUT                 = 702,

    /**
     * This error indicates a kernel launch that uses an incompatible texturing
     * mode.
     */
    CUDA_ERROR_LAUNCH_INCOMPATIBLE_TEXTURING  = 703,

    /**
     * This error indicates that a call to ::cuCtxEnablePeerAccess() is
     * trying to re-enable peer access to a context which has already
     * had peer access to it enabled.
     */
    CUDA_ERROR_PEER_ACCESS_ALREADY_ENABLED    = 704,

    /**
     * This error indicates that ::cuCtxDisablePeerAccess() is
     * trying to disable peer access which has not been enabled yet
     * via ::cuCtxEnablePeerAccess().
     */
    CUDA_ERROR_PEER_ACCESS_NOT_ENABLED        = 705,

    /**
     * This error indicates that the primary context for the specified device
     * has already been initialized.
     */
    CUDA_ERROR_PRIMARY_CONTEXT_ACTIVE         = 708,

    /**
     * This error indicates that the context current to the calling thread
     * has been destroyed using ::cuCtxDestroy, or is a primary context which
     * has not yet been initialized.
     */
    CUDA_ERROR_CONTEXT_IS_DESTROYED           = 709,

    /**
     * A device-side assert triggered during kernel execution. The context
     * cannot be used anymore, and must be destroyed. All existing device
     * memory allocations from this context are invalid and must be
     * reconstructed if the program is to continue using CUDA.
     */
    CUDA_ERROR_ASSERT                         = 710,

    /**
     * This error indicates that the hardware resources required to enable
     * peer access have been exhausted for one or more of the devices
     * passed to ::cuCtxEnablePeerAccess().
     */
    CUDA_ERROR_TOO_MANY_PEERS                 = 711,

    /**
     * This error indicates that the memory range passed to ::cuMemHostRegister()
     * has already been registered.
     */
    CUDA_ERROR_HOST_MEMORY_ALREADY_REGISTERED = 712,

    /**
     * This error indicates that the pointer passed to ::cuMemHostUnregister()
     * does not correspond to any currently registered memory region.
     */
    CUDA_ERROR_HOST_MEMORY_NOT_REGISTERED     = 713,

    /**
     * While executing a kernel, the device encountered a stack error.
     * This can be due to stack corruption or exceeding the stack size limit.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_HARDWARE_STACK_ERROR           = 714,

    /**
     * While executing a kernel, the device encountered an illegal instruction.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_ILLEGAL_INSTRUCTION            = 715,

    /**
     * While executing a kernel, the device encountered a load or store instruction
     * on a memory address which is not aligned.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_MISALIGNED_ADDRESS             = 716,

    /**
     * While executing a kernel, the device encountered an instruction
     * which can only operate on memory locations in certain address spaces
     * (global, shared, or local), but was supplied a memory address not
     * belonging to an allowed address space.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_INVALID_ADDRESS_SPACE          = 717,

    /**
     * While executing a kernel, the device program counter wrapped its address space.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_INVALID_PC                     = 718,

    /**
     * An exception occurred on the device while executing a kernel. Common
     * causes include dereferencing an invalid device pointer and accessing
     * out of bounds shared memory. Less common cases can be system specific - more
     * information about these cases can be found in the system specific user guide.
     * This leaves the process in an inconsistent state and any further CUDA work
     * will return the same error. To continue using CUDA, the process must be terminated
     * and relaunched.
     */
    CUDA_ERROR_LAUNCH_FAILED                  = 719,

    /**
     * This error indicates that the number of blocks launched per grid for a kernel that was
     * launched via either ::cuLaunchCooperativeKernel or ::cuLaunchCooperativeKernelMultiDevice
     * exceeds the maximum number of blocks as allowed by ::cuOccupancyMaxActiveBlocksPerMultiprocessor
     * or ::cuOccupancyMaxActiveBlocksPerMultiprocessorWithFlags times the number of multiprocessors
     * as specified by the device attribute ::CU_DEVICE_ATTRIBUTE_MULTIPROCESSOR_COUNT.
     */
    CUDA_ERROR_COOPERATIVE_LAUNCH_TOO_LARGE   = 720,

    /**
     * This error indicates that the attempted operation is not permitted.
     */
    CUDA_ERROR_NOT_PERMITTED                  = 800,

    /**
     * This error indicates that the attempted operation is not supported
     * on the current system or device.
     */
    CUDA_ERROR_NOT_SUPPORTED                  = 801,

    /**
     * This error indicates that the system is not yet ready to start any CUDA
     * work.  To continue using CUDA, verify the system configuration is in a
     * valid state and all required driver daemons are actively running.
     * More information about this error can be found in the system specific
     * user guide.
     */
    CUDA_ERROR_SYSTEM_NOT_READY               = 802,

    /**
     * This error indicates that there is a mismatch between the versions of
     * the display driver and the CUDA driver. Refer to the compatibility documentation
     * for supported versions.
     */
    CUDA_ERROR_SYSTEM_DRIVER_MISMATCH         = 803,

    /**
     * This error indicates that the system was upgraded to run with forward compatibility
     * but the visible hardware detected by CUDA does not support this configuration.
     * Refer to the compatibility documentation for the supported hardware matrix or ensure
     * that only supported hardware is visible during initialization via the CUDA_VISIBLE_DEVICES
     * environment variable.
     */
    CUDA_ERROR_COMPAT_NOT_SUPPORTED_ON_DEVICE = 804,

    /**
     * This error indicates that the MPS client failed to connect to the MPS control daemon or the MPS server.
     */
    CUDA_ERROR_MPS_CONNECTION_FAILED          = 805,

    /**
     * This error indicates that the remote procedural call between the MPS server and the MPS client failed.
     */
    CUDA_ERROR_MPS_RPC_FAILURE                = 806,

    /**
     * This error indicates that the MPS server is not ready to accept new MPS client requests.
     * This error can be returned when the MPS server is in the process of recovering from a fatal failure.
     */
    CUDA_ERROR_MPS_SERVER_NOT_READY           = 807,

    /**
     * This error indicates that the hardware resources required to create MPS client have been exhausted.
     */
    CUDA_ERROR_MPS_MAX_CLIENTS_REACHED        = 808,

    /**
     * This error indicates the the hardware resources required to support device connections have been exhausted.
     */
    CUDA_ERROR_MPS_MAX_CONNECTIONS_REACHED    = 809,

    /**
     * This error indicates that the operation is not permitted when
     * the stream is capturing.
     */
    CUDA_ERROR_STREAM_CAPTURE_UNSUPPORTED     = 900,

    /**
     * This error indicates that the current capture sequence on the stream
     * has been invalidated due to a previous error.
     */
    CUDA_ERROR_STREAM_CAPTURE_INVALIDATED     = 901,

    /**
     * This error indicates that the operation would have resulted in a merge
     * of two independent capture sequences.
     */
    CUDA_ERROR_STREAM_CAPTURE_MERGE           = 902,

    /**
     * This error indicates that the capture was not initiated in this stream.
     */
    CUDA_ERROR_STREAM_CAPTURE_UNMATCHED       = 903,

    /**
     * This error indicates that the capture sequence contains a fork that was
     * not joined to the primary stream.
     */
    CUDA_ERROR_STREAM_CAPTURE_UNJOINED        = 904,

    /**
     * This error indicates that a dependency would have been created which
     * crosses the capture sequence boundary. Only implicit in-stream ordering
     * dependencies are allowed to cross the boundary.
     */
    CUDA_ERROR_STREAM_CAPTURE_ISOLATION       = 905,

    /**
     * This error indicates a disallowed implicit dependency on a current capture
     * sequence from cudaStreamLegacy.
     */
    CUDA_ERROR_STREAM_CAPTURE_IMPLICIT        = 906,

    /**
     * This error indicates that the operation is not permitted on an event which
     * was last recorded in a capturing stream.
     */
    CUDA_ERROR_CAPTURED_EVENT                 = 907,

    /**
     * A stream capture sequence not initiated with the ::CU_STREAM_CAPTURE_MODE_RELAXED
     * argument to ::cuStreamBeginCapture was passed to ::cuStreamEndCapture in a
     * different thread.
     */
    CUDA_ERROR_STREAM_CAPTURE_WRONG_THREAD    = 908,

    /**
     * This error indicates that the timeout specified for the wait operation has lapsed.
     */
    CUDA_ERROR_TIMEOUT                        = 909,

    /**
     * This error indicates that the graph update was not performed because it included 
     * changes which violated constraints specific to instantiated graph update.
     */
    CUDA_ERROR_GRAPH_EXEC_UPDATE_FAILURE      = 910,

    /**
     * This indicates that an async error has occurred in a device outside of CUDA.
     * If CUDA was waiting for an external device's signal before consuming shared data,
     * the external device signaled an error indicating that the data is not valid for
     * consumption. This leaves the process in an inconsistent state and any further CUDA
     * work will return the same error. To continue using CUDA, the process must be
     * terminated and relaunched.
     */
    CUDA_ERROR_EXTERNAL_DEVICE               = 911,

    /**
     * This indicates that an unknown internal error has occurred.
     */
    CUDA_ERROR_UNKNOWN                        = 999
} CUresult;

/**
 * P2P Attributes
 */
typedef enum CUdevice_P2PAttribute_enum {
    CU_DEVICE_P2P_ATTRIBUTE_PERFORMANCE_RANK                     = 0x01,  /**< A relative value indicating the performance of the link between two devices */
    CU_DEVICE_P2P_ATTRIBUTE_ACCESS_SUPPORTED                     = 0x02,  /**< P2P Access is enable */
    CU_DEVICE_P2P_ATTRIBUTE_NATIVE_ATOMIC_SUPPORTED              = 0x03,  /**< Atomic operation over the link supported */
    CU_DEVICE_P2P_ATTRIBUTE_ACCESS_ACCESS_SUPPORTED              = 0x04,  /**< \deprecated use CU_DEVICE_P2P_ATTRIBUTE_CUDA_ARRAY_ACCESS_SUPPORTED instead */
    CU_DEVICE_P2P_ATTRIBUTE_CUDA_ARRAY_ACCESS_SUPPORTED          = 0x04   /**< Accessing CUDA arrays over the link supported */
} CUdevice_P2PAttribute;

/**
 * CUDA stream callback
 * \param hStream The stream the callback was added to, as passed to ::cuStreamAddCallback.  May be NULL.
 * \param status ::CUDA_SUCCESS or any persistent error on the stream.
 * \param userData User parameter provided at registration.
 */
typedef void (CUDA_CB *CUstreamCallback)(CUstream hStream, CUresult status, void *userData);

/**
 * Block size to per-block dynamic shared memory mapping for a certain
 * kernel \param blockSize Block size of the kernel.
 *
 * \return The dynamic shared memory needed by a block.
 */
typedef size_t (CUDA_CB *CUoccupancyB2DSize)(int blockSize);

/**
 * If set, host memory is portable between CUDA contexts.
 * Flag for ::cuMemHostAlloc()
 */
#define CU_MEMHOSTALLOC_PORTABLE        0x01

/**
 * If set, host memory is mapped into CUDA address space and
 * ::cuMemHostGetDevicePointer() may be called on the host pointer.
 * Flag for ::cuMemHostAlloc()
 */
#define CU_MEMHOSTALLOC_DEVICEMAP       0x02

/**
 * If set, host memory is allocated as write-combined - fast to write,
 * faster to DMA, slow to read except via SSE4 streaming load instruction
 * (MOVNTDQA).
 * Flag for ::cuMemHostAlloc()
 */
#define CU_MEMHOSTALLOC_WRITECOMBINED   0x04

/**
 * If set, host memory is portable between CUDA contexts.
 * Flag for ::cuMemHostRegister()
 */
#define CU_MEMHOSTREGISTER_PORTABLE     0x01

/**
 * If set, host memory is mapped into CUDA address space and
 * ::cuMemHostGetDevicePointer() may be called on the host pointer.
 * Flag for ::cuMemHostRegister()
 */
#define CU_MEMHOSTREGISTER_DEVICEMAP    0x02

/**
 * If set, the passed memory pointer is treated as pointing to some
 * memory-mapped I/O space, e.g. belonging to a third-party PCIe device.
 * On Windows the flag is a no-op.
 * On Linux that memory is marked as non cache-coherent for the GPU and
 * is expected to be physically contiguous. It may return
 * ::CUDA_ERROR_NOT_PERMITTED if run as an unprivileged user,
 * ::CUDA_ERROR_NOT_SUPPORTED on older Linux kernel versions.
 * On all other platforms, it is not supported and ::CUDA_ERROR_NOT_SUPPORTED
 * is returned.
 * Flag for ::cuMemHostRegister()
 */
#define CU_MEMHOSTREGISTER_IOMEMORY     0x04

/**
* If set, the passed memory pointer is treated as pointing to memory that is
* considered read-only by the device.  On platforms without
* ::CU_DEVICE_ATTRIBUTE_PAGEABLE_MEMORY_ACCESS_USES_HOST_PAGE_TABLES, this flag is
* required in order to register memory mapped to the CPU as read-only.  Support
* for the use of this flag can be queried from the device attribute
* ::CU_DEVICE_ATTRIBUTE_READ_ONLY_HOST_REGISTER_SUPPORTED.  Using this flag with
* a current context associated with a device that does not have this attribute
* set will cause ::cuMemHostRegister to error with ::CUDA_ERROR_NOT_SUPPORTED.
*/
#define CU_MEMHOSTREGISTER_READ_ONLY    0x08

/**
 * 2D memory copy parameters
 */
typedef struct CUDA_MEMCPY2D_st {
    size_t srcXInBytes;         /**< Source X in bytes */
    size_t srcY;                /**< Source Y */

    CUmemorytype srcMemoryType; /**< Source memory type (host, device, array) */
    const void *srcHost;        /**< Source host pointer */
    CUdeviceptr srcDevice;      /**< Source device pointer */
    CUarray srcArray;           /**< Source array reference */
    size_t srcPitch;            /**< Source pitch (ignored when src is array) */

    size_t dstXInBytes;         /**< Destination X in bytes */
    size_t dstY;                /**< Destination Y */

    CUmemorytype dstMemoryType; /**< Destination memory type (host, device, array) */
    void *dstHost;              /**< Destination host pointer */
    CUdeviceptr dstDevice;      /**< Destination device pointer */
    CUarray dstArray;           /**< Destination array reference */
    size_t dstPitch;            /**< Destination pitch (ignored when dst is array) */

    size_t WidthInBytes;        /**< Width of 2D memory copy in bytes */
    size_t Height;              /**< Height of 2D memory copy */
} CUDA_MEMCPY2D_v2;
typedef CUDA_MEMCPY2D_v2 CUDA_MEMCPY2D;

/**
 * 3D memory copy parameters
 */
typedef struct CUDA_MEMCPY3D_st {
    size_t srcXInBytes;         /**< Source X in bytes */
    size_t srcY;                /**< Source Y */
    size_t srcZ;                /**< Source Z */
    size_t srcLOD;              /**< Source LOD */
    CUmemorytype srcMemoryType; /**< Source memory type (host, device, array) */
    const void *srcHost;        /**< Source host pointer */
    CUdeviceptr srcDevice;      /**< Source device pointer */
    CUarray srcArray;           /**< Source array reference */
    void *reserved0;            /**< Must be NULL */
    size_t srcPitch;            /**< Source pitch (ignored when src is array) */
    size_t srcHeight;           /**< Source height (ignored when src is array; may be 0 if Depth==1) */

    size_t dstXInBytes;         /**< Destination X in bytes */
    size_t dstY;                /**< Destination Y */
    size_t dstZ;                /**< Destination Z */
    size_t dstLOD;              /**< Destination LOD */
    CUmemorytype dstMemoryType; /**< Destination memory type (host, device, array) */
    void *dstHost;              /**< Destination host pointer */
    CUdeviceptr dstDevice;      /**< Destination device pointer */
    CUarray dstArray;           /**< Destination array reference */
    void *reserved1;            /**< Must be NULL */
    size_t dstPitch;            /**< Destination pitch (ignored when dst is array) */
    size_t dstHeight;           /**< Destination height (ignored when dst is array; may be 0 if Depth==1) */

    size_t WidthInBytes;        /**< Width of 3D memory copy in bytes */
    size_t Height;              /**< Height of 3D memory copy */
    size_t Depth;               /**< Depth of 3D memory copy */
} CUDA_MEMCPY3D_v2;
typedef CUDA_MEMCPY3D_v2 CUDA_MEMCPY3D;

/**
 * Memcpy node parameters
 */
typedef struct CUDA_MEMCPY_NODE_PARAMS_st {
    int flags;                 /**< Must be zero */
    int reserved;              /**< Must be zero */
    CUcontext copyCtx;         /**< Context on which to run the node */
    CUDA_MEMCPY3D copyParams;  /**< Parameters for the memory copy */
} CUDA_MEMCPY_NODE_PARAMS;

/**
 * 3D memory cross-context copy parameters
 */
typedef struct CUDA_MEMCPY3D_PEER_st {
    size_t srcXInBytes;         /**< Source X in bytes */
    size_t srcY;                /**< Source Y */
    size_t srcZ;                /**< Source Z */
    size_t srcLOD;              /**< Source LOD */
    CUmemorytype srcMemoryType; /**< Source memory type (host, device, array) */
    const void *srcHost;        /**< Source host pointer */
    CUdeviceptr srcDevice;      /**< Source device pointer */
    CUarray srcArray;           /**< Source array reference */
    CUcontext srcContext;       /**< Source context (ignored with srcMemoryType is ::CU_MEMORYTYPE_ARRAY) */
    size_t srcPitch;            /**< Source pitch (ignored when src is array) */
    size_t srcHeight;           /**< Source height (ignored when src is array; may be 0 if Depth==1) */

    size_t dstXInBytes;         /**< Destination X in bytes */
    size_t dstY;                /**< Destination Y */
    size_t dstZ;                /**< Destination Z */
    size_t dstLOD;              /**< Destination LOD */
    CUmemorytype dstMemoryType; /**< Destination memory type (host, device, array) */
    void *dstHost;              /**< Destination host pointer */
    CUdeviceptr dstDevice;      /**< Destination device pointer */
    CUarray dstArray;           /**< Destination array reference */
    CUcontext dstContext;       /**< Destination context (ignored with dstMemoryType is ::CU_MEMORYTYPE_ARRAY) */
    size_t dstPitch;            /**< Destination pitch (ignored when dst is array) */
    size_t dstHeight;           /**< Destination height (ignored when dst is array; may be 0 if Depth==1) */

    size_t WidthInBytes;        /**< Width of 3D memory copy in bytes */
    size_t Height;              /**< Height of 3D memory copy */
    size_t Depth;               /**< Depth of 3D memory copy */
} CUDA_MEMCPY3D_PEER_v1;
typedef CUDA_MEMCPY3D_PEER_v1 CUDA_MEMCPY3D_PEER;

/**
 * Array descriptor
 */
typedef struct CUDA_ARRAY_DESCRIPTOR_st
{
    size_t Width;             /**< Width of array */
    size_t Height;            /**< Height of array */

    CUarray_format Format;    /**< Array format */
    unsigned int NumChannels; /**< Channels per array element */
} CUDA_ARRAY_DESCRIPTOR_v2;
typedef CUDA_ARRAY_DESCRIPTOR_v2 CUDA_ARRAY_DESCRIPTOR;

/**
 * 3D array descriptor
 */
typedef struct CUDA_ARRAY3D_DESCRIPTOR_st
{
    size_t Width;             /**< Width of 3D array */
    size_t Height;            /**< Height of 3D array */
    size_t Depth;             /**< Depth of 3D array */

    CUarray_format Format;    /**< Array format */
    unsigned int NumChannels; /**< Channels per array element */
    unsigned int Flags;       /**< Flags */
} CUDA_ARRAY3D_DESCRIPTOR_v2;
typedef CUDA_ARRAY3D_DESCRIPTOR_v2 CUDA_ARRAY3D_DESCRIPTOR;

/**
 * Indicates that the layered sparse CUDA array or CUDA mipmapped array has a single mip tail region for all layers
 */
#define CU_ARRAY_SPARSE_PROPERTIES_SINGLE_MIPTAIL 0x1

/**
 * CUDA array sparse properties
 */
typedef struct CUDA_ARRAY_SPARSE_PROPERTIES_st {
    struct {
        unsigned int width;     /**< Width of sparse tile in elements */
        unsigned int height;    /**< Height of sparse tile in elements */
        unsigned int depth;     /**< Depth of sparse tile in elements */
    } tileExtent;

    /**
     * First mip level at which the mip tail begins.
     */
    unsigned int miptailFirstLevel;
    /**
     * Total size of the mip tail.
     */
    unsigned long long miptailSize;
    /**
     * Flags will either be zero or ::CU_ARRAY_SPARSE_PROPERTIES_SINGLE_MIPTAIL
     */
    unsigned int flags;
    unsigned int reserved[4];
} CUDA_ARRAY_SPARSE_PROPERTIES_v1;
typedef CUDA_ARRAY_SPARSE_PROPERTIES_v1 CUDA_ARRAY_SPARSE_PROPERTIES;


/**
 * CUDA array memory requirements
 */
typedef struct CUDA_ARRAY_MEMORY_REQUIREMENTS_st {
    size_t size;                /**< Total required memory size */
    size_t alignment;           /**< alignment requirement */
    unsigned int reserved[4];
} CUDA_ARRAY_MEMORY_REQUIREMENTS_v1;
typedef CUDA_ARRAY_MEMORY_REQUIREMENTS_v1 CUDA_ARRAY_MEMORY_REQUIREMENTS;


/**
 * CUDA Resource descriptor
 */
typedef struct CUDA_RESOURCE_DESC_st
{
    CUresourcetype resType;                   /**< Resource type */

    union {
        struct {
            CUarray hArray;                   /**< CUDA array */
        } array;
        struct {
            CUmipmappedArray hMipmappedArray; /**< CUDA mipmapped array */
        } mipmap;
        struct {
            CUdeviceptr devPtr;               /**< Device pointer */
            CUarray_format format;            /**< Array format */
            unsigned int numChannels;         /**< Channels per array element */
            size_t sizeInBytes;               /**< Size in bytes */
        } linear;
        struct {
            CUdeviceptr devPtr;               /**< Device pointer */
            CUarray_format format;            /**< Array format */
            unsigned int numChannels;         /**< Channels per array element */
            size_t width;                     /**< Width of the array in elements */
            size_t height;                    /**< Height of the array in elements */
            size_t pitchInBytes;              /**< Pitch between two rows in bytes */
        } pitch2D;
        struct {
            int reserved[32];
        } reserved;
    } res;

    unsigned int flags;                       /**< Flags (must be zero) */
} CUDA_RESOURCE_DESC_v1;
typedef CUDA_RESOURCE_DESC_v1 CUDA_RESOURCE_DESC;

/**
 * Texture descriptor
 */
typedef struct CUDA_TEXTURE_DESC_st {
    CUaddress_mode addressMode[3];  /**< Address modes */
    CUfilter_mode filterMode;       /**< Filter mode */
    unsigned int flags;             /**< Flags */
    unsigned int maxAnisotropy;     /**< Maximum anisotropy ratio */
    CUfilter_mode mipmapFilterMode; /**< Mipmap filter mode */
    float mipmapLevelBias;          /**< Mipmap level bias */
    float minMipmapLevelClamp;      /**< Mipmap minimum level clamp */
    float maxMipmapLevelClamp;      /**< Mipmap maximum level clamp */
    float borderColor[4];           /**< Border Color */
    int reserved[12];
} CUDA_TEXTURE_DESC_v1;
typedef CUDA_TEXTURE_DESC_v1 CUDA_TEXTURE_DESC;

/**
 * Resource view format
 */
typedef enum CUresourceViewFormat_enum
{
    CU_RES_VIEW_FORMAT_NONE          = 0x00, /**< No resource view format (use underlying resource format) */
    CU_RES_VIEW_FORMAT_UINT_1X8      = 0x01, /**< 1 channel unsigned 8-bit integers */
    CU_RES_VIEW_FORMAT_UINT_2X8      = 0x02, /**< 2 channel unsigned 8-bit integers */
    CU_RES_VIEW_FORMAT_UINT_4X8      = 0x03, /**< 4 channel unsigned 8-bit integers */
    CU_RES_VIEW_FORMAT_SINT_1X8      = 0x04, /**< 1 channel signed 8-bit integers */
    CU_RES_VIEW_FORMAT_SINT_2X8      = 0x05, /**< 2 channel signed 8-bit integers */
    CU_RES_VIEW_FORMAT_SINT_4X8      = 0x06, /**< 4 channel signed 8-bit integers */
    CU_RES_VIEW_FORMAT_UINT_1X16     = 0x07, /**< 1 channel unsigned 16-bit integers */
    CU_RES_VIEW_FORMAT_UINT_2X16     = 0x08, /**< 2 channel unsigned 16-bit integers */
    CU_RES_VIEW_FORMAT_UINT_4X16     = 0x09, /**< 4 channel unsigned 16-bit integers */
    CU_RES_VIEW_FORMAT_SINT_1X16     = 0x0a, /**< 1 channel signed 16-bit integers */
    CU_RES_VIEW_FORMAT_SINT_2X16     = 0x0b, /**< 2 channel signed 16-bit integers */
    CU_RES_VIEW_FORMAT_SINT_4X16     = 0x0c, /**< 4 channel signed 16-bit integers */
    CU_RES_VIEW_FORMAT_UINT_1X32     = 0x0d, /**< 1 channel unsigned 32-bit integers */
    CU_RES_VIEW_FORMAT_UINT_2X32     = 0x0e, /**< 2 channel unsigned 32-bit integers */
    CU_RES_VIEW_FORMAT_UINT_4X32     = 0x0f, /**< 4 channel unsigned 32-bit integers */
    CU_RES_VIEW_FORMAT_SINT_1X32     = 0x10, /**< 1 channel signed 32-bit integers */
    CU_RES_VIEW_FORMAT_SINT_2X32     = 0x11, /**< 2 channel signed 32-bit integers */
    CU_RES_VIEW_FORMAT_SINT_4X32     = 0x12, /**< 4 channel signed 32-bit integers */
    CU_RES_VIEW_FORMAT_FLOAT_1X16    = 0x13, /**< 1 channel 16-bit floating point */
    CU_RES_VIEW_FORMAT_FLOAT_2X16    = 0x14, /**< 2 channel 16-bit floating point */
    CU_RES_VIEW_FORMAT_FLOAT_4X16    = 0x15, /**< 4 channel 16-bit floating point */
    CU_RES_VIEW_FORMAT_FLOAT_1X32    = 0x16, /**< 1 channel 32-bit floating point */
    CU_RES_VIEW_FORMAT_FLOAT_2X32    = 0x17, /**< 2 channel 32-bit floating point */
    CU_RES_VIEW_FORMAT_FLOAT_4X32    = 0x18, /**< 4 channel 32-bit floating point */
    CU_RES_VIEW_FORMAT_UNSIGNED_BC1  = 0x19, /**< Block compressed 1 */
    CU_RES_VIEW_FORMAT_UNSIGNED_BC2  = 0x1a, /**< Block compressed 2 */
    CU_RES_VIEW_FORMAT_UNSIGNED_BC3  = 0x1b, /**< Block compressed 3 */
    CU_RES_VIEW_FORMAT_UNSIGNED_BC4  = 0x1c, /**< Block compressed 4 unsigned */
    CU_RES_VIEW_FORMAT_SIGNED_BC4    = 0x1d, /**< Block compressed 4 signed */
    CU_RES_VIEW_FORMAT_UNSIGNED_BC5  = 0x1e, /**< Block compressed 5 unsigned */
    CU_RES_VIEW_FORMAT_SIGNED_BC5    = 0x1f, /**< Block compressed 5 signed */
    CU_RES_VIEW_FORMAT_UNSIGNED_BC6H = 0x20, /**< Block compressed 6 unsigned half-float */
    CU_RES_VIEW_FORMAT_SIGNED_BC6H   = 0x21, /**< Block compressed 6 signed half-float */
    CU_RES_VIEW_FORMAT_UNSIGNED_BC7  = 0x22  /**< Block compressed 7 */
} CUresourceViewFormat;

/**
 * Resource view descriptor
 */
typedef struct CUDA_RESOURCE_VIEW_DESC_st
{
    CUresourceViewFormat format;   /**< Resource view format */
    size_t width;                  /**< Width of the resource view */
    size_t height;                 /**< Height of the resource view */
    size_t depth;                  /**< Depth of the resource view */
    unsigned int firstMipmapLevel; /**< First defined mipmap level */
    unsigned int lastMipmapLevel;  /**< Last defined mipmap level */
    unsigned int firstLayer;       /**< First layer index */
    unsigned int lastLayer;        /**< Last layer index */
    unsigned int reserved[16];
} CUDA_RESOURCE_VIEW_DESC_v1;
typedef CUDA_RESOURCE_VIEW_DESC_v1 CUDA_RESOURCE_VIEW_DESC;

/**
 * GPU Direct v3 tokens
 */
typedef struct CUDA_POINTER_ATTRIBUTE_P2P_TOKENS_st {
    unsigned long long p2pToken;
    unsigned int vaSpaceToken;
} CUDA_POINTER_ATTRIBUTE_P2P_TOKENS_v1;
typedef CUDA_POINTER_ATTRIBUTE_P2P_TOKENS_v1 CUDA_POINTER_ATTRIBUTE_P2P_TOKENS;

/**
* Access flags that specify the level of access the current context's device has
* on the memory referenced.
*/
typedef enum CUDA_POINTER_ATTRIBUTE_ACCESS_FLAGS_enum {
    CU_POINTER_ATTRIBUTE_ACCESS_FLAG_NONE      = 0x0,   /**< No access, meaning the device cannot access this memory at all, thus must be staged through accessible memory in order to complete certain operations */
    CU_POINTER_ATTRIBUTE_ACCESS_FLAG_READ      = 0x1,   /**< Read-only access, meaning writes to this memory are considered invalid accesses and thus return error in that case. */
    CU_POINTER_ATTRIBUTE_ACCESS_FLAG_READWRITE = 0x3    /**< Read-write access, the device has full read-write access to the memory */
} CUDA_POINTER_ATTRIBUTE_ACCESS_FLAGS;

/**
 * Kernel launch parameters
 */
typedef struct CUDA_LAUNCH_PARAMS_st {
    CUfunction function;         /**< Kernel to launch */
    unsigned int gridDimX;       /**< Width of grid in blocks */
    unsigned int gridDimY;       /**< Height of grid in blocks */
    unsigned int gridDimZ;       /**< Depth of grid in blocks */
    unsigned int blockDimX;      /**< X dimension of each thread block */
    unsigned int blockDimY;      /**< Y dimension of each thread block */
    unsigned int blockDimZ;      /**< Z dimension of each thread block */
    unsigned int sharedMemBytes; /**< Dynamic shared-memory size per thread block in bytes */
    CUstream hStream;            /**< Stream identifier */
    void **kernelParams;         /**< Array of pointers to kernel parameters */
} CUDA_LAUNCH_PARAMS_v1;
typedef CUDA_LAUNCH_PARAMS_v1 CUDA_LAUNCH_PARAMS;

/**
 * External memory handle types
 */
typedef enum CUexternalMemoryHandleType_enum {
    /**
     * Handle is an opaque file descriptor
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD          = 1,
    /**
     * Handle is an opaque shared NT handle
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_WIN32       = 2,
    /**
     * Handle is an opaque, globally shared handle
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_WIN32_KMT   = 3,
    /**
     * Handle is a D3D12 heap object
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D12_HEAP         = 4,
    /**
     * Handle is a D3D12 committed resource
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D12_RESOURCE     = 5,
    /**
     * Handle is a shared NT handle to a D3D11 resource
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D11_RESOURCE     = 6,
    /**
     * Handle is a globally shared handle to a D3D11 resource
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D11_RESOURCE_KMT = 7,
    /**
     * Handle is an NvSciBuf object
     */
    CU_EXTERNAL_MEMORY_HANDLE_TYPE_NVSCIBUF = 8
} CUexternalMemoryHandleType;

/**
 * Indicates that the external memory object is a dedicated resource
 */
#define CUDA_EXTERNAL_MEMORY_DEDICATED   0x1

/** When the \p flags parameter of ::CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS
 * contains this flag, it indicates that signaling an external semaphore object
 * should skip performing appropriate memory synchronization operations over all
 * the external memory objects that are imported as ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_NVSCIBUF,
 * which otherwise are performed by default to ensure data coherency with other
 * importers of the same NvSciBuf memory objects.
 */
#define CUDA_EXTERNAL_SEMAPHORE_SIGNAL_SKIP_NVSCIBUF_MEMSYNC 0x01

/** When the \p flags parameter of ::CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS
 * contains this flag, it indicates that waiting on an external semaphore object
 * should skip performing appropriate memory synchronization operations over all
 * the external memory objects that are imported as ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_NVSCIBUF,
 * which otherwise are performed by default to ensure data coherency with other
 * importers of the same NvSciBuf memory objects.
 */
#define CUDA_EXTERNAL_SEMAPHORE_WAIT_SKIP_NVSCIBUF_MEMSYNC 0x02

/**
 * When \p flags of ::cuDeviceGetNvSciSyncAttributes is set to this,
 * it indicates that application needs signaler specific NvSciSyncAttr
 * to be filled by ::cuDeviceGetNvSciSyncAttributes.
 */
#define CUDA_NVSCISYNC_ATTR_SIGNAL 0x1

/**
 * When \p flags of ::cuDeviceGetNvSciSyncAttributes is set to this,
 * it indicates that application needs waiter specific NvSciSyncAttr
 * to be filled by ::cuDeviceGetNvSciSyncAttributes.
 */
#define CUDA_NVSCISYNC_ATTR_WAIT 0x2
/**
 * External memory handle descriptor
 */
typedef struct CUDA_EXTERNAL_MEMORY_HANDLE_DESC_st {
    /**
     * Type of the handle
     */
    CUexternalMemoryHandleType type;
    union {
        /**
         * File descriptor referencing the memory object. Valid
         * when type is
         * ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD
         */
        int fd;
        /**
         * Win32 handle referencing the semaphore object. Valid when
         * type is one of the following:
         * - ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_WIN32
         * - ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_WIN32_KMT
         * - ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D12_HEAP
         * - ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D12_RESOURCE
         * - ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D11_RESOURCE
         * - ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D11_RESOURCE_KMT
         * Exactly one of 'handle' and 'name' must be non-NULL. If
         * type is one of the following:
         * ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_WIN32_KMT
         * ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_D3D11_RESOURCE_KMT
         * then 'name' must be NULL.
         */
        struct {
            /**
             * Valid NT handle. Must be NULL if 'name' is non-NULL
             */
            void *handle;
            /**
             * Name of a valid memory object.
             * Must be NULL if 'handle' is non-NULL.
             */
            const void *name;
        } win32;
        /**
         * A handle representing an NvSciBuf Object. Valid when type
         * is ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_NVSCIBUF
         */
        const void *nvSciBufObject;
    } handle;
    /**
     * Size of the memory allocation
     */
    unsigned long long size;
    /**
     * Flags must either be zero or ::CUDA_EXTERNAL_MEMORY_DEDICATED
     */
    unsigned int flags;
    unsigned int reserved[16];
} CUDA_EXTERNAL_MEMORY_HANDLE_DESC_v1;
typedef CUDA_EXTERNAL_MEMORY_HANDLE_DESC_v1 CUDA_EXTERNAL_MEMORY_HANDLE_DESC;

/**
 * External memory buffer descriptor
 */
typedef struct CUDA_EXTERNAL_MEMORY_BUFFER_DESC_st {
    /**
     * Offset into the memory object where the buffer's base is
     */
    unsigned long long offset;
    /**
     * Size of the buffer
     */
    unsigned long long size;
    /**
     * Flags reserved for future use. Must be zero.
     */
    unsigned int flags;
    unsigned int reserved[16];
} CUDA_EXTERNAL_MEMORY_BUFFER_DESC_v1;
typedef CUDA_EXTERNAL_MEMORY_BUFFER_DESC_v1 CUDA_EXTERNAL_MEMORY_BUFFER_DESC;

/**
 * External memory mipmap descriptor
 */
typedef struct CUDA_EXTERNAL_MEMORY_MIPMAPPED_ARRAY_DESC_st {
    /**
     * Offset into the memory object where the base level of the
     * mipmap chain is.
     */
    unsigned long long offset;
    /**
     * Format, dimension and type of base level of the mipmap chain
     */
    CUDA_ARRAY3D_DESCRIPTOR arrayDesc;
    /**
     * Total number of levels in the mipmap chain
     */
    unsigned int numLevels;
    unsigned int reserved[16];
} CUDA_EXTERNAL_MEMORY_MIPMAPPED_ARRAY_DESC_v1;
typedef CUDA_EXTERNAL_MEMORY_MIPMAPPED_ARRAY_DESC_v1 CUDA_EXTERNAL_MEMORY_MIPMAPPED_ARRAY_DESC;

/**
 * External semaphore handle types
 */
typedef enum CUexternalSemaphoreHandleType_enum {
    /**
     * Handle is an opaque file descriptor
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_OPAQUE_FD             = 1,
    /**
     * Handle is an opaque shared NT handle
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_OPAQUE_WIN32          = 2,
    /**
     * Handle is an opaque, globally shared handle
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_OPAQUE_WIN32_KMT      = 3,
    /**
     * Handle is a shared NT handle referencing a D3D12 fence object
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D12_FENCE           = 4,
    /**
     * Handle is a shared NT handle referencing a D3D11 fence object
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D11_FENCE           = 5,
    /**
     * Opaque handle to NvSciSync Object
	 */
	CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_NVSCISYNC             = 6,
    /**
     * Handle is a shared NT handle referencing a D3D11 keyed mutex object
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D11_KEYED_MUTEX     = 7,
    /**
     * Handle is a globally shared handle referencing a D3D11 keyed mutex object
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D11_KEYED_MUTEX_KMT = 8,
    /**
     * Handle is an opaque file descriptor referencing a timeline semaphore
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_TIMELINE_SEMAPHORE_FD = 9,
    /**
     * Handle is an opaque shared NT handle referencing a timeline semaphore
     */
    CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_TIMELINE_SEMAPHORE_WIN32 = 10
} CUexternalSemaphoreHandleType;

/**
 * External semaphore handle descriptor
 */
typedef struct CUDA_EXTERNAL_SEMAPHORE_HANDLE_DESC_st {
    /**
     * Type of the handle
     */
    CUexternalSemaphoreHandleType type;
    union {
        /**
         * File descriptor referencing the semaphore object. Valid
         * when type is one of the following:
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_OPAQUE_FD
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_TIMELINE_SEMAPHORE_FD
         */
        int fd;
        /**
         * Win32 handle referencing the semaphore object. Valid when
         * type is one of the following:
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_OPAQUE_WIN32
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_OPAQUE_WIN32_KMT
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D12_FENCE
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D11_FENCE
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D11_KEYED_MUTEX
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_TIMELINE_SEMAPHORE_WIN32
         * Exactly one of 'handle' and 'name' must be non-NULL. If
         * type is one of the following:
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_OPAQUE_WIN32_KMT
         * - ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_D3D11_KEYED_MUTEX_KMT
         * then 'name' must be NULL.
         */
        struct {
            /**
             * Valid NT handle. Must be NULL if 'name' is non-NULL
             */
            void *handle;
            /**
             * Name of a valid synchronization primitive.
             * Must be NULL if 'handle' is non-NULL.
             */
            const void *name;
        } win32;
        /**
         * Valid NvSciSyncObj. Must be non NULL
         */
        const void* nvSciSyncObj;
    } handle;
    /**
     * Flags reserved for the future. Must be zero.
     */
    unsigned int flags;
    unsigned int reserved[16];
} CUDA_EXTERNAL_SEMAPHORE_HANDLE_DESC_v1;

typedef CUDA_EXTERNAL_SEMAPHORE_HANDLE_DESC_v1 CUDA_EXTERNAL_SEMAPHORE_HANDLE_DESC;

/**
 * External semaphore signal parameters
 */
typedef struct CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS_st {
    struct {
        /**
         * Parameters for fence objects
         */
        struct {
            /**
             * Value of fence to be signaled
             */
            unsigned long long value;
        } fence;
        union {
            /**
             * Pointer to NvSciSyncFence. Valid if ::CUexternalSemaphoreHandleType
             * is of type ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_NVSCISYNC.
             */
            void *fence;
            unsigned long long reserved;
        } nvSciSync;
        /**
         * Parameters for keyed mutex objects
         */
        struct {
            /**
             * Value of key to release the mutex with
             */
            unsigned long long key;
        } keyedMutex;
        unsigned int reserved[12];
    } params;
    /**
     * Only when ::CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS is used to
     * signal a ::CUexternalSemaphore of type
     * ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_NVSCISYNC, the valid flag is
     * ::CUDA_EXTERNAL_SEMAPHORE_SIGNAL_SKIP_NVSCIBUF_MEMSYNC which indicates
     * that while signaling the ::CUexternalSemaphore, no memory synchronization
     * operations should be performed for any external memory object imported
     * as ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_NVSCIBUF.
     * For all other types of ::CUexternalSemaphore, flags must be zero.
     */
    unsigned int flags;
    unsigned int reserved[16];
} CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS_v1;

typedef CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS_v1 CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS;

/**
 * Semaphore signal node parameters
 */
typedef struct CUDA_EXT_SEM_SIGNAL_NODE_PARAMS_v2_st {
    CUexternalSemaphore* extSemArray;                         /**< Array of external semaphore handles. */
    const CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS* paramsArray; /**< Array of external semaphore signal parameters. */
    unsigned int numExtSems;                                  /**< Number of handles and parameters supplied in extSemArray and paramsArray. */
} CUDA_EXT_SEM_SIGNAL_NODE_PARAMS_v2;

/**
 * External semaphore wait parameters
 */
typedef struct CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS_st {
    struct {
        /**
         * Parameters for fence objects
         */
        struct {
            /**
             * Value of fence to be waited on
             */
            unsigned long long value;
        } fence;
        /**
         * Pointer to NvSciSyncFence. Valid if CUexternalSemaphoreHandleType
         * is of type CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_NVSCISYNC.
         */
        union {
            void *fence;
            unsigned long long reserved;
        } nvSciSync;
        /**
         * Parameters for keyed mutex objects
         */
        struct {
            /**
             * Value of key to acquire the mutex with
             */
            unsigned long long key;
            /**
             * Timeout in milliseconds to wait to acquire the mutex
             */
            unsigned int timeoutMs;
        } keyedMutex;
        unsigned int reserved[10];
    } params;
    /**
     * Only when ::CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS is used to wait on
     * a ::CUexternalSemaphore of type ::CU_EXTERNAL_SEMAPHORE_HANDLE_TYPE_NVSCISYNC,
     * the valid flag is ::CUDA_EXTERNAL_SEMAPHORE_WAIT_SKIP_NVSCIBUF_MEMSYNC
     * which indicates that while waiting for the ::CUexternalSemaphore, no memory
     * synchronization operations should be performed for any external memory
     * object imported as ::CU_EXTERNAL_MEMORY_HANDLE_TYPE_NVSCIBUF.
     * For all other types of ::CUexternalSemaphore, flags must be zero.
     */
    unsigned int flags;
    unsigned int reserved[16];
} CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS_v1;

typedef CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS_v1 CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS;

/**
 * Semaphore wait node parameters
 */
typedef struct CUDA_EXT_SEM_WAIT_NODE_PARAMS_v2_st {
    CUexternalSemaphore* extSemArray;                       /**< Array of external semaphore handles. */
    const CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS* paramsArray; /**< Array of external semaphore wait parameters. */
    unsigned int numExtSems;                                /**< Number of handles and parameters supplied in extSemArray and paramsArray. */
} CUDA_EXT_SEM_WAIT_NODE_PARAMS_v2;

/**
 * Semaphore signal node parameters
 */
typedef struct CUDA_EXT_SEM_SIGNAL_NODE_PARAMS_st {
    CUexternalSemaphore* extSemArray;                         /**< Array of external semaphore handles. */
    const CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS* paramsArray; /**< Array of external semaphore signal parameters. */
    unsigned int numExtSems;                                  /**< Number of handles and parameters supplied in extSemArray and paramsArray. */
} CUDA_EXT_SEM_SIGNAL_NODE_PARAMS_v1;

typedef CUDA_EXT_SEM_SIGNAL_NODE_PARAMS_v1 CUDA_EXT_SEM_SIGNAL_NODE_PARAMS;

/**
 * Semaphore wait node parameters
 */
typedef struct CUDA_EXT_SEM_WAIT_NODE_PARAMS_st {
    CUexternalSemaphore* extSemArray;                       /**< Array of external semaphore handles. */
    const CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS* paramsArray; /**< Array of external semaphore wait parameters. */
    unsigned int numExtSems;                                /**< Number of handles and parameters supplied in extSemArray and paramsArray. */
} CUDA_EXT_SEM_WAIT_NODE_PARAMS_v1;

typedef CUDA_EXT_SEM_WAIT_NODE_PARAMS_v1 CUDA_EXT_SEM_WAIT_NODE_PARAMS;

typedef unsigned long long CUmemGenericAllocationHandle_v1;
typedef CUmemGenericAllocationHandle_v1 CUmemGenericAllocationHandle;

/**
 * Flags for specifying particular handle types
 */
typedef enum CUmemAllocationHandleType_enum {
    CU_MEM_HANDLE_TYPE_NONE                  = 0x0,  /**< Does not allow any export mechanism. > */
    CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR = 0x1,  /**< Allows a file descriptor to be used for exporting. Permitted only on POSIX systems. (int) */
    CU_MEM_HANDLE_TYPE_WIN32                 = 0x2,  /**< Allows a Win32 NT handle to be used for exporting. (HANDLE) */
    CU_MEM_HANDLE_TYPE_WIN32_KMT             = 0x4,  /**< Allows a Win32 KMT handle to be used for exporting. (D3DKMT_HANDLE) */
    CU_MEM_HANDLE_TYPE_MAX                   = 0x7FFFFFFF
} CUmemAllocationHandleType;

/**
 * Specifies the memory protection flags for mapping.
 */
typedef enum CUmemAccess_flags_enum {
    CU_MEM_ACCESS_FLAGS_PROT_NONE        = 0x0,  /**< Default, make the address range not accessible */
    CU_MEM_ACCESS_FLAGS_PROT_READ        = 0x1,  /**< Make the address range read accessible */
    CU_MEM_ACCESS_FLAGS_PROT_READWRITE   = 0x3,  /**< Make the address range read-write accessible */
    CU_MEM_ACCESS_FLAGS_PROT_MAX         = 0x7FFFFFFF
} CUmemAccess_flags;


/**
 * Specifies the type of location
 */
typedef enum CUmemLocationType_enum {
    CU_MEM_LOCATION_TYPE_INVALID    = 0x0,
    CU_MEM_LOCATION_TYPE_DEVICE     = 0x1,  /**< Location is a device location, thus id is a device ordinal */
    CU_MEM_LOCATION_TYPE_HOST       = 0x2,   /**< Location is host, id is ignored */
    CU_MEM_LOCATION_TYPE_HOST_NUMA  = 0x3,  /**< Location is a host NUMA node, thus id is a host NUMA node id */
    CU_MEM_LOCATION_TYPE_HOST_NUMA_CURRENT = 0x4,  /**< Location is a host NUMA node of the current thread, id is ignored */
    CU_MEM_LOCATION_TYPE_MAX     = 0x7FFFFFFF
} CUmemLocationType;

/**
* Defines the allocation types available
*/
typedef enum CUmemAllocationType_enum {
    CU_MEM_ALLOCATION_TYPE_INVALID = 0x0,

    /** This allocation type is 'pinned', i.e. cannot migrate from its current
      * location while the application is actively using it
      */
    CU_MEM_ALLOCATION_TYPE_PINNED  = 0x1,
    CU_MEM_ALLOCATION_TYPE_MAX     = 0x7FFFFFFF
} CUmemAllocationType;

/**
* Flag for requesting different optimal and required granularities for an allocation.
*/
typedef enum CUmemAllocationGranularity_flags_enum {
    CU_MEM_ALLOC_GRANULARITY_MINIMUM     = 0x0,     /**< Minimum required granularity for allocation */
    CU_MEM_ALLOC_GRANULARITY_RECOMMENDED = 0x1      /**< Recommended granularity for allocation for best performance */
} CUmemAllocationGranularity_flags;


/**
* Specifies the handle type for address range
*/
typedef enum CUmemRangeHandleType_enum
{
    CU_MEM_RANGE_HANDLE_TYPE_DMA_BUF_FD = 0x1,
    CU_MEM_RANGE_HANDLE_TYPE_MAX        = 0x7FFFFFFF
} CUmemRangeHandleType;


/**
 * Sparse subresource types
 */
typedef enum CUarraySparseSubresourceType_enum {
    CU_ARRAY_SPARSE_SUBRESOURCE_TYPE_SPARSE_LEVEL = 0,
    CU_ARRAY_SPARSE_SUBRESOURCE_TYPE_MIPTAIL = 1
} CUarraySparseSubresourceType;

/**
 * Memory operation types
 */
typedef enum CUmemOperationType_enum {
    CU_MEM_OPERATION_TYPE_MAP = 1,
    CU_MEM_OPERATION_TYPE_UNMAP = 2
} CUmemOperationType;

/**
 * Memory handle types
 */
typedef enum CUmemHandleType_enum {
    CU_MEM_HANDLE_TYPE_GENERIC = 0
} CUmemHandleType;

/**
 * Specifies the CUDA array or CUDA mipmapped array memory mapping information
 */
typedef struct CUarrayMapInfo_st {    
    CUresourcetype resourceType;                    /**< Resource type */

    union {
        CUmipmappedArray mipmap;
        CUarray array;
    } resource;

    CUarraySparseSubresourceType subresourceType;   /**< Sparse subresource type */

    union {
        struct {
            unsigned int level;                     /**< For CUDA mipmapped arrays must a valid mipmap level. For CUDA arrays must be zero */            
            unsigned int layer;                     /**< For CUDA layered arrays must be a valid layer index. Otherwise, must be zero */
            unsigned int offsetX;                   /**< Starting X offset in elements */
            unsigned int offsetY;                   /**< Starting Y offset in elements */
            unsigned int offsetZ;                   /**< Starting Z offset in elements */            
            unsigned int extentWidth;               /**< Width in elements */
            unsigned int extentHeight;              /**< Height in elements */
            unsigned int extentDepth;               /**< Depth in elements */
        } sparseLevel;
        struct {
            unsigned int layer;                     /**< For CUDA layered arrays must be a valid layer index. Otherwise, must be zero */
            unsigned long long offset;              /**< Offset within mip tail */
            unsigned long long size;                /**< Extent in bytes */
        } miptail;
    } subresource;

    CUmemOperationType memOperationType;            /**< Memory operation type */
    CUmemHandleType memHandleType;                  /**< Memory handle type */

    union {
        CUmemGenericAllocationHandle memHandle;
    } memHandle;

    unsigned long long offset;                      /**< Offset within the memory */
    unsigned int deviceBitMask;                     /**< Device ordinal bit mask */
    unsigned int flags;                             /**< flags for future use, must be zero now. */
    unsigned int reserved[2];                       /**< Reserved for future use, must be zero now. */
} CUarrayMapInfo_v1;
typedef CUarrayMapInfo_v1 CUarrayMapInfo;

/**
 * Specifies a memory location.
 */
typedef struct CUmemLocation_st {
    CUmemLocationType type; /**< Specifies the location type, which modifies the meaning of id. */
    int id;                 /**< identifier for a given this location's ::CUmemLocationType. */
} CUmemLocation_v1;
typedef CUmemLocation_v1 CUmemLocation;

/**
 * Specifies compression attribute for an allocation.
 */
typedef enum CUmemAllocationCompType_enum {
    CU_MEM_ALLOCATION_COMP_NONE = 0x0, /**< Allocating non-compressible memory */
    CU_MEM_ALLOCATION_COMP_GENERIC = 0x1 /**< Allocating  compressible memory */
} CUmemAllocationCompType;

/**
 * This flag if set indicates that the memory will be used as a tile pool.
 */
#define CU_MEM_CREATE_USAGE_TILE_POOL    0x1

/**
* Specifies the allocation properties for a allocation.
*/
typedef struct CUmemAllocationProp_st {
    /** Allocation type */
    CUmemAllocationType type;
    /** requested ::CUmemAllocationHandleType */
    CUmemAllocationHandleType requestedHandleTypes;
    /** Location of allocation */
    CUmemLocation location;
    /**
     * Windows-specific POBJECT_ATTRIBUTES required when
     * ::CU_MEM_HANDLE_TYPE_WIN32 is specified.  This object atributes structure
     * includes security attributes that define
     * the scope of which exported allocations may be tranferred to other
     * processes.  In all other cases, this field is required to be zero.
     */
    void *win32HandleMetaData;

    struct {
         /**
         * Allocation hint for requesting compressible memory.
         * On devices that support Compute Data Compression, compressible
         * memory can be used to accelerate accesses to data with unstructured
         * sparsity and other compressible data patterns. Applications are 
         * expected to query allocation property of the handle obtained with 
         * ::cuMemCreate using ::cuMemGetAllocationPropertiesFromHandle to 
         * validate if the obtained allocation is compressible or not. Note that 
         * compressed memory may not be mappable on all devices.
         */
         unsigned char compressionType;
         unsigned char gpuDirectRDMACapable;
         /** Bitmask indicating intended usage for this allocation */
         unsigned short usage;
         unsigned char reserved[4];
    } allocFlags;
} CUmemAllocationProp_v1;
typedef CUmemAllocationProp_v1 CUmemAllocationProp;

/**
 * Memory access descriptor
 */
typedef struct CUmemAccessDesc_st {
    CUmemLocation location;        /**< Location on which the request is to change it's accessibility */
    CUmemAccess_flags flags;       /**< ::CUmemProt accessibility flags to set on the request */
} CUmemAccessDesc_v1;
typedef CUmemAccessDesc_v1 CUmemAccessDesc;

typedef enum CUgraphExecUpdateResult_enum {
    CU_GRAPH_EXEC_UPDATE_SUCCESS                     = 0x0, /**< The update succeeded */
    CU_GRAPH_EXEC_UPDATE_ERROR                       = 0x1, /**< The update failed for an unexpected reason which is described in the return value of the function */
    CU_GRAPH_EXEC_UPDATE_ERROR_TOPOLOGY_CHANGED      = 0x2, /**< The update failed because the topology changed */
    CU_GRAPH_EXEC_UPDATE_ERROR_NODE_TYPE_CHANGED     = 0x3, /**< The update failed because a node type changed */
    CU_GRAPH_EXEC_UPDATE_ERROR_FUNCTION_CHANGED      = 0x4, /**< The update failed because the function of a kernel node changed (CUDA driver < 11.2) */
    CU_GRAPH_EXEC_UPDATE_ERROR_PARAMETERS_CHANGED    = 0x5, /**< The update failed because the parameters changed in a way that is not supported */
    CU_GRAPH_EXEC_UPDATE_ERROR_NOT_SUPPORTED         = 0x6, /**< The update failed because something about the node is not supported */
    CU_GRAPH_EXEC_UPDATE_ERROR_UNSUPPORTED_FUNCTION_CHANGE = 0x7, /**< The update failed because the function of a kernel node changed in an unsupported way */
    CU_GRAPH_EXEC_UPDATE_ERROR_ATTRIBUTES_CHANGED    = 0x8  /**< The update failed because the node attributes changed in a way that is not supported */
} CUgraphExecUpdateResult;

/**
 * CUDA memory pool attributes
 */
typedef enum CUmemPool_attribute_enum {
    /**
     * (value type = int)
     * Allow cuMemAllocAsync to use memory asynchronously freed
     * in another streams as long as a stream ordering dependency
     * of the allocating stream on the free action exists.
     * Cuda events and null stream interactions can create the required
     * stream ordered dependencies. (default enabled)
     */
    CU_MEMPOOL_ATTR_REUSE_FOLLOW_EVENT_DEPENDENCIES = 1,

    /**
     * (value type = int)
     * Allow reuse of already completed frees when there is no dependency
     * between the free and allocation. (default enabled)
     */
    CU_MEMPOOL_ATTR_REUSE_ALLOW_OPPORTUNISTIC,
    /**
     * (value type = int)
     * Allow cuMemAllocAsync to insert new stream dependencies
     * in order to establish the stream ordering required to reuse
     * a piece of memory released by cuFreeAsync (default enabled).
     */
    CU_MEMPOOL_ATTR_REUSE_ALLOW_INTERNAL_DEPENDENCIES,
    /**
     * (value type = cuuint64_t)
     * Amount of reserved memory in bytes to hold onto before trying
     * to release memory back to the OS. When more than the release
     * threshold bytes of memory are held by the memory pool, the
     * allocator will try to release memory back to the OS on the
     * next call to stream, event or context synchronize. (default 0)
     */
    CU_MEMPOOL_ATTR_RELEASE_THRESHOLD,

    /**
     * (value type = cuuint64_t)
     * Amount of backing memory currently allocated for the mempool.
     */
    CU_MEMPOOL_ATTR_RESERVED_MEM_CURRENT,

    /**
     * (value type = cuuint64_t)
     * High watermark of backing memory allocated for the mempool since the
     * last time it was reset. High watermark can only be reset to zero.
     */
    CU_MEMPOOL_ATTR_RESERVED_MEM_HIGH,

    /**
     * (value type = cuuint64_t)
     * Amount of memory from the pool that is currently in use by the application.
     */
    CU_MEMPOOL_ATTR_USED_MEM_CURRENT,

    /**
     * (value type = cuuint64_t)
     * High watermark of the amount of memory from the pool that was in use by the application since
     * the last time it was reset. High watermark can only be reset to zero.
     */
    CU_MEMPOOL_ATTR_USED_MEM_HIGH
} CUmemPool_attribute;


/**
 * Specifies the properties of allocations made from the pool.
 */
typedef struct CUmemPoolProps_st {
    CUmemAllocationType allocType;         /**< Allocation type. Currently must be specified as CU_MEM_ALLOCATION_TYPE_PINNED */
    CUmemAllocationHandleType handleTypes; /**< Handle types that will be supported by allocations from the pool. */
    CUmemLocation location;                /**< Location where allocations should reside. */
    /**
     * Windows-specific LPSECURITYATTRIBUTES required when
     * ::CU_MEM_HANDLE_TYPE_WIN32 is specified.  This security attribute defines
     * the scope of which exported allocations may be tranferred to other
     * processes.  In all other cases, this field is required to be zero.
     */
    void *win32SecurityAttributes;
    size_t maxSize;             /**< Maximum pool size. When set to 0, defaults to a system dependent value. */
    unsigned char reserved[56]; /**< reserved for future use, must be 0 */
} CUmemPoolProps_v1;
typedef CUmemPoolProps_v1 CUmemPoolProps;

/**
 * Memory allocation node parameters
 */
typedef struct CUDA_MEM_ALLOC_NODE_PARAMS_v2_st {
    /**
    * in: location where the allocation should reside (specified in ::location).
    * ::handleTypes must be ::CU_MEM_HANDLE_TYPE_NONE. IPC is not supported.
    */
    CUmemPoolProps poolProps;
    const CUmemAccessDesc *accessDescs; /**< in: array of memory access descriptors. Used to describe peer GPU access */
    size_t accessDescCount; /**< in: number of memory access descriptors.  Must not exceed the number of GPUs. */
    size_t bytesize; /**< in: size in bytes of the requested allocation */
    CUdeviceptr dptr; /**< out: address of the allocation returned by CUDA */
} CUDA_MEM_ALLOC_NODE_PARAMS_v2;

typedef struct CUgraphNodeParams_st {
    CUgraphNodeType type;
    int reserved0[3];

    union {
        long long                             reserved1[29];
        CUDA_KERNEL_NODE_PARAMS_v3            kernel;
        CUDA_MEMCPY_NODE_PARAMS               memcpy;
        CUDA_MEMSET_NODE_PARAMS_v2            memset;
        CUDA_HOST_NODE_PARAMS_v2              host;
        CUDA_CHILD_GRAPH_NODE_PARAMS          graph;
        CUDA_EVENT_WAIT_NODE_PARAMS           eventWait;
        CUDA_EVENT_RECORD_NODE_PARAMS         eventRecord;
        CUDA_EXT_SEM_SIGNAL_NODE_PARAMS_v2    extSemSignal;
        CUDA_EXT_SEM_WAIT_NODE_PARAMS_v2      extSemWait;
        CUDA_MEM_ALLOC_NODE_PARAMS_v2         alloc;
        CUDA_MEM_FREE_NODE_PARAMS             free;
        CUDA_BATCH_MEM_OP_NODE_PARAMS_v2      memOp;
    };

    long long reserved2;
} CUgraphNodeParams;

/**
 * Opaque data for exporting a pool allocation
 */
typedef struct CUmemPoolPtrExportData_st {
    unsigned char reserved[64];
} CUmemPoolPtrExportData_v1;
typedef CUmemPoolPtrExportData_v1 CUmemPoolPtrExportData;

/**
 * Memory allocation node parameters
 */
typedef struct CUDA_MEM_ALLOC_NODE_PARAMS_st {
    /**
    * in: location where the allocation should reside (specified in ::location).
    * ::handleTypes must be ::CU_MEM_HANDLE_TYPE_NONE. IPC is not supported.
    */
    CUmemPoolProps poolProps;
    const CUmemAccessDesc *accessDescs; /**< in: array of memory access descriptors. Used to describe peer GPU access */
    size_t accessDescCount; /**< in: number of memory access descriptors.  Must not exceed the number of GPUs. */
    size_t bytesize; /**< in: size in bytes of the requested allocation */
    CUdeviceptr dptr; /**< out: address of the allocation returned by CUDA */
} CUDA_MEM_ALLOC_NODE_PARAMS;

typedef enum CUgraphMem_attribute_enum {
    /**
     * Parameters for fence objects
     * (value type = cuuint64_t)
     * Amount of memory, in bytes, currently associated with graphs
     */
    CU_GRAPH_MEM_ATTR_USED_MEM_CURRENT,

    /**
     * (value type = cuuint64_t)
     * High watermark of memory, in bytes, associated with graphs since the
     * last time it was reset.  High watermark can only be reset to zero.
     */
    CU_GRAPH_MEM_ATTR_USED_MEM_HIGH,

    /**
     * (value type = cuuint64_t)
     * Amount of memory, in bytes, currently allocated for use by
     * the CUDA graphs asynchronous allocator.
     */
    CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT,

    /**
     * (value type = cuuint64_t)
     * High watermark of memory, in bytes, currently allocated for use by
     * the CUDA graphs asynchronous allocator.
     */
    CU_GRAPH_MEM_ATTR_RESERVED_MEM_HIGH
} CUgraphMem_attribute;

/**
 * If set, each kernel launched as part of ::cuLaunchCooperativeKernelMultiDevice only
 * waits for prior work in the stream corresponding to that GPU to complete before the
 * kernel begins execution.
 */
#define CUDA_COOPERATIVE_LAUNCH_MULTI_DEVICE_NO_PRE_LAUNCH_SYNC   0x01

/**
 * If set, any subsequent work pushed in a stream that participated in a call to
 * ::cuLaunchCooperativeKernelMultiDevice will only wait for the kernel launched on
 * the GPU corresponding to that stream to complete before it begins execution.
 */
#define CUDA_COOPERATIVE_LAUNCH_MULTI_DEVICE_NO_POST_LAUNCH_SYNC  0x02

/**
 * If set, the CUDA array is a collection of layers, where each layer is either a 1D
 * or a 2D array and the Depth member of CUDA_ARRAY3D_DESCRIPTOR specifies the number
 * of layers, not the depth of a 3D array.
 */
#define CUDA_ARRAY3D_LAYERED        0x01

/**
 * Deprecated, use CUDA_ARRAY3D_LAYERED
 */
#define CUDA_ARRAY3D_2DARRAY        0x01

/**
 * This flag must be set in order to bind a surface reference
 * to the CUDA array
 */
#define CUDA_ARRAY3D_SURFACE_LDST   0x02

/**
 * If set, the CUDA array is a collection of six 2D arrays, representing faces of a cube. The
 * width of such a CUDA array must be equal to its height, and Depth must be six.
 * If ::CUDA_ARRAY3D_LAYERED flag is also set, then the CUDA array is a collection of cubemaps
 * and Depth must be a multiple of six.
 */
#define CUDA_ARRAY3D_CUBEMAP        0x04

/**
 * This flag must be set in order to perform texture gather operations
 * on a CUDA array.
 */
#define CUDA_ARRAY3D_TEXTURE_GATHER 0x08

/**
 * This flag if set indicates that the CUDA
 * array is a DEPTH_TEXTURE.
 */
#define CUDA_ARRAY3D_DEPTH_TEXTURE 0x10

/**
 * This flag indicates that the CUDA array may be bound as a color target
 * in an external graphics API
 */
#define CUDA_ARRAY3D_COLOR_ATTACHMENT 0x20

/**
 * This flag if set indicates that the CUDA array or CUDA mipmapped array
 * is a sparse CUDA array or CUDA mipmapped array respectively
 */
#define CUDA_ARRAY3D_SPARSE 0x40



/**
 * This flag if set indicates that the CUDA array or CUDA mipmapped array
 * will allow deferred memory mapping
 */
#define CUDA_ARRAY3D_DEFERRED_MAPPING 0x80

/**
 * Override the texref format with a format inferred from the array.
 * Flag for ::cuTexRefSetArray()
 */
#define CU_TRSA_OVERRIDE_FORMAT 0x01

/**
 * Read the texture as integers rather than promoting the values to floats
 * in the range [0,1].
 * Flag for ::cuTexRefSetFlags() and ::cuTexObjectCreate()
 */
#define CU_TRSF_READ_AS_INTEGER         0x01

/**
 * Use normalized texture coordinates in the range [0,1) instead of [0,dim).
 * Flag for ::cuTexRefSetFlags() and ::cuTexObjectCreate()
 */
#define CU_TRSF_NORMALIZED_COORDINATES  0x02

/**
 * Perform sRGB->linear conversion during texture read.
 * Flag for ::cuTexRefSetFlags() and ::cuTexObjectCreate()
 */
#define CU_TRSF_SRGB  0x10

 /**
  * Disable any trilinear filtering optimizations.
  * Flag for ::cuTexRefSetFlags() and ::cuTexObjectCreate()
  */
#define CU_TRSF_DISABLE_TRILINEAR_OPTIMIZATION  0x20

/**
 * Enable seamless cube map filtering.
 * Flag for ::cuTexObjectCreate()
 */
#define CU_TRSF_SEAMLESS_CUBEMAP  0x40

/**
 * C++ compile time constant for CU_LAUNCH_PARAM_END
 */
#define CU_LAUNCH_PARAM_END_AS_INT     0x00

/**
 * End of array terminator for the \p extra parameter to
 * ::cuLaunchKernel
 */
#define CU_LAUNCH_PARAM_END            ((void*)CU_LAUNCH_PARAM_END_AS_INT)


/**
 * C++ compile time constant for CU_LAUNCH_PARAM_BUFFER_POINTER
 */
#define CU_LAUNCH_PARAM_BUFFER_POINTER_AS_INT 0x01

/**
 * Indicator that the next value in the \p extra parameter to
 * ::cuLaunchKernel will be a pointer to a buffer containing all kernel
 * parameters used for launching kernel \p f.  This buffer needs to
 * honor all alignment/padding requirements of the individual parameters.
 * If ::CU_LAUNCH_PARAM_BUFFER_SIZE is not also specified in the
 * \p extra array, then ::CU_LAUNCH_PARAM_BUFFER_POINTER will have no
 * effect.
 */
#define CU_LAUNCH_PARAM_BUFFER_POINTER        ((void*)CU_LAUNCH_PARAM_BUFFER_POINTER_AS_INT)

/**
 * C++ compile time constant for CU_LAUNCH_PARAM_BUFFER_SIZE
 */
#define CU_LAUNCH_PARAM_BUFFER_SIZE_AS_INT 0x02

/**
 * Indicator that the next value in the \p extra parameter to
 * ::cuLaunchKernel will be a pointer to a size_t which contains the
 * size of the buffer specified with ::CU_LAUNCH_PARAM_BUFFER_POINTER.
 * It is required that ::CU_LAUNCH_PARAM_BUFFER_POINTER also be specified
 * in the \p extra array if the value associated with
 * ::CU_LAUNCH_PARAM_BUFFER_SIZE is not zero.
 */
#define CU_LAUNCH_PARAM_BUFFER_SIZE        ((void*)CU_LAUNCH_PARAM_BUFFER_SIZE_AS_INT)

/**
 * For texture references loaded into the module, use default texunit from
 * texture reference.
 */
#define CU_PARAM_TR_DEFAULT -1

/**
 * Device that represents the CPU
 */
#define CU_DEVICE_CPU               ((CUdevice)-1)

/**
 * Device that represents an invalid device
 */
#define CU_DEVICE_INVALID           ((CUdevice)-2)

/**
 * Bitmasks for ::CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_FLUSH_WRITES_OPTIONS
 */
typedef enum CUflushGPUDirectRDMAWritesOptions_enum {
    CU_FLUSH_GPU_DIRECT_RDMA_WRITES_OPTION_HOST   = 1<<0, /**< ::cuFlushGPUDirectRDMAWrites() and its CUDA Runtime API counterpart are supported on the device. */
    CU_FLUSH_GPU_DIRECT_RDMA_WRITES_OPTION_MEMOPS = 1<<1  /**< The ::CU_STREAM_WAIT_VALUE_FLUSH flag and the ::CU_STREAM_MEM_OP_FLUSH_REMOTE_WRITES MemOp are supported on the device. */
} CUflushGPUDirectRDMAWritesOptions;

/**
 * Platform native ordering for GPUDirect RDMA writes
 */
typedef enum CUGPUDirectRDMAWritesOrdering_enum {
    CU_GPU_DIRECT_RDMA_WRITES_ORDERING_NONE        = 0,   /**< The device does not natively support ordering of remote writes. ::cuFlushGPUDirectRDMAWrites() can be leveraged if supported. */
    CU_GPU_DIRECT_RDMA_WRITES_ORDERING_OWNER       = 100, /**< Natively, the device can consistently consume remote writes, although other CUDA devices may not. */
    CU_GPU_DIRECT_RDMA_WRITES_ORDERING_ALL_DEVICES = 200  /**< Any CUDA device in the system can consistently consume remote writes to this device. */
} CUGPUDirectRDMAWritesOrdering;

/**
 * The scopes for ::cuFlushGPUDirectRDMAWrites
 */
typedef enum CUflushGPUDirectRDMAWritesScope_enum {
    CU_FLUSH_GPU_DIRECT_RDMA_WRITES_TO_OWNER       = 100, /**< Blocks until remote writes are visible to the CUDA device context owning the data. */
    CU_FLUSH_GPU_DIRECT_RDMA_WRITES_TO_ALL_DEVICES = 200  /**< Blocks until remote writes are visible to all CUDA device contexts. */
} CUflushGPUDirectRDMAWritesScope;

/**
 * The targets for ::cuFlushGPUDirectRDMAWrites
 */
typedef enum CUflushGPUDirectRDMAWritesTarget_enum {
    CU_FLUSH_GPU_DIRECT_RDMA_WRITES_TARGET_CURRENT_CTX = 0 /**< Sets the target for ::cuFlushGPUDirectRDMAWrites() to the currently active CUDA device context. */
} CUflushGPUDirectRDMAWritesTarget;

/**
 * The additional write options for ::cuGraphDebugDotPrint
 */
typedef enum CUgraphDebugDot_flags_enum {
    CU_GRAPH_DEBUG_DOT_FLAGS_VERBOSE                        = 1<<0,  /**< Output all debug data as if every debug flag is enabled */
    CU_GRAPH_DEBUG_DOT_FLAGS_RUNTIME_TYPES                  = 1<<1,  /**< Use CUDA Runtime structures for output */
    CU_GRAPH_DEBUG_DOT_FLAGS_KERNEL_NODE_PARAMS             = 1<<2,  /**< Adds CUDA_KERNEL_NODE_PARAMS values to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_MEMCPY_NODE_PARAMS             = 1<<3,  /**< Adds CUDA_MEMCPY3D values to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_MEMSET_NODE_PARAMS             = 1<<4,  /**< Adds CUDA_MEMSET_NODE_PARAMS values to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_HOST_NODE_PARAMS               = 1<<5,  /**< Adds CUDA_HOST_NODE_PARAMS values to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_EVENT_NODE_PARAMS              = 1<<6,  /**< Adds CUevent handle from record and wait nodes to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_EXT_SEMAS_SIGNAL_NODE_PARAMS   = 1<<7,  /**< Adds CUDA_EXT_SEM_SIGNAL_NODE_PARAMS values to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_EXT_SEMAS_WAIT_NODE_PARAMS     = 1<<8,  /**< Adds CUDA_EXT_SEM_WAIT_NODE_PARAMS values to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_KERNEL_NODE_ATTRIBUTES         = 1<<9,  /**< Adds CUkernelNodeAttrValue values to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_HANDLES                        = 1<<10, /**< Adds node handles and every kernel function handle to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_MEM_ALLOC_NODE_PARAMS          = 1<<11, /**< Adds memory alloc node parameters to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_MEM_FREE_NODE_PARAMS           = 1<<12, /**< Adds memory free node parameters to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_BATCH_MEM_OP_NODE_PARAMS       = 1<<13, /**< Adds batch mem op node parameters to output */
    CU_GRAPH_DEBUG_DOT_FLAGS_EXTRA_TOPO_INFO                = 1<<14, /**< Adds edge numbering information */
    CU_GRAPH_DEBUG_DOT_FLAGS_CONDITIONAL_NODE_PARAMS        = 1<<15  /**< Adds conditional node parameters to output */
} CUgraphDebugDot_flags;

/**
 * Flags for user objects for graphs
 */
typedef enum CUuserObject_flags_enum {
    CU_USER_OBJECT_NO_DESTRUCTOR_SYNC = 1  /**< Indicates the destructor execution is not synchronized by any CUDA handle. */
} CUuserObject_flags;

/**
 * Flags for retaining user object references for graphs
 */
typedef enum CUuserObjectRetain_flags_enum {
    CU_GRAPH_USER_OBJECT_MOVE = 1  /**< Transfer references from the caller rather than creating new references. */
} CUuserObjectRetain_flags;

/**
 * Flags for instantiating a graph
 */
typedef enum CUgraphInstantiate_flags_enum {
    CUDA_GRAPH_INSTANTIATE_FLAG_AUTO_FREE_ON_LAUNCH  = 1 /**< Automatically free memory allocated in a graph before relaunching. */
  , CUDA_GRAPH_INSTANTIATE_FLAG_UPLOAD               = 2 /**< Automatically upload the graph after instantiation. Only supported by
                                                              ::cuGraphInstantiateWithParams.  The upload will be performed using the
                                                              stream provided in \p instantiateParams. */
  , CUDA_GRAPH_INSTANTIATE_FLAG_DEVICE_LAUNCH        = 4 /**< Instantiate the graph to be launchable from the device. This flag can only
                                                              be used on platforms which support unified addressing. This flag cannot be
                                                              used in conjunction with CUDA_GRAPH_INSTANTIATE_FLAG_AUTO_FREE_ON_LAUNCH. */
  , CUDA_GRAPH_INSTANTIATE_FLAG_USE_NODE_PRIORITY    = 8 /**< Run the graph using the per-node priority attributes rather than the
                                                              priority of the stream it is launched into. */
} CUgraphInstantiate_flags;
/**
 * CUDA Lazy Loading status
 */
typedef enum CUmoduleLoadingMode_enum {
    CU_MODULE_EAGER_LOADING = 0x1, /**< Lazy Kernel Loading is not enabled */
    CU_MODULE_LAZY_LOADING  = 0x2, /**< Lazy Kernel Loading is enabled */
} CUmoduleLoadingMode;

/**
 * CUDA devices corresponding to an OpenGL device
 */
typedef enum CUGLDeviceList_enum {
  CU_GL_DEVICE_LIST_ALL           = 0x01, /**< The CUDA devices for all GPUs used by the
                                               current OpenGL context */
  CU_GL_DEVICE_LIST_CURRENT_FRAME = 0x02, /**< The CUDA devices for the GPUs used by the current OpenGL
                                               context in its currently rendering frame */
  CU_GL_DEVICE_LIST_NEXT_FRAME    = 0x03, /**< The CUDA devices for the GPUs to be used by the current OpenGL
                                               context in the next frame */
} CUGLDeviceList;

/**
 *  Profiler Output Modes
 */
/*DEVICE_BUILTIN*/
typedef enum CUoutput_mode_enum {
  CU_OUT_KEY_VALUE_PAIR = 0x00, /**< Output mode Key-Value pair format. */
  CU_OUT_CSV = 0x01 /**< Output mode Comma separated values format. */
} CUoutput_mode;

typedef const struct CUDBGAPI_st *CUDBGAPI;

typedef enum {
  CUDBG_SUCCESS = 0x0000,       /* Successful execution */
  CUDBG_ERROR_UNKNOWN = 0x0001, /* Error type not listed below */
  CUDBG_ERROR_BUFFER_TOO_SMALL =
      0x0002, /* Cannot copy all the queried data into the buffer argument */
  CUDBG_ERROR_UNKNOWN_FUNCTION =
      0x0003, /* Function cannot be found in the CUDA kernel */
  CUDBG_ERROR_INVALID_ARGS =
      0x0004, /* Wrong use of arguments (NULL pointer, illegal value,...) */
  CUDBG_ERROR_UNINITIALIZED =
      0x0005, /* Debugger API has not yet been properly initialized */
  CUDBG_ERROR_INVALID_COORDINATES =
      0x0006, /* Invalid block or thread coordinates were provided */
  CUDBG_ERROR_INVALID_MEMORY_SEGMENT =
      0x0007, /* Invalid memory segment requested (read/write) */
  CUDBG_ERROR_INVALID_MEMORY_ACCESS = 0x0008, /* Requested address (+size) is
                                                 not within proper segment
                                                 boundaries */
  CUDBG_ERROR_MEMORY_MAPPING_FAILED =
      0x0009,                    /* Memory is not mapped and cannot be mapped */
  CUDBG_ERROR_INTERNAL = 0x000a, /* A debugger internal error occurred */
  CUDBG_ERROR_INVALID_DEVICE = 0x000b,   /* Specified device cannot be found */
  CUDBG_ERROR_INVALID_SM = 0x000c,       /* Specified sm cannot be found */
  CUDBG_ERROR_INVALID_WARP = 0x000d,     /* Specified warp cannot be found */
  CUDBG_ERROR_INVALID_LANE = 0x000e,     /* Specified lane cannot be found */
  CUDBG_ERROR_SUSPENDED_DEVICE = 0x000f, /* device is suspended */
  CUDBG_ERROR_RUNNING_DEVICE = 0x0010, /* device is running and not suspended */
  CUDBG_ERROR_RESERVED_0 = 0x0011,     /* Reserved error code */
  CUDBG_ERROR_INVALID_ADDRESS = 0x0012,  /* address is out-of-range */
  CUDBG_ERROR_INCOMPATIBLE_API = 0x0013, /* API version does not match */
  CUDBG_ERROR_INITIALIZATION_FAILURE =
      0x0014,                        /* The CUDA Driver failed to initialize */
  CUDBG_ERROR_INVALID_GRID = 0x0015, /* Specified grid cannot be found */
  CUDBG_ERROR_NO_EVENT_AVAILABLE = 0x0016, /* No event left to be processed */
  CUDBG_ERROR_SOME_DEVICES_WATCHDOGGED =
      0x0017, /* One or more devices have an associated watchdog (eg. X) */
  CUDBG_ERROR_ALL_DEVICES_WATCHDOGGED =
      0x0018, /* All devices have an associated watchdog (eg. X) */
  CUDBG_ERROR_INVALID_ATTRIBUTE =
      0x0019, /* Specified attribute does not exist or is incorrect */
  CUDBG_ERROR_ZERO_CALL_DEPTH =
      0x001a, /* No function calls have been made on the device */
  CUDBG_ERROR_INVALID_CALL_LEVEL = 0x001b, /* Specified call level is invalid */
  CUDBG_ERROR_COMMUNICATION_FAILURE =
      0x001c, /* Communication error between the debugger and the application.
               */
  CUDBG_ERROR_INVALID_CONTEXT = 0x001d, /* Specified context cannot be found */
  CUDBG_ERROR_ADDRESS_NOT_IN_DEVICE_MEM =
      0x001e, /* Requested address was not originally allocated from device
                                 memory (most likely visible in system memory)
               */
  CUDBG_ERROR_MEMORY_UNMAPPING_FAILED =
      0x001f, /* Memory is not unmapped and cannot be unmapped */
  CUDBG_ERROR_INCOMPATIBLE_DISPLAY_DRIVER =
      0x0020, /* The display driver is incompatible with the API */
  CUDBG_ERROR_INVALID_MODULE = 0x0021, /* The specified module is not valid */
  CUDBG_ERROR_LANE_NOT_IN_SYSCALL =
      0x0022, /* The specified lane is not inside a device syscall */
  CUDBG_ERROR_MEMCHECK_NOT_ENABLED = 0x0023, /* Memcheck has not been enabled */
  CUDBG_ERROR_INVALID_ENVVAR_ARGS =
      0x0024, /* Some environment variable's value is invalid */
  CUDBG_ERROR_OS_RESOURCES =
      0x0025, /* Error while allocating resources from the OS */
  CUDBG_ERROR_FORK_FAILED =
      0x0026, /* Error while forking the debugger process */
  CUDBG_ERROR_NO_DEVICE_AVAILABLE =
      0x0027, /* No CUDA capable device was found */
  CUDBG_ERROR_ATTACH_NOT_POSSIBLE =
      0x0028, /* Attaching to the CUDA program is not possible */
  CUDBG_ERROR_WARP_RESUME_NOT_POSSIBLE =
      0x0029, /* The resumeWarpsUntilPC() API is not possible, use
                                 resumeDevice() or singleStepWarp() instead */
  CUDBG_ERROR_INVALID_WARP_MASK =
      0x002a, /* Specified warp mask is zero, or contains invalid warps */
  CUDBG_ERROR_AMBIGUOUS_MEMORY_ADDRESS =
      0x002b, /* Address cannot be resolved to a GPU unambiguously */
  CUDBG_ERROR_RECURSIVE_API_CALL =
      0x002c, /* Debug API entry point called from within a debug API callback
               */
} CUDBGResult;

typedef struct CUeglStreamConnection_st *CUeglStreamConnection;

#define MAX_PLANES 3

typedef enum CUeglFrameType_enum {
  CU_EGL_FRAME_TYPE_ARRAY = 0,
  CU_EGL_FRAME_TYPE_PITCH = 1,
} CUeglFrameType;

typedef enum CueglColorFormat_enum {
  CU_EGL_COLOR_FORMAT_YUV420_PLANAR = 0x00,
  CU_EGL_COLOR_FORMAT_YUV420_SEMIPLANAR = 0x01,
  CU_EGL_COLOR_FORMAT_YUV422_PLANAR = 0x02,
  CU_EGL_COLOR_FORMAT_YUV422_SEMIPLANAR = 0x03,
  CU_EGL_COLOR_FORMAT_RGB = 0x04,
  CU_EGL_COLOR_FORMAT_BGR = 0x05,
  CU_EGL_COLOR_FORMAT_ARGB = 0x06,
  CU_EGL_COLOR_FORMAT_RGBA = 0x07,
  CU_EGL_COLOR_FORMAT_L = 0x08,
  CU_EGL_COLOR_FORMAT_R = 0x09
} CUeglColorFormat;

typedef struct CUeglFrame_st {
  union {
    CUarray pArray[MAX_PLANES];
    void *pPitch[MAX_PLANES];
  } frame;
  unsigned int width;
  unsigned int height;
  unsigned int depth;
  unsigned int pitch;
  unsigned int planeCount;
  unsigned int numChannels;
  CUeglFrameType frameType;
  CUeglColorFormat eglColorFormat;
  CUarray_format cuFormat;
} CUeglFrame;


/**
 * Flags for choosing a coredump attribute to get/set
 */
typedef enum CUcoredumpSettings_enum { 
    CU_COREDUMP_ENABLE_ON_EXCEPTION = 1, 
    CU_COREDUMP_TRIGGER_HOST, 
    CU_COREDUMP_LIGHTWEIGHT, 
    CU_COREDUMP_ENABLE_USER_TRIGGER, 
    CU_COREDUMP_FILE, 
    CU_COREDUMP_PIPE, 
    CU_COREDUMP_MAX 
} CUcoredumpSettings;


/**
 * Library options to be specified with ::cuLibraryLoadData() or ::cuLibraryLoadFromFile()
 */
typedef enum CUlibraryOption_enum {
    CU_LIBRARY_HOST_UNIVERSAL_FUNCTION_AND_DATA_TABLE = 0,

    /**
     * Specifes that the argument \p code passed to ::cuLibraryLoadData() will be preserved.
     * Specifying this option will let the driver know that \p code can be accessed at any point
     * until ::cuLibraryUnload(). The default behavior is for the driver to allocate and
     * maintain its own copy of \p code. Note that this is only a memory usage optimization
     * hint and the driver can choose to ignore it if required.
     * Specifying this option with ::cuLibraryLoadFromFile() is invalid and
     * will return ::CUDA_ERROR_INVALID_VALUE.
     */
    CU_LIBRARY_BINARY_IS_PRESERVED = 1,

    CU_LIBRARY_NUM_OPTIONS
} CUlibraryOption;

/**
 * Specifies the properties for a multicast object.
 */
typedef struct CUmulticastObjectProp_st {
    /**
     * The number of devices in the multicast team that will bind memory to this
     * object
     */
    unsigned int numDevices;
    /**
     * The maximum amount of memory that can be bound to this multicast object
     * per device
     */
    size_t size;
    /**
     * Bitmask of exportable handle types (see ::CUmemAllocationHandleType) for
     * this object
     */
    unsigned long long handleTypes;
    /**
     * Flags for future use, must be zero now
     */
    unsigned long long flags;
} CUmulticastObjectProp_v1;
typedef CUmulticastObjectProp_v1 CUmulticastObjectProp;

/**
 * Flags for querying different granularities for a multicast object
 */
typedef enum CUmulticastGranularity_flags_enum {
    CU_MULTICAST_GRANULARITY_MINIMUM = 0x0,    /**< Minimum required granularity */
    CU_MULTICAST_GRANULARITY_RECOMMENDED = 0x1 /**< Recommended granularity for best performance */
} CUmulticastGranularity_flags;

/**
 * Size of tensor map descriptor
 */
#define CU_TENSOR_MAP_NUM_QWORDS 16

/**
 * Tensor map descriptor. Requires compiler support for aligning to 64 bytes.
 */
typedef struct CUtensorMap_st {
#if defined(__cplusplus) && (__cplusplus >= 201103L)
    alignas(64)
#elif __STDC_VERSION__ >= 201112L
    _Alignas(64)
#endif
        cuuint64_t opaque[CU_TENSOR_MAP_NUM_QWORDS];
} CUtensorMap;

/**
 * Tensor map data type
 */
typedef enum CUtensorMapDataType_enum { 
    CU_TENSOR_MAP_DATA_TYPE_UINT8 = 0, 
    CU_TENSOR_MAP_DATA_TYPE_UINT16, 
    CU_TENSOR_MAP_DATA_TYPE_UINT32, 
    CU_TENSOR_MAP_DATA_TYPE_INT32, 
    CU_TENSOR_MAP_DATA_TYPE_UINT64, 
    CU_TENSOR_MAP_DATA_TYPE_INT64, 
    CU_TENSOR_MAP_DATA_TYPE_FLOAT16, 
    CU_TENSOR_MAP_DATA_TYPE_FLOAT32, 
    CU_TENSOR_MAP_DATA_TYPE_FLOAT64, 
    CU_TENSOR_MAP_DATA_TYPE_BFLOAT16, 
    CU_TENSOR_MAP_DATA_TYPE_FLOAT32_FTZ, 
    CU_TENSOR_MAP_DATA_TYPE_TFLOAT32, 
    CU_TENSOR_MAP_DATA_TYPE_TFLOAT32_FTZ 
} CUtensorMapDataType;

/**
 * Tensor map interleave layout type
 */
typedef enum CUtensorMapInterleave_enum { 
    CU_TENSOR_MAP_INTERLEAVE_NONE = 0, 
    CU_TENSOR_MAP_INTERLEAVE_16B, 
    CU_TENSOR_MAP_INTERLEAVE_32B 
} CUtensorMapInterleave;

/**
 * Tensor map swizzling mode of shared memory banks
 */
typedef enum CUtensorMapSwizzle_enum {
    CU_TENSOR_MAP_SWIZZLE_NONE = 0,
    CU_TENSOR_MAP_SWIZZLE_32B,
    CU_TENSOR_MAP_SWIZZLE_64B,
    CU_TENSOR_MAP_SWIZZLE_128B,
} CUtensorMapSwizzle;

/**
 * Tensor map L2 promotion type
 */
typedef enum CUtensorMapL2promotion_enum { 
    CU_TENSOR_MAP_L2_PROMOTION_NONE = 0, 
    CU_TENSOR_MAP_L2_PROMOTION_L2_64B, 
    CU_TENSOR_MAP_L2_PROMOTION_L2_128B, 
    CU_TENSOR_MAP_L2_PROMOTION_L2_256B 
} CUtensorMapL2promotion;

/**
 * Tensor map out-of-bounds fill type
 */
typedef enum CUtensorMapFloatOOBfill_enum { 
    CU_TENSOR_MAP_FLOAT_OOB_FILL_NONE = 0, 
    CU_TENSOR_MAP_FLOAT_OOB_FILL_NAN_REQUEST_ZERO_FMA 
} CUtensorMapFloatOOBfill;

/** EGL */
typedef void *EGLStreamKHR;
typedef int32_t EGLint;
typedef void *EGLImageKHR;

#ifdef __cplusplus
}
#endif

#endif //HIJACK_CUDA_SUBSET_H