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
//    {.name = "cuStreamBeginCapture"},
//    {.name = "cuStreamBeginCapture_ptsz"},
//    {.name = "cuStreamEndCapture"},
//    {.name = "cuStreamEndCapture_ptsz"},
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
//    {.name = "cuStreamGetCaptureInfo"},
//    {.name = "cuStreamGetCaptureInfo_ptsz"},
    {.name = "cuThreadExchangeStreamCaptureMode"},
    {.name = "cuDeviceGetNvSciSyncAttributes"},
    {.name = "cuGraphExecHostNodeSetParams"},
    {.name = "cuGraphExecMemcpyNodeSetParams"},
    {.name = "cuGraphExecMemsetNodeSetParams"},
//    {.name = "cuGraphExecUpdate"},
//    {.name = "cuGraphExecUpdate_v2"},
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

//static int host_device_indexes[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

extern void* _dl_sym(void*, const char*, void*);
/* This is the symbol search function */
fp_dlsym real_dlsym = NULL;
void *lib_control;

extern int get_compatibility_mode(int *mode);
extern int get_mem_ratio(uint32_t index, double *ratio);
extern int get_mem_limit(uint32_t index, size_t *limit);
extern int get_core_limit(uint32_t index, int *limit);
extern int get_core_soft_limit(uint32_t index, int *limit);
extern int get_device_uuid(uint32_t index, char *uuid);
extern int get_device_uuids(char *uuids);
extern int get_mem_oversold(uint32_t index, int *limit);
extern int get_vmem_node_enabled(int *enabled);
extern int file_exist(const char *file_path);
extern int pid_exist(int pid);
extern int is_zombie_proc(int pid);

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
    .compatibility_mode = 0,
    .sm_watcher = 0,
    .vmem_node = 0,
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
    const char* glibc_versions[] = {
      "GLIBC_2.2.5",  // for amd64
      "GLIBC_2.17",   // for arm64
      "GLIBC_2.3",
      "GLIBC_2.4",
      "GLIBC_2.10",
      "GLIBC_2.18",
      "GLIBC_2.22",
       NULL
    };
    for (int i = 0; glibc_versions[i] != NULL; i++) {
      real_dlsym = dlvsym(RTLD_NEXT, "dlsym", glibc_versions[i]);
      if (real_dlsym) {
        LOGGER(VERBOSE, "found dlsym with version: %s", glibc_versions[i]);
        break;
      }
    }
    if (unlikely(!real_dlsym)) {
      real_dlsym = _dl_sym(RTLD_NEXT, "dlsym", dlsym);
      if (!real_dlsym) {
        LOGGER(FATAL, "real dlsym not found");
      }
    }
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

static void read_version_from_proc(void) {

  char *line = NULL;
  size_t len = 0;

  FILE *fp = fopen(DRIVER_VERSION_PATH, "r");
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

int mmap_file_to_config_path(resource_data_t** data) {
  const char* filename = CONTROLLER_CONFIG_FILE_PATH;
  if (unlikely(file_exist(filename) != 0)) {
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
  if (unlikely(file_exist(filename) != 0)) {
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
  if (unlikely(file_exist(VMEMORY_NODE_PATH) != 0)) {
    mkdir(VMEMORY_NODE_PATH, 0755);
  }
  const char* filename = VMEMORY_NODE_FILE_PATH;
  if (unlikely(file_exist(filename) != 0)) {
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
  LOGGER(VERBOSE, "Pod Name         : %s", g_vgpu_config->pod_name);
  LOGGER(VERBOSE, "Pod Namespace    : %s", g_vgpu_config->pod_namespace);
  LOGGER(VERBOSE, "Pod Uid          : %s", g_vgpu_config->pod_uid);
  LOGGER(VERBOSE, "Container Name   : %s", g_vgpu_config->container_name);
  LOGGER(VERBOSE, "CompatibilityMode: %d", g_vgpu_config->compatibility_mode);
  LOGGER(VERBOSE, "SM Watcher       : %s", g_vgpu_config->sm_watcher ? "enabled" : "disabled");
  LOGGER(VERBOSE, "VMemory Node     : %s", g_vgpu_config->vmem_node ? "enabled" : "disabled");
  int index = 0;
  for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
    if (g_vgpu_config->devices[i].activate) {
      LOGGER(VERBOSE, "---------------------------GPU %d---------------------------", index);
      LOGGER(VERBOSE, "GPU UUID         : %s", g_vgpu_config->devices[i].uuid);
      LOGGER(VERBOSE, "Memory Limit     : %s", g_vgpu_config->devices[i].memory_limit ? "enabled" : "disabled");
      LOGGER(VERBOSE, "+ RealMemorySize : %ld", g_vgpu_config->devices[i].real_memory);
      LOGGER(VERBOSE, "+ TotalMemorySize: %ld", g_vgpu_config->devices[i].total_memory);
      LOGGER(VERBOSE, "Cores  Limit     : %s", g_vgpu_config->devices[i].core_limit ? "enabled" : "disabled");
      LOGGER(VERBOSE, "+ HardLimit      : %s", g_vgpu_config->devices[i].hard_limit ? "enabled" : "disabled");
      LOGGER(VERBOSE, "+ HardCoreSize   : %d", g_vgpu_config->devices[i].hard_core);
      LOGGER(VERBOSE, "+ SoftCoreSize   : %d", g_vgpu_config->devices[i].soft_core);
      LOGGER(VERBOSE, "Memory Oversold  : %s", g_vgpu_config->devices[i].memory_oversold ? "enabled" : "disabled");
      index++;
    }
  }
  LOGGER(VERBOSE, "-----------------------------------------------------------");
}

int write_file_to_config_path(resource_data_t* data) {
  int wsize = 0;
  int ret = 0;
  if (unlikely(file_exist(VGPU_MANAGER_PATH) != 0)) {
    mkdir(VGPU_MANAGER_PATH, 0755);
  }
  if (unlikely(file_exist(VGPU_CONFIG_PATH) != 0)) {
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

extern entry_t cuda_hooks_entry[];
extern const int cuda_hook_nums;

extern entry_t nvml_hooks_entry[];
extern const int nvml_hook_nums;

FUNC_ATTR_VISIBLE void* dlsym(void* handle, const char* symbol) {
  static __thread int recursion_depth = 0;
  if (recursion_depth > 0) {
    LOGGER(WARNING, "recursion protection triggered for %s", symbol);
    return real_dlsym ? real_dlsym(handle, symbol) : NULL;
  }
  recursion_depth++;

  LOGGER(DETAIL, "into dlsym %s", symbol);
  init_real_dlsym();

  int i;
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
    if (likely(lib_control)) {
      result = real_dlsym(lib_control, symbol);
      if (likely(result)) {
        LOGGER(DETAIL, "search found cuda hook %s", symbol);
        _load_necessary_data();
        goto DONE;
      }
    }
    for (i = 0; i < cuda_hook_nums; i++) {
      if (unlikely(!strcmp(symbol, cuda_hooks_entry[i].name))) {
        result = cuda_hooks_entry[i].fn_ptr;
        LOGGER(DETAIL, "search found cuda hook %s", symbol);
        _load_necessary_data();
        goto DONE;
      }
    }
  } else if (strncmp(symbol, "nvml", 4) == 0) { // hijack nvml
    if (likely(lib_control)) {
      result = real_dlsym(lib_control, symbol);
      if (likely(result)) {
        LOGGER(DETAIL, "search found nvml hook %s", symbol);
        goto DONE;
      }
    }
    for (i = 0; i < nvml_hook_nums; i++) {
      if (unlikely(!strcmp(symbol, nvml_hooks_entry[i].name))) {
        result = nvml_hooks_entry[i].fn_ptr;
        LOGGER(DETAIL, "search found nvml hook %s", symbol);
        goto DONE;
      }
    }
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

// Signal handler for cleanup
void signal_cleanup_handler(int signum) {
  LOGGER(INFO, "caught signal %d, cleaning up", signum);
  exit_cleanup_handler();
  // Re-raise signal with default handler to ensure proper exit code
  signal(signum, SIG_DFL);
  raise(signum);
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
    if (g_vgpu_config->devices[index].activate && strcmp(g_vgpu_config->devices[index].uuid, uuid) == 0) {
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

int _get_host_device_index_by_cuda_device(CUdevice device) {
  int cuda_index = (int) device;
  int host_index = cuda_to_host_device_index[cuda_index];
  if (host_index < 0) {
    CUuuid cu_uuid;
    CUresult ret;
    if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceGetUuid_v2))) {
      ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuDeviceGetUuid_v2, &cu_uuid, device);
    } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceGetUuid))){
      ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuDeviceGetUuid, &cu_uuid, device);
    } else {
      ret = CUDA_ERROR_NOT_FOUND;
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
  int host_index = -1;
  pthread_mutex_lock(&device_index_mutex);
  host_index = _get_host_device_index_by_cuda_device(device);
  pthread_mutex_unlock(&device_index_mutex);
  return host_index;
}

int get_nvml_device_index_by_cuda_device(CUdevice device) {
  int cuda_index = (int) device;
  int nvml_index = -1;
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

static volatile pid_t reset_index_changed_pid = 0;

// Reset CUDA device index only when PID changes.
void reset_cuda_index_mapping() {
  pid_t pid = getpid();
  if (likely(reset_index_changed_pid == pid)) {
    return;
  }
  pthread_mutex_lock(&device_index_mutex);
  if (reset_index_changed_pid != pid) {
    for (int index = 0; index < MAX_DEVICE_COUNT; index++) {
      cuda_to_host_device_index[index] = -1;
      cuda_to_nvml_device_index[index] = -1;
    }
    reset_index_changed_pid = pid;
  }
  pthread_mutex_unlock(&device_index_mutex);
}

void malloc_gpu_virt_memory(CUdeviceptr dptr, size_t bytes, int host_index) {
  memory_node_t *new_node = NULL;
  new_node = (memory_node_t*) malloc(sizeof(memory_node_t));
  if (unlikely(!new_node)) {
    LOGGER(ERROR, "failed to allocate virt memory node");
    return;
  }

  new_node->dptr = dptr;
  new_node->bytes = bytes;
  INIT_LIST_HEAD(&new_node->node);

  pthread_mutex_lock(&g_memory_node_lock);
  list_add(&new_node->node, &g_memory_node->node);
  pthread_mutex_unlock(&g_memory_node_lock);

  if (host_index < 0 || host_index >= MAX_DEVICE_COUNT) return;
  LOGGER(VERBOSE, "malloc virt memory to host device %d, dptr %lld, size %ld", host_index, dptr, bytes);

  if (g_device_vmem != NULL) {
    int fd = device_vmem_write_lock(host_index);
    if (fd < 0) return;
    int pid = getpid();
    int found = 0;
    unsigned int processes_size = g_device_vmem->devices[host_index].processes_size;
    for (int i = 0; i < processes_size; i++) {
      if (g_device_vmem->devices[host_index].processes[i].pid == pid) {
        g_device_vmem->devices[host_index].processes[i].used += bytes;
        found = 1;
        break;
      }
    }
    if (!found) {
      g_device_vmem->devices[host_index].processes[processes_size].pid = pid;
      g_device_vmem->devices[host_index].processes[processes_size].used = bytes;
      g_device_vmem->devices[host_index].processes_size++;
    }
    device_vmem_unlock(fd, host_index);
  }
}

void free_gpu_virt_memory(CUdeviceptr dptr, int host_index) {
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
  pthread_mutex_lock(&g_memory_node_lock);
  list_del(&entry_tmp->node);
  free(entry_tmp);
  pthread_mutex_unlock(&g_memory_node_lock);

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

static volatile pid_t init_config_changed_pid = 0;
static pthread_mutex_t init_config_mutex = PTHREAD_MUTEX_INITIALIZER;

int init_g_vgpu_config_by_env() {
  g_vgpu_config = &vgpu_config_temp;
  int ret = get_compatibility_mode(&g_vgpu_config->compatibility_mode);
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
  int i;
  char uuids[UUID_BUFFER_SIZE * MAX_DEVICE_COUNT];
  ret = get_device_uuids(uuids);
  if (unlikely(ret)) {
    for (i = 0; i < MAX_DEVICE_COUNT; i++) {
      char *uuid = &uuids[i * UUID_BUFFER_SIZE];
      memset(uuid, 0, UUID_BUFFER_SIZE);
      if (get_device_uuid(i, uuid) != 0) {
        strncpy(uuid, FAKE_GPU_UUID, UUID_BUFFER_SIZE - 1);
        uuid[UUID_BUFFER_SIZE - 1] = '\0';
      }
    }
  }

  int hard_cores = 0;
  int soft_cores = 0;
  double ratio = 1; // default ratio = 1
  int oversold = 0; // default disable oversold
  size_t real_memory = 0;
  char *gpu_uuids[MAX_DEVICE_COUNT];
  int vnode_enable = 0;
  int device_count = strsplit(uuids, gpu_uuids, ",");
  get_vmem_node_enabled(&vnode_enable);
  g_vgpu_config->vmem_node = vnode_enable;
  for (i = 0; i < device_count; i++) {
    // skip fake uuid
    if (strcmp(gpu_uuids[i], FAKE_GPU_UUID) == 0) {
      continue;
    }
    strcpy(g_vgpu_config->devices[i].uuid, gpu_uuids[i]);
    g_vgpu_config->devices[i].activate = 1;
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
      if (soft_cores > 0 && soft_cores > hard_cores) {
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
  return 0;
}

int load_controller_configuration() {
  int ret = 1;
  pid_t pid = getpid();
  if (likely(init_config_changed_pid == pid)) {
    return 0;
  }
  pthread_mutex_lock(&init_config_mutex);
  // Double check lock
  if (init_config_changed_pid == pid) {
    ret = 0;
    goto DONE;
  }
  if (g_vgpu_config == NULL) {
    ret = mmap_file_to_config_path(&g_vgpu_config);
    if (unlikely(ret != 0)) {
      ret = init_g_vgpu_config_by_env();
      if (unlikely(ret != 0)) {
        g_vgpu_config = NULL;
        goto DONE;
      }
      ret = write_file_to_config_path(g_vgpu_config);
      if (unlikely(ret != 0)) {
        LOGGER(ERROR, "failed to write vgpu config file %s", CONTROLLER_CONFIG_FILE_PATH);
        goto DONE;
      }
    }
    print_global_vgpu_config();
  }
  if (g_vgpu_config->sm_watcher && g_device_util == NULL) {
    ret = mmap_file_to_util_path(CONTROLLER_SM_UTIL_FILE_PATH, &g_device_util);
    if (ret) {
      pthread_mutex_unlock(&init_config_mutex);
      LOGGER(FATAL, "mmap sm watcher file failed");
    }
  }
  if (g_vgpu_config->vmem_node && g_device_vmem == NULL) {
    ret = mmap_file_to_vmem_node(&g_device_vmem);
    if (ret) {
      pthread_mutex_unlock(&init_config_mutex);
      LOGGER(FATAL, "mmap vmem nodes file failed");
    }
    check_cleanup_vmem_nodes();
    if (atexit(exit_cleanup_handler) != 0) {
      LOGGER(ERROR ,"register exit handler failed: %d", errno);
    }
    // Register signal handlers for cleanup on crashes
    signal(SIGTERM, signal_cleanup_handler);
    signal(SIGINT, signal_cleanup_handler);
    signal(SIGHUP, signal_cleanup_handler);
    signal(SIGABRT, signal_cleanup_handler);
    // Note: SIGKILL and SIGSTOP cannot be caught
    LOGGER(VERBOSE, "registered cleanup handlers for signals");
  }

  if ((g_vgpu_config->compatibility_mode & CLIENT_COMPATIBILITY_MODE) == CLIENT_COMPATIBILITY_MODE) {
    LOGGER(VERBOSE, "register to remote manager: uid: %s", g_vgpu_config->pod_uid);
    register_to_remote_with_data(g_vgpu_config->pod_uid, g_vgpu_config->container_name);
  }
  ret = 0;
  init_config_changed_pid = pid;
DONE:
  pthread_mutex_unlock(&init_config_mutex);
  return ret;
}

void init_nvml_to_host_device_index() {
  nvmlReturn_t rt;
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
    if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
      rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, device_index, &device);
    } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
      rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, device_index, &device);
    } else {
      rt = NVML_ERROR_FUNCTION_NOT_FOUND;
    }
    if (unlikely(rt)) {
      LOGGER(ERROR, "nvmlDeviceGetHandleByIndex call failed, nvml device: %d, return: %d, str: %s",
                     device_index, rt, NVML_ERROR(nvml_library_entry, rt));
      continue;
    }
    get_host_device_index_by_nvml_device(device);
  }
}

void _load_necessary_data() {
  // First, determine the driver version
  pthread_once(&g_cuda_ver_init, read_version_from_proc);
  load_cuda_single_library(CUDA_ENTRY_ENUM(cuDriverGetVersion));
  // Initialize the driver library
  pthread_once(&g_nvml_lib_init, load_nvml_libraries);
  pthread_once(&g_cuda_lib_init, load_cuda_libraries);
  // Read global configuration
  load_controller_configuration();
  reset_cuda_index_mapping();
}

void load_necessary_data() {
  _load_necessary_data();
  pthread_once(&init_nvml_host_index, init_nvml_to_host_device_index);
}