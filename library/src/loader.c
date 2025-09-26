/*
 * Tencent is pleased to support the open source community by making TKEStack
 * available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy of
 * the License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

//
// Created by thomas on 6/15/18.
//
#include <errno.h>
#include <fcntl.h>
#include <dlfcn.h>
#include <pthread.h>
#include <regex.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <signal.h>
#include <sys/stat.h>  
#include <sys/types.h>
#include <sys/mman.h>

#include "include/hook.h"
#include "include/cuda-helper.h"
#include "include/nvml-helper.h"

entry_t cuda_library_entry[] = {
    {.name = "cuInit"},
    {.name = "cuDeviceGet"},
    {.name = "cuDeviceGetCount"},
    {.name = "cuDeviceGetName"},
    {.name = "cuDeviceTotalMem_v2"},
    {.name = "cuDeviceGetAttribute"},
    {.name = "cuDeviceGetP2PAttribute"},
    {.name = "cuDriverGetVersion"},
    {.name = "cuDeviceGetByPCIBusId"},
    {.name = "cuDeviceGetPCIBusId"},
    {.name = "cuDevicePrimaryCtxRetain"},
    {.name = "cuDevicePrimaryCtxRelease"},
    {.name = "cuDevicePrimaryCtxSetFlags"},
    {.name = "cuDevicePrimaryCtxGetState"},
    {.name = "cuDevicePrimaryCtxReset"},
    {.name = "cuCtxCreate_v2"},
    {.name = "cuCtxGetFlags"},
    {.name = "cuCtxSetCurrent"},
    {.name = "cuCtxGetCurrent"},
    {.name = "cuCtxDetach"},
    {.name = "cuCtxGetApiVersion"},
    {.name = "cuCtxGetDevice"},
    {.name = "cuCtxGetLimit"},
    {.name = "cuCtxSetLimit"},
    {.name = "cuCtxGetCacheConfig"},
    {.name = "cuCtxSetCacheConfig"},
    {.name = "cuCtxGetSharedMemConfig"},
    {.name = "cuCtxGetStreamPriorityRange"},
    {.name = "cuCtxSetSharedMemConfig"},
    {.name = "cuCtxSynchronize"},
    {.name = "cuModuleLoad"},
    {.name = "cuModuleLoadData"},
    {.name = "cuModuleLoadFatBinary"},
    {.name = "cuModuleUnload"},
    {.name = "cuModuleGetFunction"},
    {.name = "cuModuleGetGlobal_v2"},
    {.name = "cuModuleGetTexRef"},
    {.name = "cuModuleGetSurfRef"},
    {.name = "cuLinkCreate"},
    {.name = "cuLinkAddData"},
    {.name = "cuLinkAddFile"},
    {.name = "cuLinkComplete"},
    {.name = "cuLinkDestroy"},
    {.name = "cuMemGetInfo_v2"},
    {.name = "cuMemAllocManaged"},
    {.name = "cuMemAlloc_v2"},
    {.name = "cuMemAllocPitch_v2"},
    {.name = "cuMemFree_v2"},
    {.name = "cuMemGetAddressRange_v2"},
    {.name = "cuMemFreeHost"},
    {.name = "cuMemHostAlloc"},
    {.name = "cuMemHostGetDevicePointer_v2"},
    {.name = "cuMemHostGetFlags"},
    {.name = "cuMemHostRegister_v2"},
    {.name = "cuMemHostUnregister"},
    {.name = "cuPointerGetAttribute"},
    {.name = "cuPointerGetAttributes"},
    {.name = "cuMemcpy"},
    {.name = "cuMemcpy_ptds"},
    {.name = "cuMemcpyAsync"},
    {.name = "cuMemcpyAsync_ptsz"},
    {.name = "cuMemcpyPeer"},
    {.name = "cuMemcpyPeer_ptds"},
    {.name = "cuMemcpyPeerAsync"},
    {.name = "cuMemcpyPeerAsync_ptsz"},
    {.name = "cuMemcpyHtoD_v2"},
    {.name = "cuMemcpyHtoD_v2_ptds"},
    {.name = "cuMemcpyHtoDAsync_v2"},
    {.name = "cuMemcpyHtoDAsync_v2_ptsz"},
    {.name = "cuMemcpyDtoH_v2"},
    {.name = "cuMemcpyDtoH_v2_ptds"},
    {.name = "cuMemcpyDtoHAsync_v2"},
    {.name = "cuMemcpyDtoHAsync_v2_ptsz"},
    {.name = "cuMemcpyDtoD_v2"},
    {.name = "cuMemcpyDtoD_v2_ptds"},
    {.name = "cuMemcpyDtoDAsync_v2"},
    {.name = "cuMemcpyDtoDAsync_v2_ptsz"},
    {.name = "cuMemcpy2DUnaligned_v2"},
    {.name = "cuMemcpy2DUnaligned_v2_ptds"},
    {.name = "cuMemcpy2DAsync_v2"},
    {.name = "cuMemcpy2DAsync_v2_ptsz"},
    {.name = "cuMemcpy3D_v2"},
    {.name = "cuMemcpy3D_v2_ptds"},
    {.name = "cuMemcpy3DAsync_v2"},
    {.name = "cuMemcpy3DAsync_v2_ptsz"},
    {.name = "cuMemcpy3DPeer"},
    {.name = "cuMemcpy3DPeer_ptds"},
    {.name = "cuMemcpy3DPeerAsync"},
    {.name = "cuMemcpy3DPeerAsync_ptsz"},
    {.name = "cuMemsetD8_v2"},
    {.name = "cuMemsetD8_v2_ptds"},
    {.name = "cuMemsetD8Async"},
    {.name = "cuMemsetD8Async_ptsz"},
    {.name = "cuMemsetD2D8_v2"},
    {.name = "cuMemsetD2D8_v2_ptds"},
    {.name = "cuMemsetD2D8Async"},
    {.name = "cuMemsetD2D8Async_ptsz"},
    {.name = "cuFuncSetCacheConfig"},
    {.name = "cuFuncSetSharedMemConfig"},
    {.name = "cuFuncGetAttribute"},
    {.name = "cuArrayCreate_v2"},
    {.name = "cuArrayGetDescriptor_v2"},
    {.name = "cuArray3DCreate_v2"},
    {.name = "cuArray3DGetDescriptor_v2"},
    {.name = "cuArrayDestroy"},
    {.name = "cuMipmappedArrayCreate"},
    {.name = "cuMipmappedArrayGetLevel"},
    {.name = "cuMipmappedArrayDestroy"},
    {.name = "cuTexRefCreate"},
    {.name = "cuTexRefDestroy"},
    {.name = "cuTexRefSetArray"},
    {.name = "cuTexRefSetMipmappedArray"},
    {.name = "cuTexRefSetAddress_v2"},
    {.name = "cuTexRefSetAddress2D_v3"},
    {.name = "cuTexRefSetFormat"},
    {.name = "cuTexRefSetAddressMode"},
    {.name = "cuTexRefSetFilterMode"},
    {.name = "cuTexRefSetMipmapFilterMode"},
    {.name = "cuTexRefSetMipmapLevelBias"},
    {.name = "cuTexRefSetMipmapLevelClamp"},
    {.name = "cuTexRefSetMaxAnisotropy"},
    {.name = "cuTexRefSetFlags"},
    {.name = "cuTexRefSetBorderColor"},
    {.name = "cuTexRefGetBorderColor"},
    {.name = "cuSurfRefSetArray"},
    {.name = "cuTexObjectCreate"},
    {.name = "cuTexObjectDestroy"},
    {.name = "cuTexObjectGetResourceDesc"},
    {.name = "cuTexObjectGetTextureDesc"},
    {.name = "cuTexObjectGetResourceViewDesc"},
    {.name = "cuSurfObjectCreate"},
    {.name = "cuSurfObjectDestroy"},
    {.name = "cuSurfObjectGetResourceDesc"},
    {.name = "cuLaunchKernel"},
    {.name = "cuLaunchKernel_ptsz"},
    {.name = "cuLaunchKernelEx"},
    {.name = "cuLaunchKernelEx_ptsz"},
    {.name = "cuEventCreate"},
    {.name = "cuEventRecord"},
    {.name = "cuEventRecord_ptsz"},
    {.name = "cuEventQuery"},
    {.name = "cuEventSynchronize"},
    {.name = "cuEventDestroy_v2"},
    {.name = "cuEventElapsedTime"},
    {.name = "cuStreamWaitValue32"},
    {.name = "cuStreamWaitValue32_ptsz"},
    {.name = "cuStreamWriteValue32"},
    {.name = "cuStreamWriteValue32_ptsz"},
    {.name = "cuStreamBatchMemOp"},
    {.name = "cuStreamBatchMemOp_ptsz"},
    {.name = "cuStreamCreate"},
    {.name = "cuStreamCreateWithPriority"},
    {.name = "cuStreamGetPriority"},
    {.name = "cuStreamGetPriority_ptsz"},
    {.name = "cuStreamGetFlags"},
    {.name = "cuStreamGetFlags_ptsz"},
    {.name = "cuStreamDestroy_v2"},
    {.name = "cuStreamWaitEvent"},
    {.name = "cuStreamWaitEvent_ptsz"},
    {.name = "cuStreamAddCallback"},
    {.name = "cuStreamAddCallback_ptsz"},
    {.name = "cuStreamSynchronize"},
    {.name = "cuStreamSynchronize_ptsz"},
    {.name = "cuStreamQuery"},
    {.name = "cuStreamQuery_ptsz"},
    {.name = "cuStreamAttachMemAsync"},
    {.name = "cuStreamAttachMemAsync_ptsz"},
    {.name = "cuDeviceCanAccessPeer"},
    {.name = "cuCtxEnablePeerAccess"},
    {.name = "cuCtxDisablePeerAccess"},
    {.name = "cuIpcGetEventHandle"},
    {.name = "cuIpcOpenEventHandle"},
    {.name = "cuIpcGetMemHandle"},
    {.name = "cuIpcOpenMemHandle"},
    {.name = "cuIpcCloseMemHandle"},
    {.name = "cuGLCtxCreate_v2"},
    {.name = "cuGLInit"},
    {.name = "cuGLGetDevices"},
    {.name = "cuGLRegisterBufferObject"},
    {.name = "cuGLMapBufferObject_v2"},
    {.name = "cuGLMapBufferObject_v2_ptds"},
    {.name = "cuGLMapBufferObjectAsync_v2"},
    {.name = "cuGLMapBufferObjectAsync_v2_ptsz"},
    {.name = "cuGLUnmapBufferObject"},
    {.name = "cuGLUnmapBufferObjectAsync"},
    {.name = "cuGLUnregisterBufferObject"},
    {.name = "cuGLSetBufferObjectMapFlags"},
    {.name = "cuGraphicsGLRegisterImage"},
    {.name = "cuGraphicsGLRegisterBuffer"},
    {.name = "cuGraphicsUnregisterResource"},
    {.name = "cuGraphicsMapResources"},
    {.name = "cuGraphicsMapResources_ptsz"},
    {.name = "cuGraphicsUnmapResources"},
    {.name = "cuGraphicsUnmapResources_ptsz"},
    {.name = "cuGraphicsResourceSetMapFlags_v2"},
    {.name = "cuGraphicsSubResourceGetMappedArray"},
    {.name = "cuGraphicsResourceGetMappedMipmappedArray"},
    {.name = "cuGraphicsResourceGetMappedPointer_v2"},
    {.name = "cuProfilerInitialize"},
    {.name = "cuProfilerStart"},
    {.name = "cuProfilerStop"},
    {.name = "cuVDPAUGetDevice"},
    {.name = "cuVDPAUCtxCreate_v2"},
    {.name = "cuGraphicsVDPAURegisterVideoSurface"},
    {.name = "cuGraphicsVDPAURegisterOutputSurface"},
    {.name = "cuGetExportTable"},
    {.name = "cuOccupancyMaxActiveBlocksPerMultiprocessor"},
    {.name = "cuMemAdvise"},
    {.name = "cuMemAdvise_v2"},
    {.name = "cuMemPrefetchAsync"},
    {.name = "cuMemPrefetchAsync_ptsz"},
    {.name = "cuMemPrefetchAsync_v2"},
    {.name = "cuMemPrefetchAsync_v2_ptsz"},
    {.name = "cuMemRangeGetAttribute"},
    {.name = "cuMemRangeGetAttributes"},
    {.name = "cuGetErrorString"},
    {.name = "cuGetErrorName"},
    {.name = "cuArray3DCreate"},
    {.name = "cuArray3DGetDescriptor"},
    {.name = "cuArrayCreate"},
    {.name = "cuArrayGetDescriptor"},
    {.name = "cuCtxAttach"},
    {.name = "cuCtxCreate"},
    {.name = "cuCtxDestroy"},
    {.name = "cuCtxDestroy_v2"},
    {.name = "cuCtxPopCurrent"},
    {.name = "cuCtxPopCurrent_v2"},
    {.name = "cuCtxPushCurrent"},
    {.name = "cuCtxPushCurrent_v2"},
    {.name = "cudbgApiAttach"},
    {.name = "cudbgApiDetach"},
    {.name = "cudbgApiInit"},
    {.name = "cudbgGetAPI"},
    {.name = "cudbgGetAPIVersion"},
    {.name = "cudbgMain"},
    {.name = "cudbgReportDriverApiError"},
    {.name = "cudbgReportDriverInternalError"},
    {.name = "cuDeviceComputeCapability"},
    {.name = "cuDeviceGetProperties"},
    {.name = "cuDeviceTotalMem"},
    {.name = "cuEGLInit"},
    {.name = "cuEGLStreamConsumerAcquireFrame"},
    {.name = "cuEGLStreamConsumerConnect"},
    {.name = "cuEGLStreamConsumerConnectWithFlags"},
    {.name = "cuEGLStreamConsumerDisconnect"},
    {.name = "cuEGLStreamConsumerReleaseFrame"},
    {.name = "cuEGLStreamProducerConnect"},
    {.name = "cuEGLStreamProducerDisconnect"},
    {.name = "cuEGLStreamProducerPresentFrame"},
    {.name = "cuEGLStreamProducerReturnFrame"},
    {.name = "cuEventDestroy"},
    {.name = "cuFuncSetAttribute"},
    {.name = "cuFuncSetBlockShape"},
    {.name = "cuFuncSetSharedSize"},
    {.name = "cuGLCtxCreate"},
    {.name = "cuGLGetDevices_v2"},
    {.name = "cuGLMapBufferObject"},
    {.name = "cuGLMapBufferObjectAsync"},
    {.name = "cuGraphicsEGLRegisterImage"},
    {.name = "cuGraphicsResourceGetMappedEglFrame"},
    {.name = "cuGraphicsResourceGetMappedPointer"},
    {.name = "cuGraphicsResourceSetMapFlags"},
    {.name = "cuLaunch"},
    {.name = "cuLaunchCooperativeKernel"},
    {.name = "cuLaunchCooperativeKernelMultiDevice"},
    {.name = "cuLaunchCooperativeKernel_ptsz"},
    {.name = "cuLaunchGrid"},
    {.name = "cuLaunchGridAsync"},
    {.name = "cuLinkAddData_v2"},
    {.name = "cuLinkAddFile_v2"},
    {.name = "cuLinkCreate_v2"},
    {.name = "cuMemAlloc"},
    {.name = "cuMemAllocHost"},
    {.name = "cuMemAllocHost_v2"},
    {.name = "cuMemAllocPitch"},
    {.name = "cuMemcpy2D"},
    {.name = "cuMemcpy2DAsync"},
    {.name = "cuMemcpy2DUnaligned"},
    {.name = "cuMemcpy2D_v2"},
    {.name = "cuMemcpy2D_v2_ptds"},
    {.name = "cuMemcpy3D"},
    {.name = "cuMemcpy3DAsync"},
    {.name = "cuMemcpyAtoA"},
    {.name = "cuMemcpyAtoA_v2"},
    {.name = "cuMemcpyAtoA_v2_ptds"},
    {.name = "cuMemcpyAtoD"},
    {.name = "cuMemcpyAtoD_v2"},
    {.name = "cuMemcpyAtoD_v2_ptds"},
    {.name = "cuMemcpyAtoH"},
    {.name = "cuMemcpyAtoHAsync"},
    {.name = "cuMemcpyAtoHAsync_v2"},
    {.name = "cuMemcpyAtoHAsync_v2_ptsz"},
    {.name = "cuMemcpyAtoH_v2"},
    {.name = "cuMemcpyAtoH_v2_ptds"},
    {.name = "cuMemcpyDtoA"},
    {.name = "cuMemcpyDtoA_v2"},
    {.name = "cuMemcpyDtoA_v2_ptds"},
    {.name = "cuMemcpyDtoD"},
    {.name = "cuMemcpyDtoDAsync"},
    {.name = "cuMemcpyDtoH"},
    {.name = "cuMemcpyDtoHAsync"},
    {.name = "cuMemcpyHtoA"},
    {.name = "cuMemcpyHtoAAsync"},
    {.name = "cuMemcpyHtoAAsync_v2"},
    {.name = "cuMemcpyHtoAAsync_v2_ptsz"},
    {.name = "cuMemcpyHtoA_v2"},
    {.name = "cuMemcpyHtoA_v2_ptds"},
    {.name = "cuMemcpyHtoD"},
    {.name = "cuMemcpyHtoDAsync"},
    {.name = "cuMemFree"},
    {.name = "cuMemGetAddressRange"},
    //{.name = "cuMemGetAttribute"},
    //{.name = "cuMemGetAttribute_v2"},
    {.name = "cuMemGetInfo"},
    {.name = "cuMemHostGetDevicePointer"},
    {.name = "cuMemHostRegister"},
    {.name = "cuMemsetD16"},
    {.name = "cuMemsetD16Async"},
    {.name = "cuMemsetD16Async_ptsz"},
    {.name = "cuMemsetD16_v2"},
    {.name = "cuMemsetD16_v2_ptds"},
    {.name = "cuMemsetD2D16"},
    {.name = "cuMemsetD2D16Async"},
    {.name = "cuMemsetD2D16Async_ptsz"},
    {.name = "cuMemsetD2D16_v2"},
    {.name = "cuMemsetD2D16_v2_ptds"},
    {.name = "cuMemsetD2D32"},
    {.name = "cuMemsetD2D32Async"},
    {.name = "cuMemsetD2D32Async_ptsz"},
    {.name = "cuMemsetD2D32_v2"},
    {.name = "cuMemsetD2D32_v2_ptds"},
    {.name = "cuMemsetD2D8"},
    {.name = "cuMemsetD32"},
    {.name = "cuMemsetD32Async"},
    {.name = "cuMemsetD32Async_ptsz"},
    {.name = "cuMemsetD32_v2"},
    {.name = "cuMemsetD32_v2_ptds"},
    {.name = "cuMemsetD8"},
    {.name = "cuModuleGetGlobal"},
    {.name = "cuModuleLoadDataEx"},
    {.name = "cuOccupancyMaxActiveBlocksPerMultiprocessorWithFlags"},
    {.name = "cuOccupancyMaxActiveClusters"},
    {.name = "cuOccupancyMaxPotentialBlockSize"},
    {.name = "cuOccupancyMaxPotentialBlockSizeWithFlags"},
    {.name = "cuOccupancyMaxPotentialClusterSize"},
    {.name = "cuParamSetf"},
    {.name = "cuParamSeti"},
    {.name = "cuParamSetSize"},
    {.name = "cuParamSetTexRef"},
    {.name = "cuParamSetv"},
    {.name = "cuPointerSetAttribute"},
    {.name = "cuStreamDestroy"},
    {.name = "cuStreamWaitValue64"},
    {.name = "cuStreamWaitValue64_ptsz"},
    {.name = "cuStreamWriteValue64"},
    {.name = "cuStreamWriteValue64_ptsz"},
    {.name = "cuSurfRefGetArray"},
    {.name = "cuTexRefGetAddress"},
    {.name = "cuTexRefGetAddressMode"},
    {.name = "cuTexRefGetAddress_v2"},
    {.name = "cuTexRefGetArray"},
    {.name = "cuTexRefGetFilterMode"},
    {.name = "cuTexRefGetFlags"},
    {.name = "cuTexRefGetFormat"},
    {.name = "cuTexRefGetMaxAnisotropy"},
    {.name = "cuTexRefGetMipmapFilterMode"},
    {.name = "cuTexRefGetMipmapLevelBias"},
    {.name = "cuTexRefGetMipmapLevelClamp"},
    {.name = "cuTexRefGetMipmappedArray"},
    {.name = "cuTexRefSetAddress"},
    {.name = "cuTexRefSetAddress2D"},
    {.name = "cuTexRefSetAddress2D_v2"},
    {.name = "cuVDPAUCtxCreate"},
    {.name = "cuEGLApiInit"},
    {.name = "cuDestroyExternalMemory"},
    {.name = "cuDestroyExternalSemaphore"},
    {.name = "cuDeviceGetUuid"},
    {.name = "cuExternalMemoryGetMappedBuffer"},
    {.name = "cuExternalMemoryGetMappedMipmappedArray"},
    {.name = "cuGraphAddChildGraphNode"},
    {.name = "cuGraphAddDependencies"},
    {.name = "cuGraphAddEmptyNode"},
    {.name = "cuGraphAddHostNode"},
    {.name = "cuGraphAddKernelNode"},
    {.name = "cuGraphAddKernelNode_v2"},
    {.name = "cuGraphAddMemcpyNode"},
    {.name = "cuGraphAddMemsetNode"},
    {.name = "cuGraphChildGraphNodeGetGraph"},
    {.name = "cuGraphClone"},
    {.name = "cuGraphCreate"},
    {.name = "cuGraphDestroy"},
    {.name = "cuGraphDestroyNode"},
    {.name = "cuGraphExecDestroy"},
    {.name = "cuGraphGetEdges"},
    {.name = "cuGraphGetNodes"},
    {.name = "cuGraphGetRootNodes"},
    {.name = "cuGraphHostNodeGetParams"},
    {.name = "cuGraphHostNodeSetParams"},
    {.name = "cuGraphInstantiate"},
    {.name = "cuGraphKernelNodeGetParams"},
    {.name = "cuGraphKernelNodeGetParams_v2"},
    {.name = "cuGraphKernelNodeSetParams"},
    {.name = "cuGraphKernelNodeSetParams_v2"},
    {.name = "cuGraphLaunch"},
    {.name = "cuGraphLaunch_ptsz"},
    {.name = "cuGraphMemcpyNodeGetParams"},
    {.name = "cuGraphMemcpyNodeSetParams"},
    {.name = "cuGraphMemsetNodeGetParams"},
    {.name = "cuGraphMemsetNodeSetParams"},
    {.name = "cuGraphNodeFindInClone"},
    {.name = "cuGraphNodeGetDependencies"},
    {.name = "cuGraphNodeGetDependentNodes"},
    {.name = "cuGraphNodeGetType"},
    {.name = "cuGraphRemoveDependencies"},
    {.name = "cuImportExternalMemory"},
    {.name = "cuImportExternalSemaphore"},
    {.name = "cuLaunchHostFunc"},
    {.name = "cuLaunchHostFunc_ptsz"},
    {.name = "cuSignalExternalSemaphoresAsync"},
    {.name = "cuSignalExternalSemaphoresAsync_ptsz"},
    {.name = "cuStreamBeginCapture"},
    {.name = "cuStreamBeginCapture_ptsz"},
    {.name = "cuStreamEndCapture"},
    {.name = "cuStreamEndCapture_ptsz"},
    {.name = "cuStreamGetCtx"},
    {.name = "cuStreamGetCtx_v2"},
    {.name = "cuStreamGetCtx_ptsz"},
    {.name = "cuStreamGetCtx_v2_ptsz"},
    {.name = "cuGreenCtxStreamCreate"},
    {.name = "cuStreamIsCapturing"},
    {.name = "cuStreamIsCapturing_ptsz"},
    {.name = "cuWaitExternalSemaphoresAsync"},
    {.name = "cuWaitExternalSemaphoresAsync_ptsz"},
    {.name = "cuGraphExecKernelNodeSetParams"},
    {.name = "cuStreamBeginCapture_v2"},
    {.name = "cuStreamBeginCapture_v2_ptsz"},
//    {.name = "cuStreamGetCaptureInfo"},
//    {.name = "cuStreamGetCaptureInfo_ptsz"},
    {.name = "cuThreadExchangeStreamCaptureMode"},
    {.name = "cuDeviceGetNvSciSyncAttributes"},
    {.name = "cuGraphExecHostNodeSetParams"},
    {.name = "cuGraphExecMemcpyNodeSetParams"},
    {.name = "cuGraphExecMemsetNodeSetParams"},
    {.name = "cuGraphExecUpdate"},
    {.name = "cuGraphExecUpdate_v2"},
    {.name = "cuMemAddressFree"},
    {.name = "cuMemAddressReserve"},
    {.name = "cuMemCreate"},
    {.name = "cuMemExportToShareableHandle"},
    {.name = "cuMemGetAccess"},
    {.name = "cuMemGetAllocationGranularity"},
    {.name = "cuMemGetAllocationPropertiesFromHandle"},
    {.name = "cuMemImportFromShareableHandle"},
    {.name = "cuMemMap"},
    {.name = "cuMemRelease"},
    {.name = "cuMemSetAccess"},
    {.name = "cuMemUnmap"},
    {.name = "cuCtxResetPersistingL2Cache"},
    {.name = "cuDevicePrimaryCtxRelease_v2"},
    {.name = "cuDevicePrimaryCtxReset_v2"},
    {.name = "cuDevicePrimaryCtxSetFlags_v2"},
    {.name = "cuFuncGetModule"},
    {.name = "cuGraphInstantiate_v2"},
    {.name = "cuGraphKernelNodeCopyAttributes"},
    {.name = "cuGraphKernelNodeGetAttribute"},
    {.name = "cuGraphKernelNodeSetAttribute"},
    {.name = "cuMemRetainAllocationHandle"},
    {.name = "cuOccupancyAvailableDynamicSMemPerBlock"},
    {.name = "cuStreamCopyAttributes"},
    {.name = "cuStreamCopyAttributes_ptsz"},
    {.name = "cuStreamGetAttribute"},
    {.name = "cuStreamGetAttribute_ptsz"},
    {.name = "cuStreamSetAttribute"},
    {.name = "cuStreamSetAttribute_ptsz"},
    {.name = "cuArrayGetPlane"},
    {.name = "cuArrayGetSparseProperties"},
    {.name = "cuDeviceGetDefaultMemPool"},
    {.name = "cuDeviceGetLuid"},
    {.name = "cuDeviceGetMemPool"},
    {.name = "cuDeviceGetTexture1DLinearMaxWidth"},
    {.name = "cuDeviceSetMemPool"},
    {.name = "cuEventRecordWithFlags"},
    {.name = "cuEventRecordWithFlags_ptsz"},
    {.name = "cuGraphAddEventRecordNode"},
    {.name = "cuGraphAddEventWaitNode"},
    {.name = "cuGraphAddExternalSemaphoresSignalNode"},
    {.name = "cuGraphAddExternalSemaphoresWaitNode"},
    {.name = "cuGraphEventRecordNodeGetEvent"},
    {.name = "cuGraphEventRecordNodeSetEvent"},
    {.name = "cuGraphEventWaitNodeGetEvent"},
    {.name = "cuGraphEventWaitNodeSetEvent"},
    {.name = "cuGraphExecChildGraphNodeSetParams"},
    {.name = "cuGraphExecEventRecordNodeSetEvent"},
    {.name = "cuGraphExecEventWaitNodeSetEvent"},
    {.name = "cuGraphExecExternalSemaphoresSignalNodeSetParams"},
    {.name = "cuGraphExecExternalSemaphoresWaitNodeSetParams"},
    {.name = "cuGraphExternalSemaphoresSignalNodeGetParams"},
    {.name = "cuGraphExternalSemaphoresSignalNodeSetParams"},
    {.name = "cuGraphExternalSemaphoresWaitNodeGetParams"},
    {.name = "cuGraphExternalSemaphoresWaitNodeSetParams"},
    {.name = "cuGraphUpload"},
    {.name = "cuGraphUpload_ptsz"},
    {.name = "cuIpcOpenMemHandle_v2"},
    {.name = "cuMemAllocAsync"},
    {.name = "cuMemAllocAsync_ptsz"},
    {.name = "cuMemAllocFromPoolAsync"},
    {.name = "cuMemAllocFromPoolAsync_ptsz"},
    {.name = "cuMemFreeAsync"},
    {.name = "cuMemFreeAsync_ptsz"},
    {.name = "cuMemMapArrayAsync"},
    {.name = "cuMemMapArrayAsync_ptsz"},
    {.name = "cuMemPoolCreate"},
    {.name = "cuMemPoolDestroy"},
    {.name = "cuMemPoolExportPointer"},
    {.name = "cuMemPoolExportToShareableHandle"},
    {.name = "cuMemPoolGetAccess"},
    {.name = "cuMemPoolGetAttribute"},
    {.name = "cuMemPoolImportFromShareableHandle"},
    {.name = "cuMemPoolImportPointer"},
    {.name = "cuMemPoolSetAccess"},
    {.name = "cuMemPoolSetAttribute"},
    {.name = "cuMemPoolTrimTo"},
    {.name = "cuMipmappedArrayGetSparseProperties"},
    {.name = "cuCtxCreate_v3"},
    {.name = "cuCtxCreate_v4"},
    {.name = "cuCtxGetExecAffinity"},
    {.name = "cuDeviceGetExecAffinitySupport"},
    {.name = "cuDeviceGetGraphMemAttribute"},
    {.name = "cuDeviceGetUuid_v2"},
    {.name = "cuDeviceGraphMemTrim"},
    {.name = "cuDeviceSetGraphMemAttribute"},
    {.name = "cuFlushGPUDirectRDMAWrites"},
    {.name = "cuGetProcAddress"},
    {.name = "cuGetProcAddress_v2"},
    {.name = "cuGraphAddMemAllocNode"},
    {.name = "cuGraphAddMemFreeNode"},
    {.name = "cuGraphDebugDotPrint"},
    {.name = "cuGraphInstantiateWithFlags"},
    {.name = "cuGraphMemAllocNodeGetParams"},
    {.name = "cuGraphMemFreeNodeGetParams"},
    {.name = "cuGraphReleaseUserObject"},
    {.name = "cuGraphRetainUserObject"},
//    {.name = "cuStreamGetCaptureInfo_v2"},
//    {.name = "cuStreamGetCaptureInfo_v2_ptsz"},
    {.name = "cuStreamUpdateCaptureDependencies"},
    {.name = "cuStreamUpdateCaptureDependencies_ptsz"},
    {.name = "cuUserObjectCreate"},
    {.name = "cuUserObjectRelease"},
    {.name = "cuUserObjectRetain"},
    {.name = "cuArrayGetMemoryRequirements"},
    {.name = "cuMipmappedArrayGetMemoryRequirements"},
    {.name = "cuStreamWaitValue32_v2"},
    {.name = "cuStreamWaitValue32_v2_ptsz"},
    {.name = "cuStreamWaitValue64_v2"},
    {.name = "cuStreamWaitValue64_v2_ptsz"},
    {.name = "cuStreamWriteValue32_v2"},
    {.name = "cuStreamWriteValue32_v2_ptsz"},
    {.name = "cuStreamWriteValue64_v2"},
    {.name = "cuStreamWriteValue64_v2_ptsz"},
    {.name = "cuStreamBatchMemOp_v2"},
    {.name = "cuStreamBatchMemOp_v2_ptsz"},
    {.name = "cuGraphAddBatchMemOpNode"},
    {.name = "cuGraphBatchMemOpNodeGetParams"},
    {.name = "cuGraphBatchMemOpNodeSetParams"},
    {.name = "cuGraphExecBatchMemOpNodeSetParams"},
    {.name = "cuGraphNodeGetEnabled"},
    {.name = "cuGraphNodeSetEnabled"},
    {.name = "cuModuleGetLoadingMode"},
    {.name = "cuMemGetHandleForAddressRange"},
    {.name = "cuGraphAddNode"},
    {.name = "cuGraphAddNode_v2"},
    {.name = "cuGraphExecGetFlags"},
    {.name = "cuGraphExecNodeSetParams"},
    {.name = "cuGraphInstantiateWithParams"},
    {.name = "cuGraphInstantiateWithParams_ptsz"},
    {.name = "cuGraphNodeSetParams"},
    {.name = "cuStreamGetId"},
    {.name = "cuStreamGetId_ptsz"},
    {.name = "cuCoredumpGetAttribute"},
    {.name = "cuCoredumpGetAttributeGlobal"},
    {.name = "cuCoredumpSetAttribute"},
    {.name = "cuCoredumpSetAttributeGlobal"},
    {.name = "cuCtxGetId"},
    {.name = "cuCtxSetFlags"},
    {.name = "cuKernelGetAttribute"},
    {.name = "cuKernelGetFunction"},
    {.name = "cuKernelSetAttribute"},
    {.name = "cuKernelSetCacheConfig"},
    {.name = "cuLibraryGetGlobal"},
    {.name = "cuLibraryGetKernel"},
    {.name = "cuLibraryGetManaged"},
    {.name = "cuLibraryGetModule"},
    {.name = "cuLibraryGetUnifiedFunction"},
    {.name = "cuLibraryLoadData"},
    {.name = "cuLibraryLoadFromFile"},
    {.name = "cuLibraryUnload"},
    {.name = "cuMulticastAddDevice"},
    {.name = "cuMulticastBindAddr"},
    {.name = "cuMulticastBindMem"},
    {.name = "cuMulticastCreate"},
    {.name = "cuMulticastGetGranularity"},
    {.name = "cuMulticastUnbind"},
    {.name = "cuTensorMapEncodeIm2col"},
    {.name = "cuTensorMapEncodeTiled"},
    {.name = "cuTensorMapReplaceAddress"},
};

entry_t nvml_library_entry[] = {
    {.name = "nvmlInit"},
    {.name = "nvmlShutdown"},
    {.name = "nvmlErrorString"},
    {.name = "nvmlDeviceGetHandleByIndex"},
    {.name = "nvmlDeviceGetComputeRunningProcesses"},
    {.name = "nvmlDeviceGetPciInfo"},
    {.name = "nvmlDeviceGetProcessUtilization"},
    {.name = "nvmlDeviceGetCount"},
    {.name = "nvmlDeviceClearAccountingPids"},
    {.name = "nvmlDeviceClearCpuAffinity"},
    {.name = "nvmlDeviceClearEccErrorCounts"},
    {.name = "nvmlDeviceDiscoverGpus"},
    {.name = "nvmlDeviceFreezeNvLinkUtilizationCounter"},
    {.name = "nvmlDeviceGetAccountingBufferSize"},
    {.name = "nvmlDeviceGetAccountingMode"},
    {.name = "nvmlDeviceGetAccountingPids"},
    {.name = "nvmlDeviceGetAccountingStats"},
    {.name = "nvmlDeviceGetActiveVgpus"},
    {.name = "nvmlDeviceGetAPIRestriction"},
    {.name = "nvmlDeviceGetApplicationsClock"},
    {.name = "nvmlDeviceGetAutoBoostedClocksEnabled"},
    {.name = "nvmlDeviceGetBAR1MemoryInfo"},
    {.name = "nvmlDeviceGetBoardId"},
    {.name = "nvmlDeviceGetBoardPartNumber"},
    {.name = "nvmlDeviceGetBrand"},
    {.name = "nvmlDeviceGetBridgeChipInfo"},
    {.name = "nvmlDeviceGetClock"},
    {.name = "nvmlDeviceGetClockInfo"},
    {.name = "nvmlDeviceGetComputeMode"},
    {.name = "nvmlDeviceGetCount_v2"},
    {.name = "nvmlDeviceGetCpuAffinity"},
    {.name = "nvmlDeviceGetCreatableVgpus"},
    {.name = "nvmlDeviceGetCudaComputeCapability"},
    {.name = "nvmlDeviceGetCurrentClocksThrottleReasons"},
    {.name = "nvmlDeviceGetCurrPcieLinkGeneration"},
    {.name = "nvmlDeviceGetCurrPcieLinkWidth"},
    {.name = "nvmlDeviceGetDecoderUtilization"},
    {.name = "nvmlDeviceGetDefaultApplicationsClock"},
    {.name = "nvmlDeviceGetDetailedEccErrors"},
    {.name = "nvmlDeviceGetDisplayActive"},
    {.name = "nvmlDeviceGetDisplayMode"},
    {.name = "nvmlDeviceGetDriverModel"},
    {.name = "nvmlDeviceGetEccMode"},
    {.name = "nvmlDeviceGetEncoderCapacity"},
    {.name = "nvmlDeviceGetEncoderSessions"},
    {.name = "nvmlDeviceGetEncoderStats"},
    {.name = "nvmlDeviceGetEncoderUtilization"},
    {.name = "nvmlDeviceGetEnforcedPowerLimit"},
    {.name = "nvmlDeviceGetFanSpeed"},
    {.name = "nvmlDeviceGetFanSpeed_v2"},
    {.name = "nvmlDeviceGetFieldValues"},
    {.name = "nvmlDeviceGetGpuOperationMode"},
    {.name = "nvmlDeviceGetGraphicsRunningProcesses"},
    {.name = "nvmlDeviceGetGridLicensableFeatures"},
    {.name = "nvmlDeviceGetHandleByIndex_v2"},
    {.name = "nvmlDeviceGetHandleByPciBusId"},
    {.name = "nvmlDeviceGetHandleByPciBusId_v2"},
    {.name = "nvmlDeviceGetHandleBySerial"},
    {.name = "nvmlDeviceGetHandleByUUID"},
    {.name = "nvmlDeviceGetIndex"},
    {.name = "nvmlDeviceGetInforomConfigurationChecksum"},
    {.name = "nvmlDeviceGetInforomImageVersion"},
    {.name = "nvmlDeviceGetInforomVersion"},
    {.name = "nvmlDeviceGetMaxClockInfo"},
    {.name = "nvmlDeviceGetMaxCustomerBoostClock"},
    {.name = "nvmlDeviceGetMaxPcieLinkGeneration"},
    {.name = "nvmlDeviceGetMaxPcieLinkWidth"},
    {.name = "nvmlDeviceGetMemoryErrorCounter"},
    {.name = "nvmlDeviceGetMemoryInfo"},
    {.name = "nvmlDeviceGetMemoryInfo_v2"},
    {.name = "nvmlDeviceGetMinorNumber"},
    {.name = "nvmlDeviceGetMPSComputeRunningProcesses"},
    {.name = "nvmlDeviceGetMultiGpuBoard"},
    {.name = "nvmlDeviceGetName"},
    {.name = "nvmlDeviceGetNvLinkCapability"},
    {.name = "nvmlDeviceGetNvLinkErrorCounter"},
    {.name = "nvmlDeviceGetNvLinkRemotePciInfo"},
    {.name = "nvmlDeviceGetNvLinkRemotePciInfo_v2"},
    {.name = "nvmlDeviceGetNvLinkState"},
    {.name = "nvmlDeviceGetNvLinkUtilizationControl"},
    {.name = "nvmlDeviceGetNvLinkUtilizationCounter"},
    {.name = "nvmlDeviceGetNvLinkVersion"},
    {.name = "nvmlDeviceGetP2PStatus"},
    {.name = "nvmlDeviceGetPcieReplayCounter"},
    {.name = "nvmlDeviceGetPcieThroughput"},
    {.name = "nvmlDeviceGetPciInfo_v2"},
    {.name = "nvmlDeviceGetPciInfo_v3"},
    {.name = "nvmlDeviceGetPerformanceState"},
    {.name = "nvmlDeviceGetPersistenceMode"},
    {.name = "nvmlDeviceGetPowerManagementDefaultLimit"},
    {.name = "nvmlDeviceGetPowerManagementLimit"},
    {.name = "nvmlDeviceGetPowerManagementLimitConstraints"},
    {.name = "nvmlDeviceGetPowerManagementMode"},
    {.name = "nvmlDeviceGetPowerState"},
    {.name = "nvmlDeviceGetPowerUsage"},
    {.name = "nvmlDeviceGetRetiredPages"},
    {.name = "nvmlDeviceGetRetiredPagesPendingStatus"},
    {.name = "nvmlDeviceGetSamples"},
    {.name = "nvmlDeviceGetSerial"},
    {.name = "nvmlDeviceGetSupportedClocksThrottleReasons"},
    {.name = "nvmlDeviceGetSupportedEventTypes"},
    {.name = "nvmlDeviceGetSupportedGraphicsClocks"},
    {.name = "nvmlDeviceGetSupportedMemoryClocks"},
    {.name = "nvmlDeviceGetSupportedVgpus"},
    {.name = "nvmlDeviceGetTemperature"},
    {.name = "nvmlDeviceGetTemperatureThreshold"},
    {.name = "nvmlDeviceGetTopologyCommonAncestor"},
    {.name = "nvmlDeviceGetTopologyNearestGpus"},
    {.name = "nvmlDeviceGetTotalEccErrors"},
    {.name = "nvmlDeviceGetTotalEnergyConsumption"},
    {.name = "nvmlDeviceGetUtilizationRates"},
    {.name = "nvmlDeviceGetUUID"},
    {.name = "nvmlDeviceGetVbiosVersion"},
    {.name = "nvmlDeviceGetVgpuMetadata"},
    {.name = "nvmlDeviceGetVgpuProcessUtilization"},
    {.name = "nvmlDeviceGetVgpuUtilization"},
    {.name = "nvmlDeviceGetViolationStatus"},
    {.name = "nvmlDeviceGetVirtualizationMode"},
    {.name = "nvmlDeviceModifyDrainState"},
    {.name = "nvmlDeviceOnSameBoard"},
    {.name = "nvmlDeviceQueryDrainState"},
    {.name = "nvmlDeviceRegisterEvents"},
    {.name = "nvmlDeviceRemoveGpu"},
    {.name = "nvmlDeviceRemoveGpu_v2"},
    {.name = "nvmlDeviceResetApplicationsClocks"},
    {.name = "nvmlDeviceResetNvLinkErrorCounters"},
    {.name = "nvmlDeviceResetNvLinkUtilizationCounter"},
    {.name = "nvmlDeviceSetAccountingMode"},
    {.name = "nvmlDeviceSetAPIRestriction"},
    {.name = "nvmlDeviceSetApplicationsClocks"},
    {.name = "nvmlDeviceSetAutoBoostedClocksEnabled"},
    /** We hook this*/
    {.name = "nvmlDeviceSetComputeMode"},
    {.name = "nvmlDeviceSetCpuAffinity"},
    {.name = "nvmlDeviceSetDefaultAutoBoostedClocksEnabled"},
    {.name = "nvmlDeviceSetDriverModel"},
    {.name = "nvmlDeviceSetEccMode"},
    {.name = "nvmlDeviceSetGpuOperationMode"},
    {.name = "nvmlDeviceSetNvLinkUtilizationControl"},
    {.name = "nvmlDeviceSetPersistenceMode"},
    {.name = "nvmlDeviceSetPowerManagementLimit"},
    {.name = "nvmlDeviceSetVirtualizationMode"},
    {.name = "nvmlDeviceValidateInforom"},
    {.name = "nvmlEventSetCreate"},
    {.name = "nvmlEventSetFree"},
    {.name = "nvmlEventSetWait"},
    {.name = "nvmlGetVgpuCompatibility"},
    {.name = "nvmlInit_v2"},
    {.name = "nvmlInitWithFlags"},
    {.name = "nvmlInternalGetExportTable"},
    {.name = "nvmlSystemGetCudaDriverVersion"},
    {.name = "nvmlSystemGetCudaDriverVersion_v2"},
    {.name = "nvmlSystemGetDriverVersion"},
    {.name = "nvmlSystemGetHicVersion"},
    {.name = "nvmlSystemGetNVMLVersion"},
    {.name = "nvmlSystemGetProcessName"},
    {.name = "nvmlSystemGetTopologyGpuSet"},
    {.name = "nvmlUnitGetCount"},
    {.name = "nvmlUnitGetDevices"},
    {.name = "nvmlUnitGetFanSpeedInfo"},
    {.name = "nvmlUnitGetHandleByIndex"},
    {.name = "nvmlUnitGetLedState"},
    {.name = "nvmlUnitGetPsuInfo"},
    {.name = "nvmlUnitGetTemperature"},
    {.name = "nvmlUnitGetUnitInfo"},
    {.name = "nvmlUnitSetLedState"},
    {.name = "nvmlVgpuInstanceGetEncoderCapacity"},
    {.name = "nvmlVgpuInstanceGetEncoderSessions"},
    {.name = "nvmlVgpuInstanceGetEncoderStats"},
    {.name = "nvmlVgpuInstanceGetFbUsage"},
    {.name = "nvmlVgpuInstanceGetFrameRateLimit"},
    {.name = "nvmlVgpuInstanceGetLicenseStatus"},
    {.name = "nvmlVgpuInstanceGetMetadata"},
    {.name = "nvmlVgpuInstanceGetType"},
    {.name = "nvmlVgpuInstanceGetUUID"},
    {.name = "nvmlVgpuInstanceGetVmDriverVersion"},
    {.name = "nvmlVgpuInstanceGetVmID"},
    {.name = "nvmlVgpuInstanceSetEncoderCapacity"},
    {.name = "nvmlVgpuTypeGetClass"},
    {.name = "nvmlVgpuTypeGetDeviceID"},
    {.name = "nvmlVgpuTypeGetFramebufferSize"},
    {.name = "nvmlVgpuTypeGetFrameRateLimit"},
    {.name = "nvmlVgpuTypeGetLicense"},
    {.name = "nvmlVgpuTypeGetMaxInstances"},
    {.name = "nvmlVgpuTypeGetName"},
    {.name = "nvmlVgpuTypeGetNumDisplayHeads"},
    {.name = "nvmlVgpuTypeGetResolution"},
    {.name = "nvmlDeviceGetFBCSessions"},
    {.name = "nvmlDeviceGetFBCStats"},
    {.name = "nvmlDeviceGetGridLicensableFeatures_v2"},
    {.name = "nvmlDeviceGetRetiredPages_v2"},
    {.name = "nvmlDeviceResetGpuLockedClocks"},
    {.name = "nvmlDeviceSetGpuLockedClocks"},
    {.name = "nvmlGetBlacklistDeviceCount"},
    {.name = "nvmlGetBlacklistDeviceInfoByIndex"},
    {.name = "nvmlVgpuInstanceGetAccountingMode"},
    {.name = "nvmlVgpuInstanceGetAccountingPids"},
    {.name = "nvmlVgpuInstanceGetAccountingStats"},
    {.name = "nvmlVgpuInstanceGetFBCSessions"},
    {.name = "nvmlVgpuInstanceGetFBCStats"},
    {.name = "nvmlVgpuTypeGetMaxInstancesPerVm"},
    {.name = "nvmlGetVgpuVersion"},
    {.name = "nvmlSetVgpuVersion"},
    {.name = "nvmlDeviceGetGridLicensableFeatures_v3"},
    {.name = "nvmlDeviceGetHostVgpuMode"},
    {.name = "nvmlDeviceGetPgpuMetadataString"},
    {.name = "nvmlVgpuInstanceGetEccMode"},
    {.name = "nvmlComputeInstanceDestroy"},
    {.name = "nvmlComputeInstanceGetInfo"},
    {.name = "nvmlDeviceCreateGpuInstance"},
    {.name = "nvmlDeviceGetArchitecture"},
    {.name = "nvmlDeviceGetAttributes"},
    {.name = "nvmlDeviceGetAttributes_v2"},
    {.name = "nvmlDeviceGetComputeInstanceId"},
    {.name = "nvmlDeviceGetCpuAffinityWithinScope"},
    {.name = "nvmlDeviceGetDeviceHandleFromMigDeviceHandle"},
    {.name = "nvmlDeviceGetGpuInstanceById"},
    {.name = "nvmlDeviceGetGpuInstanceId"},
    {.name = "nvmlDeviceGetGpuInstancePossiblePlacements"},
    {.name = "nvmlDeviceGetGpuInstanceProfileInfo"},
    {.name = "nvmlDeviceGetGpuInstanceRemainingCapacity"},
    {.name = "nvmlDeviceGetGpuInstances"},
    {.name = "nvmlDeviceGetMaxMigDeviceCount"},
    {.name = "nvmlDeviceGetMemoryAffinity"},
    {.name = "nvmlDeviceGetMigDeviceHandleByIndex"},
    {.name = "nvmlDeviceGetMigMode"},
    {.name = "nvmlDeviceGetRemappedRows"},
    {.name = "nvmlDeviceGetRowRemapperHistogram"},
    {.name = "nvmlDeviceIsMigDeviceHandle"},
    {.name = "nvmlDeviceSetMigMode"},
    {.name = "nvmlEventSetWait_v2"},
    {.name = "nvmlGpuInstanceCreateComputeInstance"},
    {.name = "nvmlGpuInstanceDestroy"},
    {.name = "nvmlGpuInstanceGetComputeInstanceById"},
    {.name = "nvmlGpuInstanceGetComputeInstanceProfileInfo"},
    {.name = "nvmlGpuInstanceGetComputeInstanceRemainingCapacity"},
    {.name = "nvmlGpuInstanceGetComputeInstances"},
    {.name = "nvmlGpuInstanceGetInfo"},
    {.name = "nvmlVgpuInstanceClearAccountingPids"},
    {.name = "nvmlVgpuInstanceGetMdevUUID"},
    {.name = "nvmlComputeInstanceGetInfo_v2"},
    {.name = "nvmlDeviceGetComputeRunningProcesses_v2"},
    {.name = "nvmlDeviceGetGraphicsRunningProcesses_v2"},
    {.name = "nvmlDeviceSetTemperatureThreshold"},
    {.name = "nvmlRetry_NvRmControl"},
    {.name = "nvmlVgpuInstanceGetGpuInstanceId"},
    {.name = "nvmlVgpuTypeGetGpuInstanceProfileId"},
    {.name = "nvmlDeviceCreateGpuInstanceWithPlacement"},
    {.name = "nvmlDeviceGetBusType"},
    {.name = "nvmlDeviceGetClkMonStatus"},
    {.name = "nvmlDeviceGetGpuInstancePossiblePlacements_v2"},
    {.name = "nvmlDeviceGetGridLicensableFeatures_v4"},
    {.name = "nvmlDeviceGetIrqNum"},
    {.name = "nvmlDeviceGetMPSComputeRunningProcesses_v2"},
    {.name = "nvmlDeviceGetNvLinkRemoteDeviceType"},
    {.name = "nvmlDeviceResetMemoryLockedClocks"},
    {.name = "nvmlDeviceSetMemoryLockedClocks"},
    {.name = "nvmlGetExcludedDeviceCount"},
    {.name = "nvmlGetExcludedDeviceInfoByIndex"},
    {.name = "nvmlVgpuInstanceGetLicenseInfo"},
    {.name = "nvmlDeviceClearFieldValues"},
    {.name = "nvmlDeviceGetAdaptiveClockInfoStatus"},
    {.name = "nvmlDeviceGetComputeRunningProcesses_v3"},
    {.name = "nvmlDeviceGetDefaultEccMode"},
    {.name = "nvmlDeviceGetDynamicPstatesInfo"},
    {.name = "nvmlDeviceGetFanControlPolicy_v2"},
    {.name = "nvmlDeviceGetGpcClkMinMaxVfOffset"},
    {.name = "nvmlDeviceGetGpcClkVfOffset"},
    {.name = "nvmlDeviceGetGpuFabricInfo"},
    {.name = "nvmlDeviceGetGpuInstanceProfileInfoV"},
    {.name = "nvmlDeviceGetGpuMaxPcieLinkGeneration"},
    {.name = "nvmlDeviceGetGraphicsRunningProcesses_v3"},
    {.name = "nvmlDeviceGetGspFirmwareMode"},
    {.name = "nvmlDeviceGetGspFirmwareVersion"},
    {.name = "nvmlDeviceGetJpgUtilization"},
    {.name = "nvmlDeviceGetMemClkMinMaxVfOffset"},
    {.name = "nvmlDeviceGetMemClkVfOffset"},
    {.name = "nvmlDeviceGetMemoryBusWidth"},
    {.name = "nvmlDeviceGetMinMaxClockOfPState"},
    {.name = "nvmlDeviceGetMinMaxFanSpeed"},
    {.name = "nvmlDeviceGetModuleId"},
    {.name = "nvmlDeviceGetMPSComputeRunningProcesses_v3"},
    {.name = "nvmlDeviceGetNumFans"},
    {.name = "nvmlDeviceGetNumGpuCores"},
    {.name = "nvmlDeviceGetOfaUtilization"},
    {.name = "nvmlDeviceGetPcieLinkMaxSpeed"},
    {.name = "nvmlDeviceGetPcieSpeed"},
    {.name = "nvmlDeviceGetPowerSource"},
    {.name = "nvmlDeviceGetSupportedClocksEventReasons"},
    {.name = "nvmlDeviceGetSupportedPerformanceStates"},
    {.name = "nvmlDeviceGetTargetFanSpeed"},
    {.name = "nvmlDeviceGetThermalSettings"},
    {.name = "nvmlDeviceGetVgpuCapabilities"},
    {.name = "nvmlGetVgpuDriverCapabilities"},
    {.name = "nvmlDeviceGetVgpuSchedulerCapabilities"},
    {.name = "nvmlDeviceGetVgpuSchedulerLog"},
    {.name = "nvmlDeviceGetVgpuSchedulerState"},
    {.name = "nvmlDeviceSetVgpuSchedulerState"},
    {.name = "nvmlDeviceSetConfComputeUnprotectedMemSize"},
    {.name = "nvmlDeviceSetDefaultFanSpeed_v2"},
    {.name = "nvmlDeviceSetFanControlPolicy"},
    {.name = "nvmlDeviceSetFanSpeed_v2"},
    {.name = "nvmlDeviceSetGpcClkVfOffset"},
    {.name = "nvmlDeviceSetMemClkVfOffset"},
    {.name = "nvmlDeviceSetNvLinkDeviceLowPowerThreshold"},
    {.name = "nvmlDeviceSetPowerManagementLimit_v2"},
    {.name = "nvmlGpmMetricsGet"},
    {.name = "nvmlGpmMigSampleGet"},
    {.name = "nvmlGpmQueryDeviceSupport"},
    {.name = "nvmlGpmQueryIfStreamingEnabled"},
    {.name = "nvmlGpmSampleAlloc"},
    {.name = "nvmlGpmSampleFree"},
    {.name = "nvmlGpmSampleGet"},
    {.name = "nvmlGpmSetStreamingEnabled"},
    {.name = "nvmlGpuInstanceCreateComputeInstanceWithPlacement"},
    {.name = "nvmlGpuInstanceGetComputeInstancePossiblePlacements"},
    {.name = "nvmlGpuInstanceGetComputeInstanceProfileInfoV"},
    {.name = "nvmlSystemGetConfComputeCapabilities"},
    {.name = "nvmlSystemGetConfComputeGpusReadyState"},
    {.name = "nvmlSystemGetConfComputeState"},
    {.name = "nvmlSystemGetNvlinkBwMode"},
    {.name = "nvmlSystemSetConfComputeGpusReadyState"},
    {.name = "nvmlSystemSetNvlinkBwMode"},
    {.name = "nvmlVgpuInstanceGetGpuPciId"},
    {.name = "nvmlVgpuInstanceGetLicenseInfo_v2"},
    {.name = "nvmlVgpuTypeGetCapabilities"},
    {.name = "nvmlDeviceGetCurrentClocksEventReasons"},
    {.name = "nvmlDeviceGetConfComputeProtectedMemoryUsage"},
    {.name = "nvmlDeviceGetConfComputeMemSizeInfo"},
    {.name = "nvmlDeviceGetConfComputeGpuCertificate"},
    {.name = "nvmlDeviceGetConfComputeGpuAttestationReport"},
    {.name = "nvmlDeviceGetRunningProcessDetailList"},
};

static void UNUSED bug_on() {
  BUILD_BUG_ON((sizeof(nvml_library_entry) / sizeof(nvml_library_entry[0])) !=
               NVML_ENTRY_END);

  BUILD_BUG_ON((sizeof(cuda_library_entry) / sizeof(cuda_library_entry[0])) !=
               CUDA_ENTRY_END);
}

/** register once set */
static pthread_once_t g_cuda_lib_init = PTHREAD_ONCE_INIT;
static pthread_once_t g_nvml_lib_init = PTHREAD_ONCE_INIT;
static pthread_once_t init_dlsym_flag = PTHREAD_ONCE_INIT;
static pthread_once_t init_host_indexes = PTHREAD_ONCE_INIT;

static int host_device_indexes[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

extern void* _dl_sym(void*, const char*, void*);
/* This is the symbol search function */
fp_dlsym real_dlsym = NULL;
void *lib_control;

extern int get_compatibility_mode(int *mode);
extern int get_mem_ratio(uint32_t index, double *ratio);
extern int get_mem_limit(uint32_t index, size_t *limit);
extern int get_core_limit(uint32_t index, int *limit);
extern int get_core_soft_limit(uint32_t index, int *limit);
extern int get_devices_uuid(char *uuids);
extern int get_mem_oversold(uint32_t index, int *limit);
extern int get_vmem_node_enabled(int *enabled);
extern int extract_container_id(char *path, char *container_id, size_t container_id_size);

// vmemory node lock
extern int device_vmem_write_lock(int ordinal);
extern int device_vmem_read_lock(int ordinal);
extern void device_vmem_unlock(int fd, int ordinal);

resource_data_t vgpu_config_temp = {
    .driver_version = {},
    .pod_uid = "",
    .pod_name = "",
    .pod_namespace = "",
    .container_name = "",
    .devices = {},
    .host_index = {},
    .device_count = 0,
    .compatibility_mode = 0,
    .sm_watcher = 0,
    .vmem_node = 0,
};

resource_data_t* g_vgpu_config = &vgpu_config_temp;

device_util_t* g_device_util = NULL;

memory_node_t memory_node_temp = {
    .dptr = 0,
    .bytes = 0,
    .node = LIST_HEAD_INIT(memory_node_temp.node)
};

memory_node_t* g_memory_node = &memory_node_temp;
static pthread_mutex_t g_memory_node_lock = PTHREAD_MUTEX_INITIALIZER;

device_vmemory_t* g_device_vmem = NULL;

char container_id[FILENAME_MAX] = {0};
char driver_version[FILENAME_MAX] = "1";

void init_real_dlsym() {
  if (real_dlsym == NULL) {
    real_dlsym = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
    lib_control = dlopen(CONTROLLER_DRIVER_FILE_PATH, RTLD_LAZY);
    if (real_dlsym == NULL) {
      real_dlsym = _dl_sym(RTLD_NEXT, "dlsym", dlsym);
    }
    if (real_dlsym == NULL) {
      LOGGER(FATAL, "real dlsym not found");
    }
  }
}

static void load_nvml_libraries() {
  void *table = NULL;
  char driver_filename[FILENAME_MAX];
  int i;

  init_real_dlsym();

  snprintf(driver_filename, FILENAME_MAX - 1, "%s.%s", DRIVER_ML_LIBRARY_PREFIX,
           driver_version);
  driver_filename[FILENAME_MAX - 1] = '\0';

  table = dlopen(driver_filename, RTLD_NOW | RTLD_NODELETE);
  if (unlikely(!table)) {
    LOGGER(FATAL, "can't find library %s", driver_filename);
  }

  for (i = 0; i < NVML_ENTRY_END; i++) {
    if (unlikely(nvml_library_entry[i].fn_ptr)) {
      continue;
    }
    LOGGER(DETAIL, "loading %s:%d", nvml_library_entry[i].name, i);
    nvml_library_entry[i].fn_ptr = real_dlsym(table, nvml_library_entry[i].name);
    if (unlikely(!nvml_library_entry[i].fn_ptr)) {
      nvml_library_entry[i].fn_ptr = real_dlsym(RTLD_NEXT,nvml_library_entry[i].name);
      if (unlikely(!nvml_library_entry[i].fn_ptr)) {
        LOGGER(VERBOSE, "can't find function %s in %s", nvml_library_entry[i].name,
              driver_filename);
      }
    }
  }

  LOGGER(INFO, "loaded nvml libraries");
  dlclose(table);
}

static void load_cuda_single_library(int idx) {
  void *table = NULL;
  char cuda_filename[FILENAME_MAX];

  init_real_dlsym();

  if (likely(cuda_library_entry[idx].fn_ptr)) {
    return;
  }
  snprintf(cuda_filename, FILENAME_MAX - 1, "%s.%s",
                CUDA_LIBRARY_PREFIX, driver_version);
  cuda_filename[FILENAME_MAX - 1] = '\0';

  table = dlopen(cuda_filename, RTLD_NOW | RTLD_NODELETE);
  if (unlikely(!table)) {
    LOGGER(FATAL, "can't find library %s", cuda_filename);
  }

  cuda_library_entry[idx].fn_ptr = real_dlsym(table, cuda_library_entry[idx].name);
  if (unlikely(!cuda_library_entry[idx].fn_ptr)) {
    LOGGER(VERBOSE, "can't find function %s in %s", cuda_library_entry[idx].name,
           cuda_filename);
  }

  dlclose(table);
}


static void load_nvml_single_library(int idx) {
  void *table = NULL;
  char driver_filename[FILENAME_MAX];

  init_real_dlsym();

  snprintf(driver_filename, FILENAME_MAX - 1, "%s.%s", DRIVER_ML_LIBRARY_PREFIX,
           driver_version);
  driver_filename[FILENAME_MAX - 1] = '\0';

  if (likely(nvml_library_entry[idx].fn_ptr)) {
    return;
  }

  table = dlopen(driver_filename, RTLD_NOW | RTLD_NODELETE);
  if (unlikely(!table)) {
    LOGGER(FATAL, "can't find library %s", driver_filename);
  }

  nvml_library_entry[idx].fn_ptr = real_dlsym(table, nvml_library_entry[idx].name);
  if (unlikely(!nvml_library_entry[idx].fn_ptr)) {
    LOGGER(VERBOSE, "can't find function %s in %s", nvml_library_entry[idx].name,
           driver_filename);
  }

  dlclose(table);
}

void load_cuda_libraries() {
  void *table = NULL;
  int i = 0;
  char cuda_filename[FILENAME_MAX];

  init_real_dlsym();

  snprintf(cuda_filename, FILENAME_MAX - 1, "%s.%s",
                CUDA_LIBRARY_PREFIX, driver_version);
  cuda_filename[FILENAME_MAX - 1] = '\0';

  table = dlopen(cuda_filename, RTLD_NOW | RTLD_NODELETE);
  if (unlikely(!table)) {
    LOGGER(FATAL, "can't find library %s", cuda_filename);
  }

  for (i = 0; i < CUDA_ENTRY_END; i++) {
    if (unlikely(cuda_library_entry[i].fn_ptr)) {
      continue;
    }
    LOGGER(DETAIL, "loading %s:%d", cuda_library_entry[i].name, i);
    cuda_library_entry[i].fn_ptr = real_dlsym(table, cuda_library_entry[i].name);
    if (unlikely(!cuda_library_entry[i].fn_ptr)) {
      cuda_library_entry[i].fn_ptr = real_dlsym(RTLD_NEXT,cuda_library_entry[i].name);
      if (unlikely(!cuda_library_entry[i].fn_ptr)) {
        LOGGER(VERBOSE, "can't find function %s in %s", cuda_library_entry[i].name,
              cuda_filename);
      }
    }
  }

  LOGGER(INFO,"loaded cuda libraries");
  dlclose(table);
}

static void matchRegex(const char *pattern, const char *matchString,
                       char *version) {
  regex_t regex;
  int reti;
  regmatch_t matches[1];
  char msgbuf[512];

  reti = regcomp(&regex, pattern, REG_EXTENDED);
  if (reti) {
    LOGGER(VERBOSE, "Could not compile regex: %s", DRIVER_VERSION_MATCH_PATTERN);
    return;
  }

  reti = regexec(&regex, matchString, 1, matches, 0);
  switch (reti) {
  case 0:
    strncpy(version, matchString + matches[0].rm_so,
            matches[0].rm_eo - matches[0].rm_so);
    version[matches[0].rm_eo - matches[0].rm_so] = '\0';
    break;
  case REG_NOMATCH:
    LOGGER(VERBOSE, "Regex does not match for string: %s", matchString);
    break;
  default:
    regerror(reti, &regex, msgbuf, sizeof(msgbuf));
    LOGGER(VERBOSE, "Regex match failed: %s", msgbuf);
  }

  regfree(&regex);
  return;
}

static void read_version_from_proc(char *version) {

  char *line = NULL;
  size_t len = 0;

  FILE *fp = fopen(DRIVER_VERSION_PATH, "r");
  if (fp == NULL) {
    LOGGER(VERBOSE, "can't open %s, error %s", DRIVER_VERSION_PATH, strerror(errno));
    return;
  }

  while ((getline(&line, &len, fp) != -1)) {
    if (strncmp(line, "NVRM", 4) == 0) {
      matchRegex(DRIVER_VERSION_MATCH_PATTERN, line, version);
      break;
    }
  }
  fclose(fp);
}

int strsplit(const char *s, char **dest, const char *sep) {
  char *token;
  int index = 0;
  char *src = (char *)malloc(strlen(s) + 1);
  strcpy(src, s);
  token = strtok(src, sep);
  while (token != NULL) {
    dest[index] = token;
    index += 1;
    token = strtok(NULL, sep);
  }
  return index;
}

int check_file_exist(const char *file_path) {
  if (access(file_path, F_OK) != -1) {
      return 0;
  } else {
      return 1;
  }
}

int mmap_file_to_config_path(resource_data_t** data) {
  const char* filename = CONTROLLER_CONFIG_FILE_PATH;
  if (unlikely(check_file_exist(filename))) {
    return 1;
  }
  int fd;
  int ret = 0;
  fd = open(filename, O_RDONLY | O_CLOEXEC);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", filename, strerror(errno));
    return 1;
  }
  struct stat sb;
  if (fstat(fd, &sb) == -1) {
    LOGGER(ERROR, "fstat failed: %s", strerror(errno));
    ret = 1;
    goto DONE;
  }
  if (sb.st_size != sizeof(resource_data_t)) {
    LOGGER(ERROR, "file size mismatch: expected %zu, got %lld",
                    sizeof(resource_data_t), (long long)sb.st_size);
    ret = 1;
    goto DONE;
  }
  *data = (resource_data_t*)mmap(NULL, sb.st_size, PROT_READ, MAP_PRIVATE, fd, 0);
  if (*data == MAP_FAILED) {
    LOGGER(ERROR, "mmap global config failed: %s", strerror(errno));
    ret = 1;
    goto DONE;
  }

DONE:
  close(fd);
  return ret;
}

int mmap_file_to_util_path(const char* filename, device_util_t** data) {
  if (unlikely(check_file_exist(filename))) {
    return 1;
  }
  int fd;
  int ret = 0;
  fd = open(filename, O_RDONLY | O_CLOEXEC);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", filename, strerror(errno));
    return 1;
  }
  struct stat sb;
  if (fstat(fd, &sb) == -1) {
    LOGGER(ERROR, "fstat failed: %s", strerror(errno));
    ret = 1;
    goto DONE;
  }
  if (sb.st_size != sizeof(device_util_t)) {
    LOGGER(ERROR, "file size mismatch: expected %zu, got %lld",
                    sizeof(device_util_t), (long long)sb.st_size);
    ret = 1;
    goto DONE;
  }
  *data = (device_util_t*)mmap(NULL, sb.st_size, PROT_READ, MAP_PRIVATE, fd, 0);
  if (*data == MAP_FAILED) {
    LOGGER(ERROR, "mmap sm watcher failed: %s", strerror(errno));
    ret = 1;
    goto DONE;
  }

DONE:
  close(fd);
  return ret;
}

int mmap_file_to_vmem_node(device_vmemory_t** data) {
  int fd;
  int created = 0;
  if (unlikely(check_file_exist(VMEMORY_NODE_PATH))) {
    mkdir(VMEMORY_NODE_PATH, 0755);
  }
  const char* filename = VMEMORY_NODE_FILE_PATH;
  if (unlikely(check_file_exist(filename))) {
    fd = open(filename, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
    if (unlikely(fd == -1)) {
      LOGGER(ERROR, "can't create %s, error %s", filename, strerror(errno));
      return 1;
    }
    created = 1;
    if (ftruncate(fd, sizeof(device_vmemory_t)) == -1) {
      LOGGER(ERROR, "ftruncate failed: %s", strerror(errno));
      close(fd);
      return 1;
    }
  } else {
    fd = open(filename, O_RDWR | O_CLOEXEC);
    if (unlikely(fd == -1)) {
      LOGGER(ERROR, "can't open %s, error %s", filename, strerror(errno));
      return 1;
    }
  }
  int ret = 0;
  struct stat sb;
  if (fstat(fd, &sb) == -1) {
    LOGGER(ERROR, "fstat failed: %s", strerror(errno));
    ret = 1;
    goto DONE;
  }
  if (!created && sb.st_size != sizeof(device_vmemory_t)) {
    LOGGER(ERROR, "file size mismatch: expected %zu, got %lld",
                   sizeof(device_vmemory_t), (long long)sb.st_size);
    ret = 1;
    goto DONE;
  }
  *data = (device_vmemory_t*)mmap(NULL, sb.st_size, PROT_READ|PROT_WRITE, MAP_SHARED, fd, 0);
  if (*data == MAP_FAILED) {
    LOGGER(ERROR, "mmap vmemory node failed: %s", strerror(errno));
    ret = 1;
    goto DONE;
  }
  if (created) {
    memset(*data, 0, sizeof(device_vmemory_t));
  }
DONE:
  close(fd);
  return ret;
}

void print_global_vgpu_config() {
  LOGGER(VERBOSE, "------------------print_global_vgpu_config------------------");
  LOGGER(VERBOSE, "pod name         : %s", g_vgpu_config->pod_name);
  LOGGER(VERBOSE, "pod namespace    : %s", g_vgpu_config->pod_namespace);
  LOGGER(VERBOSE, "pod uid          : %s", g_vgpu_config->pod_uid);
  LOGGER(VERBOSE, "container name   : %s", g_vgpu_config->container_name);
  LOGGER(VERBOSE, "gpu count        : %d", g_vgpu_config->device_count);
  LOGGER(VERBOSE, "CompatibilityMode: %d", g_vgpu_config->compatibility_mode);
  LOGGER(VERBOSE, "SM Watcher       : %s", g_vgpu_config->sm_watcher?"enabled" : "disabled");
  LOGGER(VERBOSE, "VMemory Node     : %s", g_vgpu_config->vmem_node?"enabled" : "disabled");
  for (int i = 0; i < g_vgpu_config->device_count; i++) {
    LOGGER(VERBOSE, "---------------------------GPU %d---------------------------", i);
    LOGGER(VERBOSE, "gpu uuid         : %s", g_vgpu_config->devices[i].uuid);
    LOGGER(VERBOSE, "memory limit     : %s", g_vgpu_config->devices[i].memory_limit? "enabled" : "disabled");
    LOGGER(VERBOSE, "+ real  memory   : %ld", g_vgpu_config->devices[i].real_memory);
    LOGGER(VERBOSE, "+ total memory   : %ld", g_vgpu_config->devices[i].total_memory);
    LOGGER(VERBOSE, "cores limit      : %s", g_vgpu_config->devices[i].core_limit? "enabled" : "disabled");
    LOGGER(VERBOSE, "+ hard limit     : %s", g_vgpu_config->devices[i].hard_limit? "enabled" : "disabled");
    LOGGER(VERBOSE, "+ hard cores     : %d", g_vgpu_config->devices[i].hard_core);
    LOGGER(VERBOSE, "+ soft cores     : %d", g_vgpu_config->devices[i].soft_core);
    LOGGER(VERBOSE, "memory oversold  : %s", g_vgpu_config->devices[i].memory_oversold? "enabled" : "disabled");
  }
  LOGGER(VERBOSE, "-----------------------------------------------------------");
}

int write_file_to_config_path(resource_data_t* data) {
  int wsize = 0;
  int ret = 0;
  if (unlikely(check_file_exist(VGPU_MANAGER_PATH))) {
    mkdir(VGPU_MANAGER_PATH, 0755);
  }
  if (unlikely(check_file_exist(VGPU_CONFIG_PATH))) {
    mkdir(VGPU_CONFIG_PATH, 0755);
  }
  int fd = open(CONTROLLER_CONFIG_FILE_PATH, O_CREAT | O_TRUNC | O_WRONLY, 0644);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    ret = 1;
    goto DONE;
  }
  wsize = (int)write(fd, (void*)data, sizeof(resource_data_t));
  if (wsize != sizeof(resource_data_t)) {
    LOGGER(ERROR, "can't write data to %s, error %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    ret = 1;
    goto DONE;
  }

  ret = mmap_file_to_config_path(&data);
  if (unlikely(ret)) {
    ret = 1;
    goto DONE;
  }

DONE:
  close(fd);
  return ret;
}

tid_dlsym tid_dlsyms[DLMAP_SIZE];
static int tid_dlsym_count = 0;
static pthread_mutex_t tid_dlsym_lock;

void init_tid_dlsyms(){
  pthread_mutex_init(&tid_dlsym_lock, NULL);
  tid_dlsym_count = 0;
  memset(tid_dlsyms, 0, sizeof(tid_dlsym) * DLMAP_SIZE);
}

int check_tid_dlsyms(int tid, void *pointer){
  int i;
  int cursor = (tid_dlsym_count < DLMAP_SIZE) ? tid_dlsym_count : DLMAP_SIZE;
  for (i = cursor-1; i >= 0; i--) {
    if (tid_dlsyms[i].tid == tid &&
      tid_dlsyms[i].pointer == pointer) {
      return 1;
    }
  }
  cursor = tid_dlsym_count % DLMAP_SIZE;
  tid_dlsyms[cursor].tid = tid;
  tid_dlsyms[cursor].pointer = pointer;
  tid_dlsym_count++;
  return 0;
}

extern entry_t cuda_hooks_entry[];
extern const int cuda_hook_nums;

extern entry_t nvml_hooks_entry[];
extern const int nvml_hook_nums;

FUNC_ATTR_VISIBLE void* dlsym(void* handle, const char* symbol) {
  pthread_once(&init_dlsym_flag, init_tid_dlsyms);

  LOGGER(DETAIL, "into dlsym %s", symbol);
  init_real_dlsym();

  if (handle == RTLD_NEXT) {
    int tid;
    void *h = real_dlsym(RTLD_NEXT,symbol);
    pthread_mutex_lock(&tid_dlsym_lock);
    tid = pthread_self();
    if (check_tid_dlsyms(tid, h)){
      LOGGER(WARNING, "recursive dlsym: %s",symbol);
      h = NULL;
    }
    pthread_mutex_unlock(&tid_dlsym_lock);
    return h;
  }

  int i;
  // hijack cuda
  if (strncmp(symbol, "cu", 2) == 0) {
    _load_necessary_data();
    if (likely(lib_control)) {
      void *f = real_dlsym(lib_control, symbol);
      if (likely(f)) {
        LOGGER(DETAIL, "search found cuda hook %s", symbol);
        return f;
      }
    }
    for (i = 0; i < cuda_hook_nums; i++) {
      if (unlikely(!strcmp(symbol, cuda_hooks_entry[i].name))) {
        LOGGER(DETAIL, "search found cuda hook %s", symbol);
        return cuda_hooks_entry[i].fn_ptr;
      }
    }
  }
  // hijack nvml
  if (strncmp(symbol, "nvml", 4) == 0) {
    _load_necessary_data();
    if (likely(lib_control)) {
      void *f = real_dlsym(lib_control, symbol);
      if (likely(f)) {
        LOGGER(DETAIL, "search found nvml hook %s", symbol);
        return f;
      }
    }
    for (i = 0; i < nvml_hook_nums; i++) {
      if (unlikely(!strcmp(symbol, nvml_hooks_entry[i].name))) {
        LOGGER(DETAIL, "search found nvml hook %s", symbol);
        return nvml_hooks_entry[i].fn_ptr;
      }
    }
  }
  return real_dlsym(handle, symbol);
}

int pid_exists(int pid) {
  if (pid <= 0) {
    return 1;
  }
  int result = kill(pid, 0);
  if (result == 0) {
    return 0;
  }
  switch (errno) {
  case ESRCH:
    return 1;
  case EPERM:
    return 0;
  }
  char path[PATH_MAX];
  snprintf(path, sizeof(path), "/proc/%d", pid);
  return check_file_exist(path);
}

void rm_vmem_node_by_non_existent_device_pid(int device_id, int pid) {
  unsigned int processes_size = g_device_vmem->devices[device_id].processes_size;
  for (int i = processes_size - 1; i >= 0; i--) {
    if (g_device_vmem->devices[device_id].processes[i].pid == pid) {
      g_device_vmem->devices[device_id].processes[i] = g_device_vmem->devices[device_id].processes[processes_size-1];
      g_device_vmem->devices[device_id].processes[processes_size-1].pid = 0;
      g_device_vmem->devices[device_id].processes[processes_size-1].used = 0;
      g_device_vmem->devices[device_id].processes_size--;
      processes_size--;
      continue;
    }
    if (pid_exists(g_device_vmem->devices[device_id].processes[i].pid) != 0) {
      LOGGER(WARNING, "detected that process %d does not exist, kicked out virtual memory node",
                       g_device_vmem->devices[device_id].processes[i].pid);
      g_device_vmem->devices[device_id].processes[i] = g_device_vmem->devices[device_id].processes[processes_size-1];
      g_device_vmem->devices[device_id].processes[processes_size-1].pid = 0;
      g_device_vmem->devices[device_id].processes[processes_size-1].used = 0;
      g_device_vmem->devices[device_id].processes_size--;
      processes_size--;
      continue;
    }
  }
}

void rm_vmem_node_by_device_pid(int device_id, int pid) {
  int index = -1;
  unsigned int processes_size = g_device_vmem->devices[device_id].processes_size;
  for (int i = 0; i < processes_size; i++) {
    if (g_device_vmem->devices[device_id].processes[i].pid == pid) {
      index = i;
      break;
    }
  }
  if (index >= 0) {
    g_device_vmem->devices[device_id].processes[index] = g_device_vmem->devices[device_id].processes[processes_size-1];
    g_device_vmem->devices[device_id].processes[processes_size-1].pid = 0;
    g_device_vmem->devices[device_id].processes[processes_size-1].used = 0;
    g_device_vmem->devices[device_id].processes_size--;
  }
}

void exit_cleanup_vmem_nodes() {
 int pid = getpid();
 LOGGER(WARNING, "process program %d exits", pid);
 if (g_device_vmem != NULL) {
   for (int index = 0; index < MAX_DEVICE_COUNT; index++) {
     if (g_device_vmem->devices[index].processes_size == 0) {
       continue;
     }
     int fd = device_vmem_write_lock(index);
     if (fd < 0) continue;
     rm_vmem_node_by_device_pid(index, pid);
     __sync_synchronize();
     device_vmem_unlock(fd, index);
   }
 }
}

void check_vmem_nodes() {
  if (g_device_vmem != NULL) {
    int pid = getpid();
    for (int index = 0; index < MAX_DEVICE_COUNT; index++) {
      if (g_device_vmem->devices[index].processes_size == 0) {
        continue;
      }
      int fd = device_vmem_write_lock(index);
      if (fd < 0) continue;
      rm_vmem_node_by_non_existent_device_pid(index, pid);
      __sync_synchronize();
      device_vmem_unlock(fd, index);
    }
  }
}

void malloc_gpu_virt_memory(CUdeviceptr dptr, size_t bytes, int device_id) {
  memory_node_t *new_node = NULL;
  new_node = (memory_node_t*) malloc(sizeof(memory_node_t));
  if (!new_node) {
    LOGGER(FATAL, "failed to allocate memory node");
  }
  LOGGER(VERBOSE, "malloc virt memory to device %d, dptr %lld, size %ld", device_id, dptr, bytes);

  new_node->dptr = dptr;
  new_node->bytes = bytes;
  INIT_LIST_HEAD(&new_node->node);

  pthread_mutex_lock(&g_memory_node_lock);
  list_add(&new_node->node, &g_memory_node->node);
  pthread_mutex_unlock(&g_memory_node_lock);

  if (g_device_vmem != NULL) {
    device_id = host_device_indexes[device_id];
    int fd = device_vmem_write_lock(device_id);
    if (fd < 0) return;
    int pid = getpid();
    int found = 0;
    unsigned int processes_size = g_device_vmem->devices[device_id].processes_size;
    for (int i = 0; i < processes_size; i++) {
      if (g_device_vmem->devices[device_id].processes[i].pid == pid) {
        g_device_vmem->devices[device_id].processes[i].used += bytes;
        found = 1;
        break;
      }
    }
    if (!found) {
      g_device_vmem->devices[device_id].processes[processes_size].pid = pid;
      g_device_vmem->devices[device_id].processes[processes_size].used = bytes;
      g_device_vmem->devices[device_id].processes_size++;
    }
    device_vmem_unlock(fd, device_id);
  }
}

void free_gpu_virt_memory(CUdeviceptr dptr, int device_id) {
  int found = 0;
  memory_node_t *entry_tmp = NULL;
  struct list_head *iter;
  list_for_each(iter, &g_memory_node->node) {
    entry_tmp = container_of(iter, memory_node_t, node);
    if (entry_tmp == NULL) continue;
    if (entry_tmp->dptr == dptr) {
      found = 1;
      break;
    }
  }
  if (!found) return;

  size_t size = entry_tmp->bytes;
  LOGGER(VERBOSE, "free virt memory to device %d, dptr %lld, size %ld", device_id, dptr, size);

  pthread_mutex_lock(&g_memory_node_lock);
  list_del(&entry_tmp->node);
  free(entry_tmp);
  pthread_mutex_unlock(&g_memory_node_lock);

  if (g_device_vmem != NULL) {
    device_id = host_device_indexes[device_id];
    int fd = device_vmem_write_lock(device_id);
    if (fd < 0) return;

    int pid = getpid();
    for (int i = 0; i< g_device_vmem->devices[device_id].processes_size; i++) {
      if (g_device_vmem->devices[device_id].processes[i].pid == pid) {
        g_device_vmem->devices[device_id].processes[i].used =
           (g_device_vmem->devices[device_id].processes[i].used >= size) ?
           (g_device_vmem->devices[device_id].processes[i].used - size) : 0;
        break;
      }
    }
    device_vmem_unlock(fd, device_id);
  }
}

void get_used_gpu_virt_memory(void *arg, int device_id) {
  size_t count = 0;
  size_t *used_memory = arg;
  if (g_vgpu_config->vmem_node && g_device_vmem != NULL) {
    device_id = host_device_indexes[device_id];
    int fd = device_vmem_read_lock(device_id);
    if (fd < 0) goto DONE;
    for (int i = 0; i < g_device_vmem->devices[device_id].processes_size; i++) {
      count += g_device_vmem->devices[device_id].processes[i].used;
    }
    device_vmem_unlock(fd, device_id);
  }
DONE:
  *used_memory = count;
}

static volatile int init_config_flag = 0;

int load_controller_configuration() {
  if (likely(init_config_flag)) {
    return 0;
  }
  int ret = 1;
  if (strlen(container_id) == 0) {
    ret = extract_container_id(HOST_CGROUP_PATH, container_id, FILENAME_MAX);
    if (!ret) {
      LOGGER(VERBOSE, "find current container id: %s", container_id);
    }
  }

  ret = mmap_file_to_config_path(&g_vgpu_config);
  if (likely(ret == 0)) {
    init_config_flag = 1;
    print_global_vgpu_config();
    if (g_vgpu_config->sm_watcher) {
      ret = mmap_file_to_util_path(CONTROLLER_SM_UTIL_FILE_PATH, &g_device_util);
      if (ret) {
        LOGGER(FATAL, "mmap sm watcher file failed");
      }
    }
    if (g_vgpu_config->vmem_node) {
      ret = mmap_file_to_vmem_node(&g_device_vmem);
      if (ret) {
        LOGGER(FATAL, "mmap vmem nodes file failed");
      }
      check_vmem_nodes();
      if (atexit(exit_cleanup_vmem_nodes) != 0) {
        LOGGER(FATAL ,"register exit handler failed: %d", errno);
      }
    }
    goto DONE;
  }

  ret = get_compatibility_mode(&g_vgpu_config->compatibility_mode);
  if (unlikely(ret)) {
    LOGGER(WARNING, "not defined env compatibility mode");
  }
  char *pod_name = getenv("VGPU_POD_NAME");
  if (likely(pod_name != NULL)){
    strncpy(g_vgpu_config->pod_name, pod_name, sizeof(g_vgpu_config->pod_name)-1);
  }
  char *pod_namespace = getenv("VGPU_POD_NAMESPACE");
  if (likely(pod_namespace != NULL)){
    strncpy(g_vgpu_config->pod_namespace, pod_namespace, sizeof(g_vgpu_config->pod_namespace)-1);
  }
  char *pod_uid = getenv("VGPU_POD_UID");
  if (likely(pod_uid != NULL)){
    strncpy(g_vgpu_config->pod_uid, pod_uid, sizeof(g_vgpu_config->pod_uid)-1);
  }
  char *container_name = getenv("VGPU_CONTAINER_NAME");
  if (likely(container_name != NULL)){
    strncpy(g_vgpu_config->container_name, container_name, sizeof(g_vgpu_config->container_name)-1);
  }

  char uuids[768];
  ret = get_devices_uuid(uuids);
  if (unlikely(ret)) {
    LOGGER(ERROR, "not found gpu devices uuid");
    goto DONE;
  }

  int hard_cores = 0;
  int soft_cores = 0;
  double ratio = 1; // default ratio = 1
  int oversold = 0; // default disable oversold
  size_t real_memory = 0;
  char *gpu_uuids[MAX_DEVICE_COUNT];
  int vnode_enable = 0;
  g_vgpu_config->device_count = strsplit(uuids, gpu_uuids, ",");
  get_vmem_node_enabled(&vnode_enable);
  g_vgpu_config->vmem_node = vnode_enable;
  for (int i = 0; i < g_vgpu_config->device_count; i++) {
    strcpy(g_vgpu_config->devices[i].uuid, gpu_uuids[i]);
    ret = get_mem_limit(i, &g_vgpu_config->devices[i].total_memory);
    if (unlikely(ret)) {
      LOGGER(VERBOSE, "gpu device %d turn off memory limit", i);
      g_vgpu_config->devices[i].memory_limit = 0;
    } else {
      g_vgpu_config->devices[i].memory_limit = 1;
    }
    ret = get_mem_oversold(i, &oversold);
    if (unlikely(ret)) {
      LOGGER(ERROR, "get device %d memory oversold failed", i);
      oversold = 0; // default disable oversold
    }
    ret = get_mem_ratio(i, &ratio);
    if (unlikely(ret)) {
      LOGGER(ERROR, "get device %d memory ratio failed", i);
      ratio = 1; // default ratio = 1
    }
    real_memory = g_vgpu_config->devices[i].total_memory;
    if (ratio > 1) {
      real_memory /= ratio;
      g_vgpu_config->devices[i].memory_oversold = 1;
    } else {
      g_vgpu_config->devices[i].memory_oversold = oversold;
    }
    g_vgpu_config->devices[i].real_memory = real_memory;

    ret = get_core_limit(i, &hard_cores);
    if (unlikely(ret)) {
      LOGGER(VERBOSE, "get device %d core limit failed", i);
      hard_cores = 0;
    }
    if (hard_cores > 0) {
      g_vgpu_config->devices[i].core_limit = 1;
      g_vgpu_config->devices[i].hard_limit = 1;
      g_vgpu_config->devices[i].hard_core = hard_cores;
      ret = get_core_soft_limit(i, &soft_cores);
      if (unlikely(ret)) {
        LOGGER(VERBOSE, "get device %d core soft limit failed", i);
        soft_cores = 0;
      }
      if (soft_cores > 0) {
        LOGGER(VERBOSE, "gpu device %d turn up core soft limit", i);
        g_vgpu_config->devices[i].hard_limit = 0;
        g_vgpu_config->devices[i].soft_core = soft_cores;
      }
    } else {
      LOGGER(VERBOSE, "gpu device %d turn off core limit", i);
      g_vgpu_config->devices[i].core_limit = 0;
      g_vgpu_config->devices[i].hard_limit = 0;
    }
  }

  ret = write_file_to_config_path(g_vgpu_config);
  if (unlikely(ret)) {
    LOGGER(ERROR, "failed to write vgpu config file %s", CONTROLLER_CONFIG_FILE_PATH);
    goto DONE;
  }
  print_global_vgpu_config();
  if (g_vgpu_config->vmem_node) {
    ret = mmap_file_to_vmem_node(&g_device_vmem);
    if (ret) {
      LOGGER(FATAL, "mmap vmem nodes file failed");
    }
    check_vmem_nodes();
    if (atexit(exit_cleanup_vmem_nodes) != 0) {
      LOGGER(FATAL ,"register exit handler failed: %d", errno);
    }
  }
  ret = 0;
  init_config_flag = 1;
DONE:
  return ret;
}

void get_device_and_uuid_and_hostindex(int device_id, nvmlDevice_t *device, char *uuid, unsigned int uuid_length, int *hindex) {
  nvmlReturn_t rt;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, device_id, device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex, device_id, device);
  } else {
    rt = NVML_ERROR_FUNCTION_NOT_FOUND;
    LOGGER(WARNING, "nvmlDeviceGetHandleByIndex function not found");
  }
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlDeviceGetHandleByIndex failed, device %d, return %d", device_id, rt);
  }
  rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetUUID, *device, uuid, uuid_length);
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlDeviceGetUUID failed, device %d, return %d", device_id, rt);
  }

  for (int index = 0; index < MAX_DEVICE_COUNT; index++) {
    if (strcmp(g_vgpu_config->host_index[index], uuid) == 0) {
      *hindex = index;
      LOGGER(VERBOSE, "find the host location %d corresponding to device %d", index, device_id);
      break;
    }
  }
}


void init_host_device_indexes() {
  nvmlReturn_t rt;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlInitWithFlags))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlInitWithFlags, 0);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlInit_v2))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlInit_v2);
  } else {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlInit);
  }
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlInit failed, return %d", rt);
  }
  unsigned int device_count;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetCount, &device_count);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount_v2))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetCount_v2, &device_count);
  } else {
    rt = NVML_ERROR_FUNCTION_NOT_FOUND;
    LOGGER(WARNING, "nvmlDeviceGetCount function not found");
  }
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlDeviceGetCount failed, return %d", rt);
  }
  for (int device_id = 0; device_id < device_count; device_id++) {
    char uuid[UUID_BUFFER_SIZE];
    nvmlDevice_t device;
    get_device_and_uuid_and_hostindex(device_id, &device, uuid, UUID_BUFFER_SIZE, &host_device_indexes[device_id]);
  }
}

void _load_necessary_data() {
  // First, determine the driver version
  read_version_from_proc(driver_version);
  load_cuda_single_library(CUDA_ENTRY_ENUM(cuDriverGetVersion));
  // Initialize the driver library
  pthread_once(&g_nvml_lib_init, load_nvml_libraries);
  pthread_once(&g_cuda_lib_init, load_cuda_libraries);
  // Read global configuration
  load_controller_configuration();
}

void load_necessary_data() {
  _load_necessary_data();
  pthread_once(&init_host_indexes, init_host_device_indexes);
}