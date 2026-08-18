/*
 * Tencent is pleased to support the open source community by making TKEStack
 * available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 * Copyright 2024-2026 coldzerofear
 * Modifications made for the vgpu-manager project by coldzerofear.
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

#include <errno.h>
#include <fcntl.h>
#include <dlfcn.h>
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
    //{.name = "cuCtxEnablePeerAccess"},
    //{.name = "cuCtxDisablePeerAccess"},
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
    //{.name = "cuGetExportTable"},
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
    {.name = "cuGraphAddDependencies_v2"},
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
    {.name = "cuGraphGetEdges_v2"},
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
    {.name = "cuGraphNodeGetDependencies_v2"},
    {.name = "cuGraphNodeGetDependentNodes"},
    {.name = "cuGraphNodeGetDependentNodes_v2"},
    {.name = "cuGraphNodeGetType"},
    {.name = "cuGraphRemoveDependencies"},
    {.name = "cuGraphRemoveDependencies_v2"},
    {.name = "cuImportExternalMemory"},
    {.name = "cuImportExternalSemaphore"},
    {.name = "cuLaunchHostFunc"},
    {.name = "cuLaunchHostFunc_ptsz"},
    {.name = "cuSignalExternalSemaphoresAsync"},
    {.name = "cuSignalExternalSemaphoresAsync_ptsz"},
//    {.name = "cuStreamBeginCapture"},
//    {.name = "cuStreamBeginCapture_ptsz"},
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
//    {.name = "cuStreamBeginCapture_v2"},
//    {.name = "cuStreamBeginCapture_v2_ptsz"},
    {.name = "cuStreamGetCaptureInfo"},
    {.name = "cuStreamGetCaptureInfo_ptsz"},
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
    {.name = "cuStreamGetCaptureInfo_v2"},
    {.name = "cuStreamGetCaptureInfo_v2_ptsz"},
    {.name = "cuStreamGetCaptureInfo_v3"},
    {.name = "cuStreamGetCaptureInfo_v3_ptsz"},
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
//    {.name = "cuModuleGetLoadingMode"},
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
//    {.name = "cuLibraryGetGlobal"},
//    {.name = "cuLibraryGetKernel"},
//    {.name = "cuLibraryGetManaged"},
//    {.name = "cuLibraryGetModule"},
//    {.name = "cuLibraryGetUnifiedFunction"},
//    {.name = "cuLibraryLoadData"},
//    {.name = "cuLibraryLoadFromFile"},
//    {.name = "cuLibraryUnload"},
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
    {.name = "nvmlDeviceGetProcessesUtilizationInfo"},
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
    {.name = "nvmlDeviceGetNumaNodeId"},
    {.name = "nvmlDeviceGetCapabilities"},
};

static void UNUSED bug_on() {
  BUILD_BUG_ON((sizeof(nvml_library_entry) / sizeof(nvml_library_entry[0])) !=
               NVML_ENTRY_END);

  BUILD_BUG_ON((sizeof(cuda_library_entry) / sizeof(cuda_library_entry[0])) !=
               CUDA_ENTRY_END);
}

/** register once set */
static pthread_once_t g_cuda_ver_init = PTHREAD_ONCE_INIT;
static pthread_once_t g_cuda_lib_init = PTHREAD_ONCE_INIT;
static pthread_once_t g_nvml_lib_init = PTHREAD_ONCE_INIT;
static pthread_once_t init_dlsym_flag = PTHREAD_ONCE_INIT;
static pthread_once_t init_nvml_host_index = PTHREAD_ONCE_INIT;
/* Guards the one-time pthread_atfork(NULL, NULL, child_after_fork) call.
 * Intentionally NOT reset by child_after_fork() in the child -- glibc's
 * atfork handler list is process-local data, inherited via COW at fork,
 * so the child already has the handler registered. Resetting would cause
 * load_necessary_data() in the child to call pthread_atfork again,
 * accumulating an extra registration per fork generation. */
static pthread_once_t g_atfork_init = PTHREAD_ONCE_INIT;
static pthread_once_t g_controller_config_init = PTHREAD_ONCE_INIT;
static pthread_once_t g_reset_cuda_index_init = PTHREAD_ONCE_INIT;

extern int get_compatibility_mode(int *mode);
extern int get_mem_ratio(uint32_t index, double *ratio);
extern int get_mem_limit(uint32_t index, size_t *limit);
extern int get_core_limit(uint32_t index, int *limit);
extern int get_core_soft_limit(uint32_t index, int *limit);
extern int get_manager_device_uuid(uint32_t index, char *uuid, size_t uuid_size);
extern int get_manager_device_uuids(char *uuids, size_t uuids_size);
extern int get_nvidia_device_uuids(char *uuids, size_t uuids_size);
extern int get_mem_oversold(uint32_t index, int *limit);
extern int get_vmem_node_enabled(int *enabled);
extern int file_exist(const char *file_path);
extern int pid_exist(int pid);
extern int is_zombie_proc(int pid);
extern int get_sm_watcher_enabled(int *i);
extern char* _getenv(const char* name);
/* This is the symbol search function */
fp_dlsym real_dlsym = NULL;
void *lib_control;

// virtual memory node lock
extern int device_vmem_write_lock(int ordinal);
extern int device_vmem_read_lock(int ordinal);
extern void device_vmem_unlock(int fd, int ordinal);

resource_data_t vgpu_config_init = {
    .magic = CONFIG_MAGIC,
    .layout_version = CONFIG_LAYOUT_VERSION,
    .region_size = sizeof(resource_data_t),
    .device_count = MAX_DEVICE_COUNT,
    .cuda_version = {},
    .driver_version = "",
    .pod_uid = "",
    .pod_name = "",
    .pod_namespace = "",
    .container_name = "",
    .devices = {},
    .compatibility_mode = 0,
    .sm_watcher = 0,
    .vmem_node = 0,
    .reg_uuid = "",
};

resource_data_t* g_vgpu_config = NULL;

device_util_t* g_device_util = NULL;

memory_node_t memory_node_temp = {
    .dptr = 0,
    .bytes = 0,
    .node = LIST_HEAD_INIT(memory_node_temp.node)
};

memory_node_t* g_memory_node = &memory_node_temp;
static pthread_mutex_t g_memory_node_lock = PTHREAD_MUTEX_INITIALIZER;

device_vmemory_t* g_device_vmem = NULL;
char driver_version[FILENAME_MAX] = "1";

void init_real_dlsym() {
  if (real_dlsym == NULL) {
    /* Probe newest-first. CUDA 12 / PyTorch 2.x toolchains link against
     * dlsym@GLIBC_2.34 (libdl merge), so a 2.22-capped list would miss
     * them and fall back to a compat dlsym whose RTLD_NEXT walk differs
     * from the version the framework actually invokes. */
    const char* glibc_versions[] = {
      "GLIBC_2.34",   // glibc 2.34+ (libdl merged into libc)
      "GLIBC_2.22",
      "GLIBC_2.18",
      "GLIBC_2.17",   // arm64 baseline
      "GLIBC_2.10",
      "GLIBC_2.4",
      "GLIBC_2.3",
      "GLIBC_2.2.5",  // amd64 baseline
      NULL
    };
    for (int i = 0; glibc_versions[i] != NULL; i++) {
      real_dlsym = dlvsym(RTLD_NEXT, "dlsym", glibc_versions[i]);
      if (real_dlsym) {
        LOGGER(INFO, "find the dlsym version: %s", glibc_versions[i]);
        break;
      }
    }
    if (unlikely(!real_dlsym)) {
      /* Last resort: pull dlsym out of libc.so.6 directly. We deliberately
       * do NOT fall back to _dl_sym(GLIBC_PRIVATE) -- it was effectively
       * removed by the glibc 2.34 libdl/libpthread merge and depending on
       * it breaks library load on modern distributions (Ubuntu 22.04+). */
      void *libc_handle = dlopen("libc.so.6", RTLD_LAZY);
      if (libc_handle) {
        real_dlsym = dlsym(libc_handle, "dlsym");
      }
      if (!real_dlsym) {
        LOGGER(FATAL, "unable to find the real dlsym");
      }
    }
  }
  if (lib_control == NULL) {
    lib_control = dlopen(CONTROLLER_DRIVER_FILE_PATH, RTLD_LAZY);
  }
}

static void load_nvml_libraries() {
  void *table = NULL;
  char driver_filename[FILENAME_MAX];
  int i;

  init_real_dlsym();

  snprintf(driver_filename, FILENAME_MAX - 1, "%s.%s", DRIVER_ML_LIBRARY_PREFIX, driver_version);
  driver_filename[FILENAME_MAX - 1] = '\0';

  table = dlopen(driver_filename, RTLD_NOW | RTLD_NODELETE);
  if (unlikely(!table)) {
    LOGGER(FATAL, "can't find library %s", driver_filename);
  }
  int entry_count = 0;
  for (i = 0; i < NVML_ENTRY_END; i++) {
    if (unlikely(nvml_library_entry[i].fn_ptr)) {
      entry_count++;
      continue;
    }
    LOGGER(DETAIL, "loading %s:%d", nvml_library_entry[i].name, i);
    nvml_library_entry[i].fn_ptr = real_dlsym(table, nvml_library_entry[i].name);
    if (unlikely(!nvml_library_entry[i].fn_ptr)) {
      nvml_library_entry[i].fn_ptr = real_dlsym(RTLD_NEXT,nvml_library_entry[i].name);
      if (unlikely(!nvml_library_entry[i].fn_ptr)) {
        LOGGER(VERBOSE, "can't find function %s in %s", nvml_library_entry[i].name, driver_filename);
        continue;
      }
    }
    entry_count++;
  }

  LOGGER(INFO, "loaded nvml libraries: %d entries", entry_count);
  dlclose(table);
}

static void load_cuda_single_library(int idx) {
  void *table = NULL;
  char cuda_filename[FILENAME_MAX];

  init_real_dlsym();
  if (likely(cuda_library_entry[idx].fn_ptr)) {
    return;
  }

  snprintf(cuda_filename, FILENAME_MAX - 1, "%s.%s", CUDA_LIBRARY_PREFIX, driver_version);
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
  if (likely(nvml_library_entry[idx].fn_ptr)) {
    return;
  }

  snprintf(driver_filename, FILENAME_MAX - 1, "%s.%s", DRIVER_ML_LIBRARY_PREFIX, driver_version);
  driver_filename[FILENAME_MAX - 1] = '\0';

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

extern entry_t cuda_hooks_entry[];
extern const int cuda_hook_nums;

/* ---- driver-pointer routing for cuGetProcAddress ------------------------- *
 *
 * cuGetProcAddress picks an ABI-specific symbol (e.g. cuCtxCreate_v2/_v3/_v4,
 * or the _ptsz twin for per-thread streams) based on cudaVersion/flags.
 * Substituting a hook by base name risks the wrong ABI, so instead we look up
 * the exact pointer the driver returned and match it to the hook with the
 * same exact symbol name -- an ABI mismatch becomes structurally impossible.
 * This is verified once at runtime (getproc_probe); the old name-based
 * blacklist stays as a fallback if verification fails. */
typedef struct {
  void       *real_fn;   /* pointer the driver hands out for this exact symbol */
  void       *hook_fn;   /* our hook of the same name, or NULL if we hook none */
  const char *name;      /* the exact symbol name, e.g. "cuCtxCreate_v4"       */
} driver_route_t;

static driver_route_t g_routes[CUDA_ENTRY_END];
static int g_routes_n = 0;

static int route_cmp(const void *a, const void *b) {
  const driver_route_t *ra = a, *rb = b;
  if (ra->real_fn != rb->real_fn) {
    return (ra->real_fn > rb->real_fn) - (ra->real_fn < rb->real_fn);
  }
  return strcmp(ra->name, rb->name);
}

/* Build the pointer -> hook index. Called once, after the driver table is
 * resolved; read-only from then on, so lookups need no locking. */
static void build_driver_routes(void) {
  g_routes_n = 0;
  for (int i = 0; i < CUDA_ENTRY_END; i++) {
    void *real = cuda_library_entry[i].fn_ptr;
    if (!real) continue;                      /* symbol absent on this driver */
    void *hook = NULL;
    if (lib_control) {
      hook = real_dlsym(lib_control, cuda_library_entry[i].name);
    }
    if (!hook) {
      for (int j = 0; j < cuda_hook_nums; j++) {
        if (!strcmp(cuda_library_entry[i].name, cuda_hooks_entry[j].name)) {
          hook = cuda_hooks_entry[j].fn_ptr;
          break;
        }
      }
    }
    g_routes[g_routes_n].real_fn = real;
    g_routes[g_routes_n].hook_fn = hook;
    g_routes[g_routes_n].name    = cuda_library_entry[i].name;
    g_routes_n++;
  }
  qsort(g_routes, g_routes_n, sizeof(g_routes[0]), route_cmp);

  /* Distinct names can alias to one address (an unversioned name that just
   * points at the current version). Give every entry sharing that address
   * whichever hook the group has -- the address IS the function. */
  for (int i = 0; i < g_routes_n; ) {
    int j = i;
    void *hook = NULL;
    while (j < g_routes_n && g_routes[j].real_fn == g_routes[i].real_fn) {
      if (!hook && g_routes[j].hook_fn) hook = g_routes[j].hook_fn;
      j++;
    }
    if (hook) for (int k = i; k < j; k++) g_routes[k].hook_fn = hook;
    i = j;
  }
  LOGGER(INFO, "driver route index built: %d entries", g_routes_n);
}

/* Split a CUDA symbol into the three parts its name is built from:
 *
 *   cuLaunchKernel_v2_ptsz  ->  base "cuLaunchKernel", version 2, suffix PTSZ
 *   cuStreamSynchronize_ptds ->  base "cuStreamSynchronize", version 0, PTDS
 *   cuInit                  ->  base "cuInit", version 0, suffix NONE
 *
 * Version 0 and suffix NONE mean "the name does not say", which is the whole
 * point: those are the components cuGetProcAddress decides for the caller. */
#define SFX_NONE 0
#define SFX_PTSZ 1
#define SFX_PTDS 2

static void split_symbol(const char *s, size_t *base_len, int *ver, int *sfx) {
  size_t len = strlen(s);

  *sfx = SFX_NONE;
  if (len > 5) {
    if      (!strcmp(s + len - 5, "_ptsz")) { *sfx = SFX_PTSZ; len -= 5; }
    else if (!strcmp(s + len - 5, "_ptds")) { *sfx = SFX_PTDS; len -= 5; }
  }

  *ver = 0;
  size_t i = len;
  while (i > 0 && s[i - 1] >= '0' && s[i - 1] <= '9') i--;
  if (i < len && i >= 2 && s[i - 1] == 'v' && s[i - 2] == '_') {
    for (size_t d = i; d < len; d++) *ver = *ver * 10 + (s[d] - '0');
    len = i - 2;
  }

  *base_len = len;
}

/* Could `cand` be what the caller meant when it asked for `req`? Base name
 * must match exactly; a version/suffix the request states is pinned, one it
 * omits is left for the driver to choose (e.g. "cuMemAlloc" matches _v2/_v3/
 * _v4, but "cuMemAlloc_v2" matches only itself). Leaving the suffix open is
 * what lets CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM route to the _ptsz
 * hook even though the requested name never says "ptsz". This only bounds
 * the search -- the driver's own returned pointer picks the actual match. */
static int symbol_in_family(const char *cand, const char *req) {
  size_t cb, rb;
  int cv, rv, cs, rs;

  split_symbol(cand, &cb, &cv, &cs);
  split_symbol(req,  &rb, &rv, &rs);

  if (cb != rb || memcmp(cand, req, cb) != 0) return 0;   /* different family */
  if (rv != 0 && rv != cv) return 0;                      /* version pinned   */
  if (rs != SFX_NONE && rs != cs) return 0;               /* suffix pinned    */
  return 1;
}

/* Resolve the pointer cuGetProcAddress produced for `symbol` to our hook.
 * The pointer identifies exactly which driver entry point was chosen; the
 * family check confirms it matches what the caller asked for -- together
 * they pin one entry point exactly, with nothing guessed from cudaVersion.
 *
 * *name is set whenever the pointer is identified, even when we hook no
 * version of it -- that tells the caller to keep the driver's pointer as-is
 * rather than fall back to name-based substitution, which could otherwise
 * bind a base-named hook to a version whose ABI it doesn't actually have. */
void* lookup_cuda_hook_ptr(void *real_fn, const char *symbol, const char **name) {
  if (name) *name = NULL;

  int lo = 0, hi = g_routes_n - 1, at = -1;
  while (lo <= hi) {
    int mid = lo + (hi - lo) / 2;
    void *cur = g_routes[mid].real_fn;
    if      (cur < real_fn) lo = mid + 1;
    else if (cur > real_fn) hi = mid - 1;
    else { at = mid; break; }
  }
  if (at < 0) return NULL;                    /* not a driver entry point we know */

  /* Widen to the whole run of names sharing this address; aliases put more than
   * one there. Runs are one or two entries in practice. */
  int i = at, j = at;
  while (i > 0 && g_routes[i - 1].real_fn == real_fn) i--;
  while (j + 1 < g_routes_n && g_routes[j + 1].real_fn == real_fn) j++;

  const driver_route_t *best = NULL;
  for (int k = i; k <= j; k++) {
    if (!symbol_in_family(g_routes[k].name, symbol)) continue;
    if (!strcmp(g_routes[k].name, symbol)) { best = &g_routes[k]; break; }
    if (!best || (!best->hook_fn && g_routes[k].hook_fn)) best = &g_routes[k];
  }
  if (!best) return NULL;                     /* address known, wrong family     */

  if (name) *name = best->name;
  return best->hook_fn;
}

void load_cuda_libraries() {
  void *table = NULL;
  int i = 0;
  char cuda_filename[FILENAME_MAX];

  init_real_dlsym();

  snprintf(cuda_filename, FILENAME_MAX - 1, "%s.%s", CUDA_LIBRARY_PREFIX, driver_version);
  cuda_filename[FILENAME_MAX - 1] = '\0';

  table = dlopen(cuda_filename, RTLD_NOW | RTLD_NODELETE);
  if (unlikely(!table)) {
    LOGGER(FATAL, "can't find library %s", cuda_filename);
  }
  int entry_count = 0;
  for (i = 0; i < CUDA_ENTRY_END; i++) {
    if (unlikely(cuda_library_entry[i].fn_ptr)) {
      entry_count++;
      continue;
    }
    LOGGER(DETAIL, "loading %s:%d", cuda_library_entry[i].name, i);
    cuda_library_entry[i].fn_ptr = real_dlsym(table, cuda_library_entry[i].name);
    if (unlikely(!cuda_library_entry[i].fn_ptr)) {
      cuda_library_entry[i].fn_ptr = real_dlsym(RTLD_NEXT,cuda_library_entry[i].name);
      if (unlikely(!cuda_library_entry[i].fn_ptr)) {
        LOGGER(VERBOSE, "can't find function %s in %s", cuda_library_entry[i].name, cuda_filename);
        continue;
      }
    }
    entry_count++;
  }

  LOGGER(INFO, "loaded cuda libraries: %d entries", entry_count);
  dlclose(table);
  build_driver_routes();
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

static void read_version_from_proc(void) {

  char *line = NULL;
  size_t len = 0;

  FILE *fp = fopen(DRIVER_VERSION_PATH, "re");  /* "e" = O_CLOEXEC, prevent fork inheritance */
  if (fp == NULL) {
    LOGGER(VERBOSE, "can't open %s, error %s", DRIVER_VERSION_PATH, strerror(errno));
    return;
  }

  while ((getline(&line, &len, fp) != -1)) {
    if (strncmp(line, "NVRM", 4) == 0) {
      matchRegex(DRIVER_VERSION_MATCH_PATTERN, line, driver_version);
      break;
    }
  }
  fclose(fp);
}

int strsplit(char *s, char **dest, const char *sep) {
  char *token;
  int index = 0;
  char *context = NULL;
  token = strtok_r(s, sep, &context);
  while (token != NULL && index < MAX_DEVICE_COUNT) {
    dest[index] = token;
    index += 1;
    token = strtok_r(NULL, sep, &context);
  }
  return index;
}

static int is_valid_device_index(int index, const char *kind) {
  if (likely(index >= 0 && index < MAX_DEVICE_COUNT)) {
    return 1;
  }
  LOGGER(ERROR, "invalid %s index %d", kind, index);
  return 0;
}

int mmap_file_to_config_path(resource_data_t** data) {
  int ret = 1;
  if (unlikely(file_exist(CONTROLLER_CONFIG_FILE_PATH) != 0)) {
    return ret;
  }
  int fd = open(CONTROLLER_CONFIG_FILE_PATH, O_RDONLY | O_CLOEXEC);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    return ret;
  }
  /* Read-lock byte 0 so we never validate a file a concurrent writer is
   * mid-write on; the header check below is a backstop if locking fails.
   * Released at DONE -- the mapping itself outlives the lock. */
  struct flock rl;
  memset(&rl, 0, sizeof(rl));
  rl.l_type = F_RDLCK;
  rl.l_whence = SEEK_SET;
  rl.l_start = 0;
  rl.l_len = 1;
  if (unlikely(ofd_fcntl(fd, 1, &rl) == -1)) {
    LOGGER(WARNING, "can't read-lock %s (%s); validating without it",
           CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
  }
  struct stat sb;
  if (fstat(fd, &sb) == -1) {
    LOGGER(ERROR, "fstat failed: %s", strerror(errno));
    goto DONE;
  }
  if (sb.st_size != CONFIG_FILE_SIZE) {
    LOGGER(ERROR, "vgpu config size mismatch: expected %d, got %lld",
                  CONFIG_FILE_SIZE, (long long)sb.st_size);
    goto DONE;
  }
  /* PROT_READ + MAP_PRIVATE: we never write, so no page is ever COW-copied
   * and every read sees the writer's live update. Tear-free consistency
   * comes from the per-device seqlock in get_device_snapshot(), not here. */
  resource_data_t *m = (resource_data_t*)mmap(NULL, CONFIG_FILE_SIZE, PROT_READ,
                                              MAP_PRIVATE, fd, 0);
  if (m == MAP_FAILED) {
    LOGGER(ERROR, "mmap global config failed: %s", strerror(errno));
    goto DONE;
  }
  /* Frozen-header check, same contract as vmem_node/sm_node: a config from a
   * mismatched layout_version is rejected cleanly instead of misread. */
  if (m->magic != CONFIG_MAGIC || m->layout_version != CONFIG_LAYOUT_VERSION ||
      m->region_size != sizeof(resource_data_t) || m->device_count != MAX_DEVICE_COUNT) {
    LOGGER(ERROR, "vgpu config header mismatch: magic=%#x ver=%u size=%u count=%u "
                  "(want %#x/%u/%zu/%d)",
                  m->magic, m->layout_version, m->region_size, m->device_count,
                  CONFIG_MAGIC, CONFIG_LAYOUT_VERSION, sizeof(resource_data_t),
                  MAX_DEVICE_COUNT);
    munmap(m, CONFIG_FILE_SIZE);
    goto DONE;
  }
  *data = m;
  ret = 0;
DONE:
  close(fd);
  return ret;
}

/* Config lock helpers (config_device_read_lock / config_device_unlock) live in
 * lock.c, mirroring the device_util_* pattern. */
extern int  config_device_read_lock(int device_index);
extern void config_device_unlock(int fd, int device_index);

#define CONFIG_SEQ_SPIN_LIMIT 1024

static inline void config_cpu_relax(void) {
#if defined(__x86_64__)
  __builtin_ia32_pause();
#elif defined(__i386__)
  __asm__ __volatile__("pause" ::: "memory");
#elif defined(__aarch64__) || defined(__arm__)
  __asm__ __volatile__("yield" ::: "memory");
#else
  __asm__ __volatile__("" ::: "memory");
#endif
}

/* Tear-free snapshot of devices[host_index] via the per-device seqlock.
 *
 * Fast path is syscall-free: two acquire loads around a plain struct copy,
 * retried if the seq is odd (writer mid-update) or changed between loads.
 * The writer's update window is nanoseconds, so this almost never spins.
 *
 * Slow path (writer crashed mid-update, or we got descheduled past the spin
 * cap): take the per-device F_RDLCK once. A crashed writer's lock is already
 * released by the kernel on fd close, so this can't hang. */
device_t get_device_snapshot(int host_index) {
  device_t snap;
  if (unlikely(host_index < 0 || host_index >= MAX_DEVICE_COUNT || g_vgpu_config == NULL)) {
    memset(&snap, 0, sizeof(snap));
    return snap;
  }
  const device_t *d = &g_vgpu_config->devices[host_index];
  unsigned spins = 0;
  for (;;) {
    uint32_t s1 = __atomic_load_n(&d->seq, __ATOMIC_ACQUIRE);
    if (likely(!(s1 & 1u))) {
      /* Plain struct copy, deliberately -- standard seqlock discipline. A
       * torn copy is caught and discarded by the s1==s2 check below, and the
       * ACQUIRE fence stops the compiler hoisting a field read past that
       * check. A whole-struct atomic load isn't an option (device_t is 128B,
       * past any lock-free width -- __atomic_load would silently fall back
       * to a libatomic lock table and break cross-process safety). */
      snap = *d;
      __atomic_thread_fence(__ATOMIC_ACQUIRE);
      uint32_t s2 = __atomic_load_n(&d->seq, __ATOMIC_ACQUIRE);
      if (likely(s1 == s2)) return snap;          /* stable copy */
    }
    config_cpu_relax();
    if (unlikely(++spins >= CONFIG_SEQ_SPIN_LIMIT)) {
      int fd = config_device_read_lock(host_index);
      snap = *d;
      if (fd >= 0) config_device_unlock(fd, host_index);
      LOGGER(WARNING, "get_device_snapshot(%d): seqlock spin cap hit, RDLCK fallback", host_index);
      return snap;
    }
  }
}

int mmap_file_to_util_path(device_util_t** data) {
  int ret = 1;
  if (unlikely(file_exist(CONTROLLER_SM_UTIL_FILE_PATH) != 0)) {
    return 0;
  }
  int fd = open(CONTROLLER_SM_UTIL_FILE_PATH, O_RDONLY | O_CLOEXEC);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", CONTROLLER_SM_UTIL_FILE_PATH, strerror(errno));
    return ret;
  }
  struct stat sb;
  if (fstat(fd, &sb) == -1) {
    LOGGER(ERROR, "fstat failed: %s", strerror(errno));
    goto DONE;
  }
  if (sb.st_size != sizeof(device_util_t)) {
    LOGGER(ERROR, "file size mismatch: expected %zu, got %lld", sizeof(device_util_t), (long long)sb.st_size);
    goto DONE;
  }
  *data = (device_util_t*)mmap(NULL, sb.st_size, PROT_READ, MAP_PRIVATE, fd, 0);
  if (*data == MAP_FAILED) {
    LOGGER(ERROR, "mmap sm watcher failed: %s", strerror(errno));
    *data = NULL;
    goto DONE;
  }
  ret = 0;
DONE:
  close(fd);
  return ret;
}

/* Does `dir` sit on its own mount, or is it just a directory inside `parent`?
 * A bind mount is its own mount point and reports a different st_dev than its
 * parent. Returns 1 mounted, 0 not, -1 unknown (stat failed -- draw no
 * conclusion). Must be called BEFORE any mkdir of `dir`, or it describes a
 * directory we created ourselves. */
static int dir_is_mount_point(const char *dir, const char *parent) {
  struct stat dir_sb, parent_sb;
  if (stat(dir, &dir_sb) != 0) return -1;
  if (stat(parent, &parent_sb) != 0) return -1;
  return dir_sb.st_dev != parent_sb.st_dev ? 1 : 0;
}

/* Identity of the vmem_node region we mapped, so a replacement can be
 * reported. A root container can `rm -rf` a writable mount and recreate the
 * file, giving late-starting processes a fresh inode while we keep the old
 * one -- from then on each group's usage sum only sees its own charges, and
 * the memory limit is under-enforced. Detection only, no remapping: this
 * process's existing charges live in the old region, and switching regions
 * mid-flight would silently drop or double-count them. */
static ino_t g_vmem_node_ino;
static dev_t g_vmem_node_dev;
static int   g_vmem_ident_warned;

void vmem_node_check_identity(void) {
  if (g_device_vmem == NULL || g_vmem_node_ino == 0) {
    return;
  }
  struct stat sb;
  if (stat(VMEMORY_NODE_FILE_PATH, &sb) != 0) {
    if (!g_vmem_ident_warned) {
      LOGGER(WARNING, "%s has been deleted; processes started from now on will keep a "
                      "SEPARATE virtual-memory ledger, so neither group sees the other's "
                      "charges and the memory limit is under-enforced. Restart the container.",
                      VMEMORY_NODE_FILE_PATH);
      g_vmem_ident_warned = 1;
    }
    return;
  }
  if (sb.st_ino != g_vmem_node_ino || sb.st_dev != g_vmem_node_dev) {
    LOGGER(WARNING, "%s was replaced (inode %llu -> %llu); this process still accounts "
                    "into the old ledger while newer processes use the new one, so the "
                    "memory limit is under-enforced. Restart the container.",
                    VMEMORY_NODE_FILE_PATH,
                    (unsigned long long)g_vmem_node_ino, (unsigned long long)sb.st_ino);
    g_vmem_node_ino = sb.st_ino;
    g_vmem_node_dev = sb.st_dev;
    g_vmem_ident_warned = 0;
    return;
  }
  g_vmem_ident_warned = 0;
}

static int vmem_node_header_valid(const device_vmemory_t *r) {
  return __atomic_load_n(&r->magic, __ATOMIC_ACQUIRE) == VMEM_NODE_MAGIC &&
         r->layout_version == VMEM_NODE_LAYOUT_VERSION            &&
         r->region_size    == (uint32_t)sizeof(device_vmemory_t)  &&
         r->device_count   == (uint32_t)MAX_DEVICE_COUNT;
}

/* Called with the header byte write-locked. Rebuilds in place instead of
 * refusing to start: a layout mismatch means the file belongs to a previous
 * incarnation of this container, so every record in it is already dead --
 * losing the ledger costs nothing. magic is published last with release
 * ordering, so an interrupted rebuild just leaves the region invalid for the
 * next process to rebuild again, and an unlocked reader mid-rebuild sees
 * magic == 0 and skips it safely. */
static void vmem_node_rebuild_locked(device_vmemory_t *r) {
  /* First build and repair share this path, but log differently: a fresh
   * all-zero region (normal first start) isn't a mismatch, only a nonzero
   * header is evidence of drift. */
  if (r->magic == 0 && r->layout_version == 0 &&
      r->region_size == 0 && r->device_count == 0) {
    LOGGER(INFO, "vmem_node region initialised (%d bytes, layout v%u)",
           VMEM_NODE_FILE_SIZE, VMEM_NODE_LAYOUT_VERSION);
  } else {
    LOGGER(WARNING, "vmem_node layout mismatch (magic=%#x ver=%u size=%u count=%u), rebuilding",
           r->magic, r->layout_version, r->region_size, r->device_count);
  }
  memset(r, 0, VMEM_NODE_FILE_SIZE);
  r->device_count   = (uint32_t)MAX_DEVICE_COUNT;
  r->region_size    = (uint32_t)sizeof(device_vmemory_t);
  r->layout_version = VMEM_NODE_LAYOUT_VERSION;
  __atomic_store_n(&r->magic, VMEM_NODE_MAGIC, __ATOMIC_RELEASE);
}

int mmap_file_to_vmem_node(device_vmemory_t** data) {
  *data = NULL;
  int ret = 1;

  /* Checked before mkdir -- afterwards a missing mount is undetectable,
   * since mkdir would just create the directory on the container's own fs. */
  if (dir_is_mount_point(VMEMORY_NODE_PATH, TMP_DIR) == 0) {
    LOGGER(WARNING, "%s is not a mount point -- the plugin did not provide it, so this "
                    "ledger lives in the container's own /tmp: it will NOT be cleaned "
                    "between container restarts and is not visible from the host",
                    VMEMORY_NODE_PATH);
  }

  if (unlikely(file_exist(VMEMORY_NODE_PATH) != 0)) {
    mkdir(VMEMORY_NODE_PATH, 0755);
  }

  /* Unconditional O_CREAT, no file_exist() pre-check -- checking first would
   * let two processes both think they created it and both memset the
   * mapping, each erasing what the other just wrote. */
  int fd = open(VMEMORY_NODE_FILE_PATH, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (unlikely(fd == -1)) {
    LOGGER(WARNING, "can't open %s: %s", VMEMORY_NODE_FILE_PATH, strerror(errno));
    return ret;
  }

  /* Lock only byte 0 of the header, not the whole file -- the per-device
   * locks live at higher offsets, so a whole-file lock would needlessly
   * contend with every per-device reader/writer. This only needs to exclude
   * another process initialising concurrently. */
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_start = 0;
  fl.l_len = 1;
  if (unlikely(ofd_fcntl(fd, 1, &fl) == -1)) {
    LOGGER(WARNING, "can't lock %s: %s", VMEMORY_NODE_FILE_PATH, strerror(errno));
    close(fd);
    return ret;
  }

  struct stat sb;
  if (unlikely(fstat(fd, &sb) == -1)) {
    LOGGER(WARNING, "fstat %s failed: %s", VMEMORY_NODE_FILE_PATH, strerror(errno));
    goto UNLOCK;
  }
  if (sb.st_size != VMEM_NODE_FILE_SIZE) {
    if (unlikely(ftruncate(fd, VMEM_NODE_FILE_SIZE) == -1)) {
      LOGGER(WARNING, "ftruncate %s failed: %s", VMEMORY_NODE_FILE_PATH, strerror(errno));
      goto UNLOCK;
    }
  }

  device_vmemory_t *region = (device_vmemory_t*)mmap(NULL, VMEM_NODE_FILE_SIZE,
                                                     PROT_READ | PROT_WRITE,
                                                     MAP_SHARED, fd, 0);
  if (unlikely(region == MAP_FAILED)) {
    LOGGER(ERROR, "mmap vmemory node failed: %s", strerror(errno));
    goto UNLOCK;
  }
  /* Fresh file (all zero) and stale file take the same path: magic does not
   * match, so rebuild. One code path, no `created` flag. */
  if (!vmem_node_header_valid(region)) {
    vmem_node_rebuild_locked(region);
  }
  /* Remember which inode we mapped so vmem_node_check_identity() can notice it
   * being replaced. sb is from the fstat above, on this same fd. */
  g_vmem_node_ino = sb.st_ino;
  g_vmem_node_dev = sb.st_dev;
  *data = region;
  ret = 0;

UNLOCK:
  fl.l_type = F_UNLCK;
  ofd_fcntl(fd, 1, &fl);
  /* Safe to close here: the mapping holds its own reference, and this runs
   * during init before this process has taken any per-device lock. */
  close(fd);
  return ret;
}

/* Reads only the FROZEN header (hook.h), whose offsets never change, so it is
 * safe to call before knowing which version wrote the file. */
static int sm_node_header_valid(const sm_node_region_t *r) {
  return __atomic_load_n(&r->magic, __ATOMIC_ACQUIRE) == SM_NODE_MAGIC &&
         r->layout_version == SM_NODE_LAYOUT_VERSION                   &&
         r->region_size    == (uint32_t)sizeof(sm_node_region_t)       &&
         r->device_count   == (uint32_t)MAX_DEVICE_COUNT;
}

/* Called with the file write-locked. Rebuilds in place: a layout mismatch
 * means the file is from a previous container incarnation, and a container
 * loads exactly one .so version for its whole life, so there's never an
 * old-version reader to protect. In-place also avoids a rename race that
 * could split the "shared" bucket into two private ones on different inodes.
 * magic is published last with release ordering, so a kill mid-rebuild just
 * leaves it absent for the next process to rebuild again. */
static void sm_node_rebuild_locked(sm_node_region_t *r) {
  /* See vmem_node_rebuild_locked: same path for first build and repair, but a
   * first build is not a mismatch and must not be reported as one. */
  if (r->magic == 0 && r->layout_version == 0 &&
      r->region_size == 0 && r->device_count == 0) {
    LOGGER(INFO, "sm_node region initialised (%d bytes, layout v%u)",
           SM_NODE_FILE_SIZE, SM_NODE_LAYOUT_VERSION);
  } else {
    LOGGER(WARNING, "sm_node layout mismatch (magic=%#x ver=%u size=%u count=%u), rebuilding",
           r->magic, r->layout_version, r->region_size, r->device_count);
  }

  memset(r, 0, SM_NODE_FILE_SIZE);

  for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
    /* Seed up_limit here, once, rather than in each watcher thread -- that
     * stops a late-joining process from resetting the container's already
     * converged limit every time it starts a watcher. total_cuda_cores is
     * left 0: it depends on CUDA device properties not yet queried, and is
     * published later purely for observability -- the authoritative ceiling
     * stays the per-process g_total_cuda_cores[], computed identically by
     * every process from the same device. */
    r->devices[i].up_limit = get_device_snapshot(i).hard_core;
  }

  r->device_count   = (uint32_t)MAX_DEVICE_COUNT;
  r->region_size    = (uint32_t)sizeof(sm_node_region_t);
  r->layout_version = SM_NODE_LAYOUT_VERSION;
  __atomic_store_n(&r->magic, SM_NODE_MAGIC, __ATOMIC_RELEASE);   /* publish */
}

int open_sm_node_lock(void) {
  if (unlikely(file_exist(SM_NODE_PATH) != 0)) {
    mkdir(SM_NODE_PATH, 0755);
  }
  /* Zero-length file: it carries no data, only byte-range locks (one byte per
   * device, so devices are independent). Never ftruncate'd -- fcntl ranges do
   * not require the bytes to exist. */
  int fd = open(SM_NODE_LOCK_PATH, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (unlikely(fd == -1)) {
    LOGGER(WARNING, "can't open %s: %s -- sampling will not be centralised",
           SM_NODE_LOCK_PATH, strerror(errno));
    return -1;
  }
  return fd;
}


/* Is SM_NODE_PATH the directory the plugin mounted in, or one we just created
 * inside the container's own /tmp? If the mount is missing, mkdir() below
 * still succeeds and everything looks healthy, but the region then lives in
 * the container's own /tmp -- never cleaned by the host's pre-start cleanup,
 * and invisible from the host. A bind mount reports a different st_dev than
 * its parent; same st_dev means no mount landed. Returns 1 mounted, 0 not,
 * -1 unknown (stat failed). */
static int sm_node_dir_is_mounted(void) {
  return dir_is_mount_point(SM_NODE_PATH, TMP_DIR);
}

int map_sm_node_region(sm_node_region_t **data) {
  *data = NULL;
  if (unlikely(g_vgpu_config == NULL)) return 1;

  /* Checked BEFORE mkdir, or the answer would describe our own directory. */
  int mounted = sm_node_dir_is_mounted();
  if (mounted == 0) {
    LOGGER(WARNING, "%s is not a mount point -- the plugin did not provide it, so this "
                    "region lives in the container's own /tmp: it will NOT be cleaned "
                    "between container restarts and is not visible from the host",
                    SM_NODE_PATH);
  } else if (mounted < 0) {
    LOGGER(VERBOSE, "%s mount state unknown (stat failed: %s)",
                    SM_NODE_PATH, strerror(errno));
  }

  if (unlikely(file_exist(SM_NODE_PATH) != 0)) {
    mkdir(SM_NODE_PATH, 0755);
  }

  /* open(O_CREAT) unconditionally, no file_exist() pre-check -- that check is
   * what makes a racy variant possible (two processes both conclude they
   * created the file and both memset it). Wrong size gets ftruncate'd, wrong
   * magic gets rebuilt, both idempotent and under the lock. */
  int fd = open(SM_NODE_FILE_PATH, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (unlikely(fd == -1)) {
    LOGGER(WARNING, "can't open %s: %s", SM_NODE_FILE_PATH, strerror(errno));
    return 1;
  }

  /* Blocking lock, no timeout, deliberately: a late arriver just sleeps in
   * the kernel until the region is built, burning no CPU. Safe here (unlike
   * lock_gpu_device) because the critical section is a bounded memset with
   * no CUDA call and nothing that can block indefinitely -- and if the
   * holder dies, the kernel releases the lock unconditionally. */
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_start = 0;
  fl.l_len = 0;                       /* whole file */
  if (unlikely(ofd_fcntl(fd, 1, &fl) == -1)) {
    LOGGER(WARNING, "can't lock %s: %s", SM_NODE_FILE_PATH, strerror(errno));
    close(fd);
    return 1;
  }

  int ret = 0;
  struct stat sb;
  if (unlikely(fstat(fd, &sb) == -1)) {
    LOGGER(WARNING, "fstat %s failed: %s", SM_NODE_FILE_PATH, strerror(errno));
    ret = 1;
    goto UNLOCK;
  }
  /* Size is a permanent constant, so this runs for a fresh file (size 0) and
   * for anything unexpected. Holes read as zero, so magic will not match and
   * the rebuild below fires. */
  if (sb.st_size != SM_NODE_FILE_SIZE) {
    if (unlikely(ftruncate(fd, SM_NODE_FILE_SIZE) == -1)) {
      LOGGER(WARNING, "ftruncate %s failed: %s", SM_NODE_FILE_PATH, strerror(errno));
      ret = 1;
      goto UNLOCK;
    }
  }

  sm_node_region_t *region = (sm_node_region_t *)mmap(NULL, SM_NODE_FILE_SIZE,
                                                      PROT_READ | PROT_WRITE,
                                                      MAP_SHARED, fd, 0);
  if (unlikely(region == MAP_FAILED)) {
    LOGGER(WARNING, "mmap %s failed: %s", SM_NODE_FILE_PATH, strerror(errno));
    ret = 1;
    goto UNLOCK;
  }

  /* Fresh (all-zero) and stale regions take the same path: magic doesn't
   * match, so we rebuild -- no "created" flag, no second branch to get wrong. */
  if (!sm_node_header_valid(region)) {
    sm_node_rebuild_locked(region);
  }
  *data = region;

UNLOCK:
  fl.l_type = F_UNLCK;
  ofd_fcntl(fd, 1, &fl);
  /* Closing the fd doesn't disturb the mapping -- it holds its own
   * reference. The lock's lifetime ends here, entirely within init. */
  close(fd);
  return ret;
}

void print_global_vgpu_config() {
  LOGGER(VERBOSE, "------------------print_global_vgpu_config------------------");
  if (g_vgpu_config->pod_name[0] != '\0') {
    LOGGER(VERBOSE, "Pod Name         : %s", g_vgpu_config->pod_name);
  }
  if (g_vgpu_config->pod_namespace[0] != '\0') {
    LOGGER(VERBOSE, "Pod Namespace    : %s", g_vgpu_config->pod_namespace);
  }
  if (g_vgpu_config->pod_uid[0] != '\0') {
    LOGGER(VERBOSE, "Pod Uid          : %s", g_vgpu_config->pod_uid);
  }
  if (g_vgpu_config->container_name[0] != '\0') {
    LOGGER(VERBOSE, "Container Name   : %s", g_vgpu_config->container_name);
  }
  if (g_vgpu_config->reg_uuid[0] != '\0') {
    LOGGER(VERBOSE, "Register Uuid    : %s", g_vgpu_config->reg_uuid);
  }
  LOGGER(VERBOSE, "CompatibilityMode: %d", g_vgpu_config->compatibility_mode);
  LOGGER(VERBOSE, "Ext SM Watcher   : %s", g_vgpu_config->sm_watcher ? "enabled" : "disabled");
  LOGGER(VERBOSE, "VMemory Node     : %s", g_vgpu_config->vmem_node ? "enabled" : "disabled");
  int index = 0;
  for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
    device_t d = get_device_snapshot(i);
    if (d.activate) {
      LOGGER(VERBOSE, "---------------------------GPU %d---------------------------", index);
      LOGGER(VERBOSE, "GPU UUID         : %s", d.uuid);
      LOGGER(VERBOSE, "Memory Limit     : %s", d.memory_limit ? "enabled" : "disabled");
      LOGGER(VERBOSE, "+ RealMemorySize : %ld", d.real_memory);
      LOGGER(VERBOSE, "+ TotalMemorySize: %ld", d.total_memory);
      LOGGER(VERBOSE, "Cores  Limit     : %s", d.core_limit ? "enabled" : "disabled");
      LOGGER(VERBOSE, "+ HardLimit      : %s", d.hard_limit ? "enabled" : "disabled");
      LOGGER(VERBOSE, "+ HardCoreSize   : %d", d.hard_core);
      LOGGER(VERBOSE, "+ SoftCoreSize   : %d", d.soft_core);
      LOGGER(VERBOSE, "Memory Oversold  : %s", d.memory_oversold ? "enabled" : "disabled");
      index++;
    }
  }
  LOGGER(VERBOSE, "-----------------------------------------------------------");
}

int write_file_to_config_path(resource_data_t* data) {
  int ret = 1;
  if (unlikely(file_exist(VGPU_MANAGER_PATH) != 0)) {
    mkdir(VGPU_MANAGER_PATH, 0755);
  }
  if (unlikely(file_exist(VGPU_CONFIG_PATH) != 0)) {
    mkdir(VGPU_CONFIG_PATH, 0755);
  }
  /* Deliberately not O_TRUNC: truncation must happen after the write lock is
   * held, not at open() time, or a concurrent peer/reader races the empty
   * window. Same discipline as mmap_file_to_vmem_node. */
  int fd = open(CONTROLLER_CONFIG_FILE_PATH, O_CREAT | O_RDWR | O_CLOEXEC, 0644);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    return ret;
  }
  /* Serialise concurrent creators on byte 0 of the header (the same byte
   * readers F_RDLCK). A dead writer's lock releases automatically on close. */
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_start = 0;
  fl.l_len = 1;
  if (unlikely(ofd_fcntl(fd, 1, &fl) == -1)) {
    LOGGER(ERROR, "can't lock %s: %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    goto DONE;
  }
  /* If a peer already wrote a full-size, valid file under this lock, skip
   * the rewrite -- every process builds identical data from the same env, so
   * a peer's file is ours. Verify the header, not just the size: a
   * stale/corrupt file of the right length must still be rewritten. */
  struct stat sb;
  uint32_t hdr[4];
  if (fstat(fd, &sb) == 0 && sb.st_size == CONFIG_FILE_SIZE &&
      pread(fd, hdr, sizeof(hdr), 0) == (ssize_t)sizeof(hdr) &&
      hdr[0] == CONFIG_MAGIC && hdr[1] == CONFIG_LAYOUT_VERSION &&
      hdr[2] == (uint32_t)sizeof(resource_data_t) && hdr[3] == (uint32_t)MAX_DEVICE_COUNT) {
    ret = 0;
    goto DONE;
  }
  /* Stamp the frozen header so the validator in mmap_file_to_config_path
   * accepts whatever build path reached this writer. */
  data->magic          = CONFIG_MAGIC;
  data->layout_version = CONFIG_LAYOUT_VERSION;
  data->region_size    = sizeof(resource_data_t);
  data->device_count   = MAX_DEVICE_COUNT;
  /* Clear, write at offset 0, then size to the reserved total. Starting from 0
   * zeroes the reserved tail; the lock keeps any reader from seeing the middle;
   * the fixed final size means a later larger struct never resizes the file
   * (which would SIGBUS an old map). */
  if (unlikely(ftruncate(fd, 0) == -1) ||
      pwrite(fd, (void*)data, sizeof(resource_data_t), 0) != (ssize_t)sizeof(resource_data_t) ||
      ftruncate(fd, CONFIG_FILE_SIZE) == -1) {
    LOGGER(ERROR, "can't write %s to %d bytes: %s",
                  CONTROLLER_CONFIG_FILE_PATH, CONFIG_FILE_SIZE, strerror(errno));
    goto DONE;
  }
  ret = 0;
DONE:
  close(fd);   /* closing the fd releases its OFD lock */
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

int check_tid_dlsyms(pthread_t tid, void *pointer){
  int i;
  int cursor = (tid_dlsym_count < DLMAP_SIZE) ? tid_dlsym_count : DLMAP_SIZE;
  for (i = cursor - 1; i >= 0; i--) {
    if ((tid_dlsyms[i].pointer == pointer) && pthread_equal(tid_dlsyms[i].tid, tid)) {
      return 1;
    }
  }
  cursor = tid_dlsym_count % DLMAP_SIZE;
  tid_dlsyms[cursor].tid = tid;
  tid_dlsyms[cursor].pointer = pointer;
  tid_dlsym_count++;
  return 0;
}

extern entry_t nvml_hooks_entry[];
extern const int nvml_hook_nums;

/* Resolve our hook for `symbol` from a hijack table.
 *
 * Every hook is exported under its own name (the version script matches
 * cu[A-Z]* / nvml[A-Z]*), so when lib_control -- a dlopen handle on our own
 * .so -- is available, the dynamic linker's hash finds any hook in O(1). A miss
 * there is conclusive: the symbol is not one of our hooks, and the linear table
 * scan would only reconfirm that. The scan is therefore kept solely for the
 * degraded case where self-dlopen failed (e.g. tests LD_PRELOAD a build-tree
 * .so that is not at CONTROLLER_DRIVER_FILE_PATH), where it is the only route. */
static void *resolve_local_hook(const char *symbol, entry_t *entries, int n) {
  if (likely(lib_control)) {
    return real_dlsym(lib_control, symbol);
  }
  for (int i = 0; i < n; i++) {
    if (unlikely(!strcmp(symbol, entries[i].name))) {
      return entries[i].fn_ptr;
    }
  }
  return NULL;
}

/* Record, once, that a cu... / nvml... symbol went through us uninstrumented.
 *
 * Reaching here already means the symbol is not one of our hooks, so nothing
 * further needs deciding -- the point is simply to leave a trail. A driver that
 * grows a variant we do not intercept (cuFoo_v3 and the like) then shows up in a
 * DETAIL run instead of being invisible.
 *
 * DETAIL because on any real workload this names hundreds of symbols we never
 * intended to hook; it is a diagnostic, not a warning. The level check comes
 * first so a normal run pays one comparison and nothing else.
 *
 * Dedup is a lock-free open-addressed set of name hashes. A mutex would be
 * wrong here twice over: it would serialise a path that is otherwise pure
 * lookup, and it would add another handle-at-fork hazard to a hook the child
 * calls immediately. Losing a race, or exhausting the probe window, costs at
 * most a duplicate line -- the right trade for a log. */
#define UNHOOKED_SET_BITS 11u                       /* 2048 slots, 8 KiB */
#define UNHOOKED_SET_SIZE (1u << UNHOOKED_SET_BITS)
#define UNHOOKED_PROBES   8u
static volatile uint32_t g_unhooked_seen[UNHOOKED_SET_SIZE];

static uint32_t symbol_hash(const char *s) {
  uint32_t h = 2166136261u;                         /* FNV-1a */
  while (*s) {
    h ^= (unsigned char)*s++;
    h *= 16777619u;
  }
  return h ? h : 1u;                                /* 0 means "empty slot" */
}

void note_unhooked_symbol(const char *symbol) {
  if (!LOGGER_SHOULD_PRINT(VERBOSE)) return;
  uint32_t h = symbol_hash(symbol);
  uint32_t i = h & (UNHOOKED_SET_SIZE - 1);
  for (uint32_t p = 0; p < UNHOOKED_PROBES; p++, i = (i + 1) & (UNHOOKED_SET_SIZE - 1)) {
    uint32_t cur = g_unhooked_seen[i];
    if (cur == h) return;                           /* already logged */
    if (cur == 0) {
      if (CAS(&g_unhooked_seen[i], 0u, h)) {
        LOGGER(VERBOSE, "unhooked driver symbol '%s'", symbol);
        return;
      }
      /* Lost the race for this slot. Re-read before probing on: the winner may
       * have been another thread recording the SAME symbol, and treating that
       * as a collision would claim a second slot and log a duplicate. */
      if (g_unhooked_seen[i] == h) return;
    }
  }
  /* Probe window exhausted: stay quiet rather than repeat on every lookup. */
}

FUNC_ATTR_VISIBLE void* dlsym(void* handle, const char* symbol) {
  static __thread int recursion_depth = 0;
  if (recursion_depth > 0) {
    LOGGER(WARNING, "recursion protection triggered for %s", symbol);
    return real_dlsym ? real_dlsym(handle, symbol) : NULL;
  }
  recursion_depth++;

  LOGGER(DETAIL, "into dlsym %s", symbol);
  init_real_dlsym();

  void* result = NULL;
  if (handle == RTLD_NEXT) {
    pthread_once(&init_dlsym_flag, init_tid_dlsyms);
    result = real_dlsym(RTLD_NEXT, symbol);
    pthread_mutex_lock(&tid_dlsym_lock);
    pthread_t tid = pthread_self();
    if (check_tid_dlsyms(tid, result)) {
      LOGGER(WARNING, "recursive dlsym: %s",symbol);
      result = NULL;
    }
    pthread_mutex_unlock(&tid_dlsym_lock);
    goto DONE;
  } else if (strncmp(symbol, "cu", 2) == 0) { // hijack cuda
    result = resolve_local_hook(symbol, cuda_hooks_entry, cuda_hook_nums);
    if (likely(result)) {
      LOGGER(DETAIL, "search found cuda hook %s", symbol);
      load_necessary_data();
      goto DONE;
    }
    note_unhooked_symbol(symbol);
  } else if (strncmp(symbol, "nvml", 4) == 0) { // hijack nvml
    result = resolve_local_hook(symbol, nvml_hooks_entry, nvml_hook_nums);
    if (likely(result)) {
      LOGGER(DETAIL, "search found nvml hook %s", symbol);
      goto DONE;
    }
    note_unhooked_symbol(symbol);
  }
  result = real_dlsym(handle, symbol);
DONE:
  recursion_depth--;
  return result;
}

void rm_vmem_node_by_non_existent_device_pid(int device_id, int pid) {
  unsigned int processes_size = g_device_vmem->devices[device_id].processes_size;
  for (int i = processes_size - 1; i >= 0; i--) {
    int curr_pid = g_device_vmem->devices[device_id].processes[i].pid;
    int kick_out = 0;
    if (curr_pid == pid) {
      kick_out = 1;
    } else if (pid_exist(curr_pid) != 0) {
      LOGGER(WARNING, "detected that process %d does not exist, kicked out virtual memory node", curr_pid);
      kick_out = 1;
    } else if (is_zombie_proc(curr_pid) != 0) {
      LOGGER(WARNING, "detected that process %d is a zombie, kicked out virtual memory node", curr_pid);
      kick_out = 1;
    }
    if (kick_out) {
      g_device_vmem->devices[device_id].processes[i] = g_device_vmem->devices[device_id].processes[processes_size-1];
      g_device_vmem->devices[device_id].processes[processes_size-1].pid = 0;
      g_device_vmem->devices[device_id].processes[processes_size-1].used = 0;
      g_device_vmem->devices[device_id].processes_size--;
      processes_size--;
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

// Clean up the virtual memory records of PID
void cleanup_vmem_nodes(int pid) {
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

// Cleaning operation when the processing program exits
void exit_cleanup_handler() {
 static int cleanup_done = 0;
 // Prevent re-entry (exit_handler might be called multiple times)
 if (__sync_lock_test_and_set(&cleanup_done, 1)) {
   return;
 }
 int pid = getpid();
 LOGGER(INFO, "process program %d exits", pid);
 cleanup_vmem_nodes(pid);
}

/* Saved host sigaction state so we can chain to JVM shutdown hooks, the
 * JVM hs_err_pid writer, Go signal package, Python KeyboardInterrupt, etc.
 * Without chaining, LD_PRELOAD'ing into those runtimes clobbers their
 * handlers and breaks graceful shutdown / crash diagnostics. */
static struct sigaction g_old_sigterm_sa;
static struct sigaction g_old_sigint_sa;
static struct sigaction g_old_sighup_sa;
static struct sigaction g_old_sigabrt_sa;

static struct sigaction* get_old_sa_slot(int signum) {
  switch (signum) {
    case SIGTERM: return &g_old_sigterm_sa;
    case SIGINT:  return &g_old_sigint_sa;
    case SIGHUP:  return &g_old_sighup_sa;
    case SIGABRT: return &g_old_sigabrt_sa;
    default:      return NULL;
  }
}

/* SIGHUP intentionally skips exit_cleanup_handler(): host SIGHUP semantics
 * is typically "reload config and keep running"; cleaning up vmem nodes
 * here would orphan tracking while the process continues allocating. */
static void signal_cleanup_handler_sa(int signum, siginfo_t *info, void *ucontext) {
  LOGGER(INFO, "caught signal %d, cleaning up", signum);
  if (signum != SIGHUP) {
    exit_cleanup_handler();
  }

  struct sigaction *old = get_old_sa_slot(signum);
  if (old != NULL) {
    if ((old->sa_flags & SA_SIGINFO) && old->sa_sigaction != NULL) {
      old->sa_sigaction(signum, info, ucontext);
      return;
    }
    if (old->sa_handler != SIG_DFL && old->sa_handler != SIG_IGN) {
      old->sa_handler(signum);
      return;
    }
  }
  /* No host handler -- restore default and re-raise to preserve original
   * exit semantics (128+signum, or core dump for SIGABRT). */
  signal(signum, SIG_DFL);
  raise(signum);
}

// Cleaning up invalid virtual memory nodes on the device.
void check_cleanup_vmem_nodes_by_device(int host_index) {
  if (host_index < 0 || host_index >= MAX_DEVICE_COUNT) return;
  if (g_device_vmem != NULL) {
    if (g_device_vmem->devices[host_index].processes_size == 0) {
      return;
    }
    int fd = device_vmem_write_lock(host_index);
    if (fd < 0) return;
    rm_vmem_node_by_non_existent_device_pid(host_index, -1);
//    __sync_synchronize();
    device_vmem_unlock(fd, host_index);
  }
}

// check and clean up any unreleased virtual memory records.
void check_cleanup_vmem_nodes() {
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

static pthread_mutex_t device_index_mutex = PTHREAD_MUTEX_INITIALIZER;
// [cuda index] -> nvml index
static volatile int cuda_to_nvml_device_index[MAX_DEVICE_COUNT] = {-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1};
// [cuda index] -> host index
static volatile int cuda_to_host_device_index[MAX_DEVICE_COUNT] = {-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1};
// [nvml index] -> host index
static volatile int nvml_to_host_device_index[MAX_DEVICE_COUNT] = {-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1,-1};

void get_host_device_index_by_uuid(char *uuid, int *host_index) {
  for (int index = 0; index < MAX_DEVICE_COUNT; index++) {
    device_t d = get_device_snapshot(index);
    if (d.activate && strcmp(d.uuid, uuid) == 0) {
      *host_index = index;
      break;
    }
  }
}

int get_host_device_index_by_nvml_device(nvmlDevice_t device) {
  int nvml_index;
  nvmlReturn_t ret = NVML_INTERNAL_CHECK(nvml_library_entry, nvmlDeviceGetIndex, device, &nvml_index);
  if (unlikely(ret)) {
    return -1;
  }
  if (unlikely(!is_valid_device_index(nvml_index, "nvml"))) {
    return -1;
  }
  int host_index = nvml_to_host_device_index[nvml_index];
  if (likely(host_index >= 0)) {
    return host_index;
  }
  pthread_mutex_lock(&device_index_mutex);
  host_index = nvml_to_host_device_index[nvml_index];
  if (host_index < 0) {
    char uuid[UUID_BUFFER_SIZE];
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetUUID, device, uuid, UUID_BUFFER_SIZE);
    if (unlikely(ret)) {
      LOGGER(VERBOSE, "nvmlDeviceGetUUID call failed, nvml device %d, return %d, str: %s",
                       nvml_index, ret, NVML_ERROR(nvml_library_entry, ret));
      goto DONE;
    }
    get_host_device_index_by_uuid(uuid, &host_index);
    if (host_index >= 0) {
      nvml_to_host_device_index[nvml_index] = host_index;
      LOGGER(VERBOSE, "nvml device %d => host device %d", nvml_index, host_index);
    }
  }
DONE:
  pthread_mutex_unlock(&device_index_mutex);
  return host_index;
}

void formatUuid(CUuuid uuid, char* uuid_str, int len) {
    if (unlikely(len < 41)) {
      if (len > 0) uuid_str[0] = '\0';
      return;
    }
    uint8_t *b = (uint8_t *)uuid.bytes;
    snprintf(uuid_str, len, "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
                            b[0x0], b[0x1], b[0x2], b[0x3], b[0x4], b[0x5], b[0x6], b[0x7],
                            b[0x8], b[0x9], b[0xA], b[0xB], b[0xC], b[0xD], b[0xE], b[0xF]);
}

static int _get_host_device_index_by_cuda_device(CUdevice device) {
  int cuda_index = (int) device;
  int host_index = cuda_to_host_device_index[cuda_index];
  if (host_index < 0) {
    CUuuid cu_uuid;
    CUresult ret = CUDA_ERROR_NOT_FOUND;
    if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceGetUuid_v2))) {
      ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuDeviceGetUuid_v2, &cu_uuid, device);
    } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceGetUuid))){
      ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuDeviceGetUuid, &cu_uuid, device);
    }
    if (unlikely(ret)) {
      LOGGER(VERBOSE, "cuDeviceGetUuid can't get uuid on cuda device %d, return %d, str: %s",
                       cuda_index, ret, CUDA_ERROR(cuda_library_entry, ret));
      return -1;
    }
    char uuid[UUID_BUFFER_SIZE];
    formatUuid(cu_uuid, uuid, UUID_BUFFER_SIZE);
    get_host_device_index_by_uuid(uuid, &host_index);
    if (host_index >= 0) {
      cuda_to_host_device_index[cuda_index] = host_index;
      LOGGER(VERBOSE, "cuda device %d => host device %d", cuda_index, host_index);
    }
  }
  return host_index;
}

int get_host_device_index_by_cuda_device(CUdevice device) {
  int cuda_index = (int) device;
  if (unlikely(!is_valid_device_index(cuda_index, "cuda"))) {
    return -1;
  }
  int host_index = cuda_to_host_device_index[cuda_index];
  if (host_index >= 0) {
    return host_index;
  }
  pthread_mutex_lock(&device_index_mutex);
  host_index = _get_host_device_index_by_cuda_device(device);
  pthread_mutex_unlock(&device_index_mutex);
  return host_index;
}

int get_nvml_device_index_by_cuda_device(CUdevice device) {
  int cuda_index = (int) device;
  if (unlikely(!is_valid_device_index(cuda_index, "cuda"))) {
    return -1;
  }
  int nvml_index = cuda_to_nvml_device_index[cuda_index];
  if (nvml_index >= 0) {
    return nvml_index;
  }
  pthread_mutex_lock(&device_index_mutex);
  nvml_index = cuda_to_nvml_device_index[cuda_index];
  if (nvml_index < 0) {
    int host_index = _get_host_device_index_by_cuda_device(device);
    if (host_index < 0) {
      goto DONE;
    }
    for (int index = 0; index < MAX_DEVICE_COUNT; index++) {
      if (host_index == nvml_to_host_device_index[index]) {
        nvml_index = index;
        break;
      }
    }
    if (nvml_index >= 0) {
      cuda_to_nvml_device_index[cuda_index] = nvml_index;
    }
  }
DONE:
  pthread_mutex_unlock(&device_index_mutex);
  return nvml_index;
}

// Reset CUDA device index only when PID changes.
void reset_cuda_index_mapping() {
  pthread_mutex_lock(&device_index_mutex);
  for (int index = 0; index < MAX_DEVICE_COUNT; index++) {
    cuda_to_host_device_index[index] = -1;
    cuda_to_nvml_device_index[index] = -1;
  }
  pthread_mutex_unlock(&device_index_mutex);
}

static void malloc_gpu_virt_memory_graph(CUdeviceptr dptr, size_t bytes, int type,
                                         CUgraph graph, int host_index) {
  int found = 0;
  memory_node_t *entry_tmp = NULL;
  struct list_head *iter;
  size_t old_bytes = 0;

  int charged = (host_index >= 0 && host_index < MAX_DEVICE_COUNT) ? host_index : -1;

  pthread_mutex_lock(&g_memory_node_lock);
  list_for_each(iter, &g_memory_node->node) {
    entry_tmp = container_of(iter, memory_node_t, node);
    if (entry_tmp != NULL && entry_tmp->dptr == dptr) {
      old_bytes = entry_tmp->bytes;
      entry_tmp->bytes = bytes;
      entry_tmp->type = type;
      entry_tmp->graph = graph;
      entry_tmp->host_index = charged;
      found = 1;
      break;
    }
  }
  if (!found) {
    memory_node_t *new_node = (memory_node_t *)malloc(sizeof(memory_node_t));
    if (unlikely(!new_node)) {
      pthread_mutex_unlock(&g_memory_node_lock);
      LOGGER(ERROR, "failed to allocate virt memory node");
      return;
    }
    new_node->dptr = dptr;
    new_node->bytes = bytes;
    new_node->type = type;
    new_node->graph = graph;
    new_node->host_index = charged;
    INIT_LIST_HEAD(&new_node->node);
    list_add(&new_node->node, &g_memory_node->node);
  }
  pthread_mutex_unlock(&g_memory_node_lock);

  if (host_index < 0 || host_index >= MAX_DEVICE_COUNT) return;
  LOGGER(VERBOSE, "malloc virt memory to host device %d, dptr %lld, size %ld", host_index, dptr, bytes);

  /* Re-recording a known dptr with a smaller size yields a negative delta;
   * apply it with the same saturation free_gpu_virt_memory() uses, since
   * `used` is size_t and letting it wrap would corrupt every later check. */
  ssize_t delta = found ? (ssize_t)bytes - (ssize_t)old_bytes : (ssize_t)bytes;

  if (g_device_vmem != NULL) {
    int fd = device_vmem_write_lock(host_index);
    if (fd < 0) return;
    int pid = getpid();
    found = 0;
    unsigned int processes_size = g_device_vmem->devices[host_index].processes_size;
    for (int i = 0; i < processes_size; i++) {
      if (g_device_vmem->devices[host_index].processes[i].pid == pid) {
        size_t cur = g_device_vmem->devices[host_index].processes[i].used;
        if (delta >= 0) {
          g_device_vmem->devices[host_index].processes[i].used = cur + (size_t)delta;
        } else {
          size_t dec = (size_t)(-delta);
          g_device_vmem->devices[host_index].processes[i].used =
              (cur >= dec) ? (cur - dec) : 0;
        }
        found = 1;
        break;
      }
    }
    if (!found) {
      if (unlikely(processes_size >= MAX_PIDS)) {
        LOGGER(ERROR, "host device %d virtual memory process list is full", host_index);
        device_vmem_unlock(fd, host_index);
        return;
      }
      g_device_vmem->devices[host_index].processes[processes_size].pid = pid;
      g_device_vmem->devices[host_index].processes[processes_size].used = (delta > 0) ? delta : 0;
      g_device_vmem->devices[host_index].processes_size++;
    }
    device_vmem_unlock(fd, host_index);
  }
}

void malloc_gpu_virt_memory(CUdeviceptr dptr, size_t bytes, int type, int host_index) {
  malloc_gpu_virt_memory_graph(dptr, bytes, type, NULL, host_index);
}

void malloc_gpu_virt_memory_captured(CUdeviceptr dptr, size_t bytes,
                                     CUgraph graph, int host_index) {
  malloc_gpu_virt_memory_graph(dptr, bytes, MEMORY_TYPE_CAPTURE, graph, host_index);
}

/* Discharge every capture record owned by graph, against whichever device
 * each record was originally charged to -- not a device looked up here,
 * since the context may already be unusable by the time this runs. Nodes
 * are detached under the list mutex first, then the shared counter is
 * updated separately (it takes the cross-process vmem lock and must not
 * nest inside the list mutex; free_gpu_virt_memory() uses the same order). */
void free_gpu_virt_memory_by_graph(CUgraph graph) {
  size_t totals[MAX_DEVICE_COUNT] = {0};
  memory_node_t *entry_tmp = NULL;
  struct list_head *iter, *tmp;
  int any = 0;

  if (graph == NULL) return;

  pthread_mutex_lock(&g_memory_node_lock);
  list_for_each_safe(iter, tmp, &g_memory_node->node) {
    entry_tmp = container_of(iter, memory_node_t, node);
    if (entry_tmp == NULL) continue;
    if (entry_tmp->type == MEMORY_TYPE_CAPTURE && entry_tmp->graph == graph) {
      int idx = entry_tmp->host_index;
      if (idx >= 0 && idx < MAX_DEVICE_COUNT) {
        totals[idx] += entry_tmp->bytes;
        any = 1;
      }
      list_del(&entry_tmp->node);
      free(entry_tmp);
    }
  }
  pthread_mutex_unlock(&g_memory_node_lock);

  if (!any || g_device_vmem == NULL) return;

  for (int dev = 0; dev < MAX_DEVICE_COUNT; dev++) {
    if (totals[dev] == 0) continue;
    LOGGER(VERBOSE, "free captured virt memory to host device %d, graph %p, size %zu",
           dev, (void *)graph, totals[dev]);
    int fd = device_vmem_write_lock(dev);
    if (fd < 0) continue;
    int pid = getpid();
    for (int i = 0; i < g_device_vmem->devices[dev].processes_size; i++) {
      if (g_device_vmem->devices[dev].processes[i].pid == pid) {
        size_t cur = g_device_vmem->devices[dev].processes[i].used;
        g_device_vmem->devices[dev].processes[i].used =
            (cur >= totals[dev]) ? (cur - totals[dev]) : 0;
        break;
      }
    }
    device_vmem_unlock(fd, dev);
  }
}

int get_gpu_virt_memory_type(CUdeviceptr dptr) {
  int type = 0;
  memory_node_t *entry_tmp = NULL;
  struct list_head *iter;
  pthread_mutex_lock(&g_memory_node_lock);
  list_for_each(iter, &g_memory_node->node) {
    entry_tmp = container_of(iter, memory_node_t, node);
    if (entry_tmp == NULL) continue;
    if (entry_tmp->dptr == dptr) {
      type = entry_tmp->type;
      break;
    }
  }
  pthread_mutex_unlock(&g_memory_node_lock);
  return type;
}

/* Retire the record for dptr, discharging whatever device it was charged to.
 * The device comes from the record, never re-derived from the current
 * context -- a context switch between alloc and free would otherwise credit
 * the wrong device. */
void free_gpu_virt_memory(CUdeviceptr dptr) {
  int found = 0;
  int host_index = -1;
  memory_node_t *entry_tmp = NULL;
  struct list_head *iter;
  size_t size = 0;
  pthread_mutex_lock(&g_memory_node_lock);
  list_for_each(iter, &g_memory_node->node) {
    entry_tmp = container_of(iter, memory_node_t, node);
    if (entry_tmp == NULL) continue;
    if (entry_tmp->dptr == dptr) {
      found = 1;
      size = entry_tmp->bytes;
      host_index = entry_tmp->host_index;
      list_del(&entry_tmp->node);
      free(entry_tmp);
      break;
    }
  }
  pthread_mutex_unlock(&g_memory_node_lock);
  if (!found) return;

  if (host_index < 0 || host_index >= MAX_DEVICE_COUNT) return;
  LOGGER(VERBOSE, "free virt memory to host device %d, dptr %lld, size %ld", host_index, dptr, size);

  if (g_device_vmem != NULL) {
    int fd = device_vmem_write_lock(host_index);
    if (fd < 0) return;
    int pid = getpid();
    for (int i = 0; i< g_device_vmem->devices[host_index].processes_size; i++) {
      if (g_device_vmem->devices[host_index].processes[i].pid == pid) {
        g_device_vmem->devices[host_index].processes[i].used =
           (g_device_vmem->devices[host_index].processes[i].used >= size) ?
           (g_device_vmem->devices[host_index].processes[i].used - size) : 0;
        break;
      }
    }
    device_vmem_unlock(fd, host_index);
  }
}

void get_used_gpu_virt_memory(void *arg, int host_index) {
  size_t count = 0;
  size_t *used_memory = arg;
  if (host_index < 0 || host_index >= MAX_DEVICE_COUNT) goto DONE;
  if (g_vgpu_config->vmem_node && g_device_vmem != NULL) {
    int fd = device_vmem_read_lock(host_index);
    if (fd < 0) goto DONE;
    for (int i = 0; i < g_device_vmem->devices[host_index].processes_size; i++) {
      count += g_device_vmem->devices[host_index].processes[i].used;
    }
    device_vmem_unlock(fd, host_index);
  }
DONE:
  *used_memory = count;
}

void init_g_vgpu_config_by_env(resource_data_t** data) {
  int cudaVersion;
  snprintf(vgpu_config_init.driver_version, sizeof(vgpu_config_init.driver_version), "%s", driver_version);
  CUresult r = CUDA_ENTRY_CHECK_STRICT(cuda_library_entry, cuDriverGetVersion, &cudaVersion);
  if (likely(r == CUDA_SUCCESS)) {
    vgpu_config_init.cuda_version.major = cudaVersion / 1000;
    vgpu_config_init.cuda_version.minor = (cudaVersion % 1000) / 10;
  }
  int ret = get_compatibility_mode(&vgpu_config_init.compatibility_mode);
  if (unlikely(ret)) {
    LOGGER(WARNING, "not defined env compatibility mode");
  }
  char *pod_name = _getenv("VGPU_POD_NAME");
  if (likely(pod_name != NULL)){
    strncpy(vgpu_config_init.pod_name, pod_name, sizeof(vgpu_config_init.pod_name)-1);
    vgpu_config_init.pod_name[sizeof(vgpu_config_init.pod_name) - 1] = '\0';
  }
  char *pod_namespace = _getenv("VGPU_POD_NAMESPACE");
  if (likely(pod_namespace != NULL)){
    strncpy(vgpu_config_init.pod_namespace, pod_namespace, sizeof(vgpu_config_init.pod_namespace)-1);
    vgpu_config_init.pod_namespace[sizeof(vgpu_config_init.pod_namespace) - 1] = '\0';
  }
  char *pod_uid = _getenv("VGPU_POD_UID");
  if (likely(pod_uid != NULL)){
    strncpy(vgpu_config_init.pod_uid, pod_uid, sizeof(vgpu_config_init.pod_uid)-1);
    vgpu_config_init.pod_uid[sizeof(vgpu_config_init.pod_uid) - 1] = '\0';
  }
  char *container_name = _getenv("VGPU_CONTAINER_NAME");
  if (likely(container_name != NULL)){
    strncpy(vgpu_config_init.container_name, container_name, sizeof(vgpu_config_init.container_name)-1);
    vgpu_config_init.container_name[sizeof(vgpu_config_init.container_name) - 1] = '\0';
  }
  char *reg_uuid = _getenv("MANAGER_CLIENT_REGISTER_UUID");
  if (reg_uuid != NULL){
    strncpy(vgpu_config_init.reg_uuid, reg_uuid, sizeof(vgpu_config_init.reg_uuid)-1);
    vgpu_config_init.reg_uuid[sizeof(vgpu_config_init.reg_uuid) - 1] = '\0';
  }
  int i;
  char uuids[UUID_BUFFER_SIZE * MAX_DEVICE_COUNT];
  ret = get_manager_device_uuids(uuids, sizeof(uuids));
  if (unlikely(ret)) {
    ret = 0;
    for (i = 0; i < MAX_DEVICE_COUNT; i++) {
      char *uuid = &uuids[i * UUID_BUFFER_SIZE];
      memset(uuid, 0, UUID_BUFFER_SIZE);
      if (get_manager_device_uuid(i, uuid, UUID_BUFFER_SIZE)) {
        strncpy(uuid, FAKE_GPU_UUID, UUID_BUFFER_SIZE - 1);
        uuid[UUID_BUFFER_SIZE - 1] = '\0';
      } else {
        ret++;  // success
      }
    }
    // When the manager's environment variables are not successful (undefined),
    // fallback to using Nvidia's environment variables to identify the GPU UUID list
    if (!ret) {
      memset(uuids, 0, sizeof(uuids));
      ret = get_nvidia_device_uuids(uuids, sizeof(uuids));
    }
  }

  int hard_cores = 0;
  int soft_cores = 0;
  double ratio = 1; // default ratio = 1
  int oversold = 0; // default disable oversold
  size_t real_memory = 0;
  char *gpu_uuids[MAX_DEVICE_COUNT];
  int device_count = strsplit(uuids, gpu_uuids, ",");
  get_vmem_node_enabled(&vgpu_config_init.vmem_node);
  get_sm_watcher_enabled(&vgpu_config_init.sm_watcher);
  for (i = 0; i < device_count; i++) {
    // skip fake uuid
    if (strcmp(gpu_uuids[i], FAKE_GPU_UUID) == 0) {
      continue;
    }
    if (snprintf(vgpu_config_init.devices[i].uuid, UUID_BUFFER_SIZE, "%s", gpu_uuids[i]) >= UUID_BUFFER_SIZE) {
      LOGGER(WARNING, "gpu uuid at index %d truncated", i);
      continue;
    }
    vgpu_config_init.devices[i].activate = 1;
    ret = get_mem_limit(i, &vgpu_config_init.devices[i].total_memory);
    if (unlikely(ret)) {
      LOGGER(VERBOSE, "gpu device %d turn off memory limit", i);
      vgpu_config_init.devices[i].memory_limit = 0;
    } else {
      vgpu_config_init.devices[i].memory_limit = 1;
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
    real_memory = vgpu_config_init.devices[i].total_memory;
    if (ratio > 1) {
      real_memory /= ratio;
      vgpu_config_init.devices[i].memory_oversold = 1;
    } else {
      vgpu_config_init.devices[i].memory_oversold = oversold;
    }
    vgpu_config_init.devices[i].real_memory = real_memory;

    ret = get_core_limit(i, &hard_cores);
    if (unlikely(ret)) {
      LOGGER(VERBOSE, "get device %d core limit failed", i);
      hard_cores = 0;
    }
    if (hard_cores > 0) {
      vgpu_config_init.devices[i].core_limit = 1;
      vgpu_config_init.devices[i].hard_limit = 1;
      vgpu_config_init.devices[i].hard_core = hard_cores;
      ret = get_core_soft_limit(i, &soft_cores);
      if (unlikely(ret)) {
        LOGGER(VERBOSE, "get device %d core soft limit failed", i);
        soft_cores = 0;
      }
      if (soft_cores > 0 && soft_cores > hard_cores) {
        LOGGER(VERBOSE, "gpu device %d turn up core soft limit", i);
        vgpu_config_init.devices[i].hard_limit = 0;
        vgpu_config_init.devices[i].soft_core = soft_cores;
      }
    } else {
      LOGGER(VERBOSE, "gpu device %d turn off core limit", i);
      vgpu_config_init.devices[i].core_limit = 0;
      vgpu_config_init.devices[i].hard_limit = 0;
    }
  }
  *data = &vgpu_config_init;
}

void load_controller_configuration() {
  if (g_vgpu_config == NULL) {
    if (unlikely(mmap_file_to_config_path(&g_vgpu_config))) {
      init_g_vgpu_config_by_env(&g_vgpu_config);
      resource_data_t *fallback = g_vgpu_config;
      if (unlikely(write_file_to_config_path(g_vgpu_config))) {
        LOGGER(ERROR, "failed to write vgpu config file %s", CONTROLLER_CONFIG_FILE_PATH);
      } else if (unlikely(mmap_file_to_config_path(&g_vgpu_config))) {
        g_vgpu_config = fallback;
      }
    }
    print_global_vgpu_config();
  }
  if (g_vgpu_config->sm_watcher && g_device_util == NULL) {
    if (mmap_file_to_util_path(&g_device_util)) {
      LOGGER(ERROR, "mmap sm watcher file failed");
    }
    if (g_device_util == NULL) {
      LOGGER(WARNING, "unable to find external SM Watcher shared cache, will roll back to nvml driver");
    }
  }
  if (g_vgpu_config->vmem_node && g_device_vmem == NULL) {
    if (mmap_file_to_vmem_node(&g_device_vmem)) {
      /* Degrade, don't die. Every dereference of g_device_vmem is NULL-checked,
       * and NULL is the default state anyway (the feature gate defaults off),
       * so this just lands the process in the common configuration. Memory
       * limiting itself isn't lost: `used` still comes from NVML regardless;
       * only vmem_used (oversold/UVA tracking, invisible to NVML) goes to 0. */
      g_device_vmem = NULL;
      LOGGER(WARNING, "unable to map vmem nodes file, will roll back to "
                      "nvml-only memory accounting (virtual memory tracking disabled)");
    } else {
      if (atexit(exit_cleanup_handler) != 0) {
        LOGGER(ERROR ,"register exit handler failed: %d", errno);
      }
      /* sigaction (not signal()), so we can chain to any host handler instead
       * of silently clobbering JVM/Go/Python signal handling. Registered only
       * on success -- with no ledger there's nothing to hand back at exit. */
      struct sigaction sa;
      memset(&sa, 0, sizeof(sa));
      sigemptyset(&sa.sa_mask);
      sa.sa_flags = SA_RESTART | SA_SIGINFO;
      sa.sa_sigaction = signal_cleanup_handler_sa;
      sigaction(SIGTERM, &sa, &g_old_sigterm_sa);
      sigaction(SIGINT,  &sa, &g_old_sigint_sa);
      sigaction(SIGHUP,  &sa, &g_old_sighup_sa);
      sigaction(SIGABRT, &sa, &g_old_sigabrt_sa);
      // Note: SIGKILL and SIGSTOP cannot be caught
      LOGGER(VERBOSE, "registered cleanup handlers for signals");
    }
  }
  // Ensure that the cleaning function can be called once every time the child process is forked.
  check_cleanup_vmem_nodes();

  if ((g_vgpu_config->compatibility_mode & CLIENT_COMPATIBILITY_MODE) == CLIENT_COMPATIBILITY_MODE) {
    LOGGER(VERBOSE, "register to remote manager: uid: %s, uuid: %s", g_vgpu_config->pod_uid, g_vgpu_config->reg_uuid);
    register_to_remote_with_data(g_vgpu_config->pod_uid, g_vgpu_config->container_name, g_vgpu_config->reg_uuid);
  }
}

nvmlReturn_t _nvmlDeviceGetHandleByIndex(unsigned int index, nvmlDevice_t *device) {
  nvmlReturn_t ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, index, device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, index, device);
  }
  return ret;
}

void init_nvml_to_host_device_index() {
  nvmlReturn_t rt;
  // Intentionally perform a real NVML init here once more before building the
  // NVML-to-host-device mapping. This front-loads NVML readiness so later hook
  // paths can assume NVML is initialized instead of paying repeated lazy-init
  // checks during the library lifetime.
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlInitWithFlags))) {
    rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlInitWithFlags, 0);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlInit_v2))) {
    rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlInit_v2);
  } else {
    rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlInit);
  }
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlInit failed, return: %d, str: %s", rt, NVML_ERROR(nvml_library_entry, rt));
  }

  unsigned int device_count;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount))) {
    rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetCount, &device_count);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount_v2))) {
    rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetCount_v2, &device_count);
  } else {
    rt = NVML_ERROR_FUNCTION_NOT_FOUND;
  }
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlDeviceGetCount call failed, return: %d, str: %s", rt, NVML_ERROR(nvml_library_entry, rt));
  }

  nvmlDevice_t device;
  for (int device_index = 0; device_index < device_count; device_index++) {
    rt = _nvmlDeviceGetHandleByIndex(device_index, &device);
    if (unlikely(rt)) {
      LOGGER(ERROR, "nvmlDeviceGetHandleByIndex call failed, nvml device: %d, return: %d, str: %s",
                     device_index, rt, NVML_ERROR(nvml_library_entry, rt));
      continue;
    }
    get_host_device_index_by_nvml_device(device);
  }
}

/* fork() child handler: re-initialise the mutexes and pthread_once_t guards
 * that protect loader.c hot paths. If a parent thread was mid-critical-
 * section at the instant of fork(), the child inherits a mutex 'locked' or
 * a once-control 'in progress' with no thread left to ever finish it --
 * every later pthread_mutex_lock/pthread_once in the child then hangs
 * forever.
 *
 * Mutexes reset: g_memory_node_lock, tid_dlsym_lock, device_index_mutex.
 *
 * Once-guards reset: g_cuda_ver_init, g_nvml_lib_init, g_cuda_lib_init --
 * gate driver-version/library loading inside load_necessary_data(), which
 * runs on every CUDA/NVML/dlsym hook entry and does real dlopen/proc-read
 * work, so a fork landing mid-call is a real (if narrow) risk. No extra
 * state needs clearing alongside the reset -- all three fully rebuild their
 * data on a re-run, and their only readers gate through
 * load_necessary_data() first, so a torn build is always replaced before
 * anything sees it. Also reset: init_nvml_host_index,
 * g_controller_config_init, g_reset_cuda_index_init.
 *
 * Deliberately NOT reset: init_dlsym_flag and g_atfork_init. Their targets
 * are already redone directly a few lines below (or, for g_atfork_init,
 * survive fork via glibc's own atfork list) -- resetting them too would
 * just make the child redo already-valid setup, risking a reinit-while-live
 * mutex instead of a harmless no-op.
 *
 * tid_dlsyms[] is cleared because parent pthread_t values are stale in the
 * child; a stale entry just misses and falls through to a real dlsym. */
void loader_child_after_fork(void) {
  // After forking, it is necessary to use it to trigger nvmlInit again to ensure that
  // subsequent steps that require nvml will not fail due to lack of initialization.
  g_cuda_ver_init = (pthread_once_t)PTHREAD_ONCE_INIT;
  g_nvml_lib_init = (pthread_once_t)PTHREAD_ONCE_INIT;
  g_cuda_lib_init = (pthread_once_t)PTHREAD_ONCE_INIT;
  init_nvml_host_index = (pthread_once_t)PTHREAD_ONCE_INIT;
  g_controller_config_init = (pthread_once_t)PTHREAD_ONCE_INIT;
  g_reset_cuda_index_init = (pthread_once_t)PTHREAD_ONCE_INIT;
  pthread_mutex_init(&g_memory_node_lock, NULL);
  pthread_mutex_init(&tid_dlsym_lock,     NULL);
  pthread_mutex_init(&device_index_mutex, NULL);
  memset(tid_dlsyms, 0, sizeof(tid_dlsyms));
  tid_dlsym_count = 0;

  /* Drop the inherited virtual-memory records -- they describe the PARENT's
   * allocations, and CUDA contexts don't survive fork. Keeping them is
   * actively harmful once the child starts allocating: the driver reuses
   * addresses, so a new dptr landing on a stale record would misattribute
   * charges, retire records it doesn't own, or misroute a free. Freeing
   * (not just re-heading the list) is safe because glibc resets the malloc
   * lock in the child via its own atfork handlers. */
  memory_node_t *entry_tmp = NULL;
  struct list_head *iter, *tmp;
  list_for_each_safe(iter, tmp, &g_memory_node->node) {
    entry_tmp = container_of(iter, memory_node_t, node);
    list_del(iter);
    free(entry_tmp);
  }
  INIT_LIST_HEAD(&g_memory_node->node);
}

/* fork() child handler implemented in cuda_hook.c. Registered lazily
 * via the g_atfork_init pthread_once below; see the block comment
 * above child_after_fork() in cuda_hook.c for the full rationale. */
extern void child_after_fork(void);

static void register_atfork_handler(void) {
  /* Tiny dedicated init function. Kept minimal so the pthread_once race
   * window (any thread that calls load_necessary_data before this returns
   * could fork into a broken pthread_once state) is just a few glibc
   * instructions wide instead of the milliseconds it would be if we
   * piggybacked on cuInit or library load. */
  (void)pthread_atfork(NULL, NULL, child_after_fork);
}

void load_necessary_data() {
  /* Register the fork-child handler before anything else, since this
   * function runs on every CUDA/NVML/dlsym hook entry -- even a process
   * that only ever touches NVML has the handler in place before it forks. */
  pthread_once(&g_atfork_init, register_atfork_handler);

  // First, determine the driver version
  pthread_once(&g_cuda_ver_init, read_version_from_proc);
  load_cuda_single_library(CUDA_ENTRY_ENUM(cuDriverGetVersion));
  // Initialize the driver library
  pthread_once(&g_nvml_lib_init, load_nvml_libraries);
  pthread_once(&g_cuda_lib_init, load_cuda_libraries);
  // Read global configuration
  pthread_once(&g_controller_config_init, load_controller_configuration);
  pthread_once(&g_reset_cuda_index_init, reset_cuda_index_mapping);
}

void init_devices_mapping() {
  pthread_once(&init_nvml_host_index, init_nvml_to_host_device_index);
}
