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
// Created by thomas on 18-4-16.
//
#include <assert.h>

#include "include/hook.h"
#include "include/cuda-helper.h"

extern entry_t cuda_library_entry[];

CUresult cuDeviceGet(CUdevice *device, int ordinal) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGet, device, ordinal);
}

CUresult cuDeviceGetCount(int *count) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetCount, count);
}

CUresult cuDeviceGetName(char *name, int len, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetName, name, len, dev);
}

CUresult cuDeviceGetP2PAttribute(int *value, CUdevice_P2PAttribute attrib,
                                 CUdevice srcDevice, CUdevice dstDevice) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetP2PAttribute, value,
                         attrib, srcDevice, dstDevice);
}

CUresult cuDeviceGetByPCIBusId(CUdevice *dev, const char *pciBusId) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetByPCIBusId, dev,
                         pciBusId);
}

CUresult cuDeviceGetPCIBusId(char *pciBusId, int len, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetPCIBusId, pciBusId, len,
                         dev);
}

CUresult cuDevicePrimaryCtxRetain(CUcontext *pctx, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxRetain, pctx,
                         dev);
}

CUresult _cuDevicePrimaryCtxRelease(CUdevice dev) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDevicePrimaryCtxRelease_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxRelease_v2, dev);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDevicePrimaryCtxRelease))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxRelease, dev);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuDevicePrimaryCtxRelease_v2(CUdevice dev) {
  return _cuDevicePrimaryCtxRelease(dev);
}

CUresult cuDevicePrimaryCtxRelease(CUdevice dev) {
  return _cuDevicePrimaryCtxRelease(dev);
}

CUresult _cuDevicePrimaryCtxSetFlags(CUdevice dev, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDevicePrimaryCtxSetFlags_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxSetFlags_v2,
                           dev, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDevicePrimaryCtxSetFlags))){
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxSetFlags,
                           dev, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuDevicePrimaryCtxSetFlags_v2(CUdevice dev, unsigned int flags) {
  return _cuDevicePrimaryCtxSetFlags(dev, flags);
}

CUresult cuDevicePrimaryCtxSetFlags(CUdevice dev, unsigned int flags) {
  return _cuDevicePrimaryCtxSetFlags(dev, flags);
}

CUresult cuDevicePrimaryCtxGetState(CUdevice dev, unsigned int *flags,
                                    int *active) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxGetState, dev,
                         flags, active);
}

CUresult _cuDevicePrimaryCtxReset(CUdevice dev) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDevicePrimaryCtxReset_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxReset_v2, dev);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDevicePrimaryCtxReset))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDevicePrimaryCtxReset, dev);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuDevicePrimaryCtxReset_v2(CUdevice dev) {
  return _cuDevicePrimaryCtxReset(dev);
}

CUresult cuDevicePrimaryCtxReset(CUdevice dev) {
  return _cuDevicePrimaryCtxReset(dev);
}

CUresult cuCtxCreate_v3(CUcontext *pctx, CUexecAffinityParam *paramsArray,
                        int numParams, unsigned int flags, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxCreate_v3, pctx, paramsArray,
                         numParams, flags, dev);
}

CUresult cuCtxCreate_v4(CUcontext *pctx, CUctxCreateParams *ctxCreateParams,
                        unsigned int flags, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxCreate_v4, pctx, ctxCreateParams,
                         flags, dev);
}

CUresult _cuCtxCreate(CUcontext *pctx, unsigned int flags, CUdevice dev) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxCreate_v2, pctx, flags, dev);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxCreate))){
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxCreate, pctx, flags, dev);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuCtxCreate_v2(CUcontext *pctx, unsigned int flags, CUdevice dev) {
  return _cuCtxCreate(pctx, flags, dev);
}

CUresult cuCtxCreate(CUcontext *pctx, unsigned int flags, CUdevice dev) {
  return _cuCtxCreate(pctx, flags, dev);
}

CUresult cuCtxGetFlags(unsigned int *flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetFlags, flags);
}

CUresult cuCtxSetCurrent(CUcontext ctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxSetCurrent, ctx);
}

CUresult cuCtxGetCurrent(CUcontext *pctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetCurrent, pctx);
}

CUresult cuCtxDetach(CUcontext ctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxDetach, ctx);
}

CUresult cuCtxGetApiVersion(CUcontext ctx, unsigned int *version) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetApiVersion, ctx, version);
}

CUresult cuCtxGetDevice(CUdevice *device) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, device);
}

CUresult cuCtxGetLimit(size_t *pvalue, CUlimit limit) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetLimit, pvalue, limit);
}

CUresult cuCtxGetCacheConfig(CUfunc_cache *pconfig) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetCacheConfig, pconfig);
}

CUresult cuCtxSetCacheConfig(CUfunc_cache config) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxSetCacheConfig, config);
}

CUresult cuCtxGetSharedMemConfig(CUsharedconfig *pConfig) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetSharedMemConfig, pConfig);
}

CUresult cuCtxGetStreamPriorityRange(int *leastPriority,
                                     int *greatestPriority) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetStreamPriorityRange,
                         leastPriority, greatestPriority);
}

CUresult cuCtxSetSharedMemConfig(CUsharedconfig config) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxSetSharedMemConfig, config);
}

CUresult cuCtxSynchronize(void) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxSynchronize);
}

CUresult cuModuleUnload(CUmodule hmod) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleUnload, hmod);
}

CUresult cuModuleGetFunction(CUfunction *hfunc, CUmodule hmod,
                             const char *name) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleGetFunction, hfunc, hmod,
                         name);
}

CUresult _cuModuleGetGlobal(CUdeviceptr *dptr, size_t *bytes, CUmodule hmod,
                           const char *name) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuModuleGetGlobal_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleGetGlobal_v2,
                           dptr, bytes, hmod, name);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuModuleGetGlobal))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleGetGlobal,
                           dptr, bytes, hmod, name);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuModuleGetGlobal_v2(CUdeviceptr *dptr, size_t *bytes, CUmodule hmod,
                              const char *name) {
  return _cuModuleGetGlobal(dptr, bytes, hmod, name);
}

CUresult cuModuleGetGlobal(CUdeviceptr *dptr, size_t *bytes, CUmodule hmod,
                           const char *name) {
  return _cuModuleGetGlobal(dptr, bytes, hmod, name);
}

CUresult cuModuleGetTexRef(CUtexref *pTexRef, CUmodule hmod, const char *name) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleGetTexRef, pTexRef, hmod,
                         name);
}

CUresult cuModuleGetSurfRef(CUsurfref *pSurfRef, CUmodule hmod,
                            const char *name) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleGetSurfRef, pSurfRef, hmod,
                         name);
}

CUresult cuLinkCreate_v2(unsigned int numOptions, CUjit_option *options,
                         void **optionValues, CUlinkState *stateOut) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkCreate_v2, numOptions,
                         options, optionValues, stateOut);
}

CUresult cuLinkCreate(unsigned int numOptions, CUjit_option *options,
                      void **optionValues, CUlinkState *stateOut) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkCreate, numOptions, options,
                         optionValues, stateOut);
}

CUresult cuLinkAddData_v2(CUlinkState state, CUjitInputType type, void *data,
                          size_t size, const char *name,
                          unsigned int numOptions, CUjit_option *options,
                          void **optionValues) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkAddData_v2, state, type,
                         data, size, name, numOptions, options, optionValues);
}

CUresult cuLinkAddData(CUlinkState state, CUjitInputType type, void *data,
                       size_t size, const char *name, unsigned int numOptions,
                       CUjit_option *options, void **optionValues) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkAddData, state, type, data,
                         size, name, numOptions, options, optionValues);
}

CUresult cuLinkAddFile_v2(CUlinkState state, CUjitInputType type,
                          const char *path, unsigned int numOptions,
                          CUjit_option *options, void **optionValues) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkAddFile_v2, state, type,
                         path, numOptions, options, optionValues);
}

CUresult cuLinkAddFile(CUlinkState state, CUjitInputType type, const char *path,
                       unsigned int numOptions, CUjit_option *options,
                       void **optionValues) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkAddFile, state, type, path,
                         numOptions, options, optionValues);
}

CUresult cuLinkComplete(CUlinkState state, void **cubinOut, size_t *sizeOut) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkComplete, state, cubinOut,
                         sizeOut);
}

CUresult cuLinkDestroy(CUlinkState state) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLinkDestroy, state);
}

CUresult _cuMemGetAddressRange(CUdeviceptr *pbase, size_t *psize, CUdeviceptr dptr) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetAddressRange_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetAddressRange_v2,
                           pbase, psize, dptr);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetAddressRange))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetAddressRange,
                           pbase, psize, dptr);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemGetAddressRange_v2(CUdeviceptr *pbase, size_t *psize, CUdeviceptr dptr) {
  return _cuMemGetAddressRange(pbase, psize, dptr);
}

CUresult cuMemGetAddressRange(CUdeviceptr *pbase, size_t *psize, CUdeviceptr dptr) {
  return _cuMemGetAddressRange(pbase, psize, dptr);
}

CUresult cuMemFreeHost(void *p) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemFreeHost, p);
}

CUresult cuMemHostAlloc(void **pp, size_t bytesize, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostAlloc, pp, bytesize,
                         Flags);
}

CUresult _cuMemHostGetDevicePointer(CUdeviceptr *pdptr, void *p, unsigned int Flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemHostGetDevicePointer_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry,  cuMemHostGetDevicePointer_v2,
                           pdptr, p, Flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemHostGetDevicePointer))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostGetDevicePointer,
                           pdptr, p, Flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemHostGetDevicePointer_v2(CUdeviceptr *pdptr, void *p, unsigned int Flags) {
  return _cuMemHostGetDevicePointer(pdptr, p, Flags);
}

CUresult cuMemHostGetDevicePointer(CUdeviceptr *pdptr, void *p, unsigned int Flags) {
  return _cuMemHostGetDevicePointer(pdptr, p, Flags);
}

CUresult cuMemHostGetFlags(unsigned int *pFlags, void *p) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostGetFlags, pFlags, p);
}

CUresult _cuMemHostRegister(void *p, size_t bytesize, unsigned int Flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemHostRegister_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostRegister_v2,
                           p, bytesize, Flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemHostRegister))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostRegister,
                           p, bytesize, Flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemHostRegister_v2(void *p, size_t bytesize, unsigned int Flags) {
  return _cuMemHostRegister(p, bytesize, Flags);
}

CUresult cuMemHostRegister(void *p, size_t bytesize, unsigned int Flags) {
  return _cuMemHostRegister(p, bytesize, Flags);
}

CUresult cuMemHostUnregister(void *p) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostUnregister, p);
}

CUresult cuPointerGetAttribute(void *data, CUpointer_attribute attribute,
                               CUdeviceptr ptr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuPointerGetAttribute, data,
                         attribute, ptr);
}

CUresult cuPointerGetAttributes(unsigned int numAttributes,
                                CUpointer_attribute *attributes, void **data,
                                CUdeviceptr ptr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuPointerGetAttributes,
                         numAttributes, attributes, data, ptr);
}

CUresult cuMemcpy_ptds(CUdeviceptr dst, CUdeviceptr src, size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy_ptds, dst, src, ByteCount);
}

CUresult cuMemcpy(CUdeviceptr dst, CUdeviceptr src, size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy), dst, src, ByteCount);
}

CUresult cuMemcpyAsync_ptsz(CUdeviceptr dst, CUdeviceptr src, size_t ByteCount,
                            CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAsync_ptsz, dst, src,
                          ByteCount, hStream);
}

CUresult cuMemcpyAsync(CUdeviceptr dst, CUdeviceptr src, size_t ByteCount,
                       CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyAsync),
                          dst, src, ByteCount, hStream);
}

CUresult cuMemcpyPeer_ptds(CUdeviceptr dstDevice, CUcontext dstContext,
                           CUdeviceptr srcDevice, CUcontext srcContext,
                           size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyPeer_ptds, dstDevice,
                         dstContext, srcDevice, srcContext, ByteCount);
}

CUresult cuMemcpyPeer(CUdeviceptr dstDevice, CUcontext dstContext,
                      CUdeviceptr srcDevice, CUcontext srcContext,
                      size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyPeer), dstDevice,
                         dstContext, srcDevice, srcContext, ByteCount);
}

CUresult cuMemcpyPeerAsync_ptsz(CUdeviceptr dstDevice, CUcontext dstContext,
                                CUdeviceptr srcDevice, CUcontext srcContext,
                                size_t ByteCount, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyPeerAsync_ptsz, dstDevice,
                         dstContext, srcDevice, srcContext, ByteCount, hStream);
}

CUresult cuMemcpyPeerAsync(CUdeviceptr dstDevice, CUcontext dstContext,
                           CUdeviceptr srcDevice, CUcontext srcContext,
                           size_t ByteCount, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyPeerAsync), dstDevice,
                         dstContext, srcDevice, srcContext, ByteCount, hStream);
}

CUresult cuMemcpyHtoD_v2_ptds(CUdeviceptr dstDevice, const void *srcHost,
                              size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoD_v2_ptds, dstDevice,
                         srcHost, ByteCount);
}

CUresult _cuMemcpyHtoD(CUdeviceptr dstDevice, const void *srcHost, size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyHtoD_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyHtoD_v2),
                           dstDevice, srcHost, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyHtoD))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoD, dstDevice, srcHost, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpyHtoD_v2(CUdeviceptr dstDevice, const void *srcHost,
                         size_t ByteCount) {
  return _cuMemcpyHtoD(dstDevice, srcHost, ByteCount);
}

CUresult cuMemcpyHtoD(CUdeviceptr dstDevice, const void *srcHost,
                      size_t ByteCount) {
  return _cuMemcpyHtoD(dstDevice, srcHost, ByteCount);
}

CUresult cuMemcpyHtoDAsync_v2_ptsz(CUdeviceptr dstDevice, const void *srcHost,
                                   size_t ByteCount, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoDAsync_v2_ptsz,
                         dstDevice, srcHost, ByteCount, hStream);
}

CUresult _cuMemcpyHtoDAsync(CUdeviceptr dstDevice, const void *srcHost,
                           size_t ByteCount, CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyHtoDAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyHtoDAsync_v2),
                           dstDevice, srcHost, ByteCount, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyHtoDAsync))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoDAsync,
                           dstDevice, srcHost, ByteCount, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpyHtoDAsync_v2(CUdeviceptr dstDevice, const void *srcHost,
                              size_t ByteCount, CUstream hStream) {
  return _cuMemcpyHtoDAsync(dstDevice, srcHost, ByteCount, hStream);
}

CUresult cuMemcpyHtoDAsync(CUdeviceptr dstDevice, const void *srcHost,
                           size_t ByteCount, CUstream hStream) {
  return _cuMemcpyHtoDAsync(dstDevice, srcHost, ByteCount, hStream);
}

CUresult cuMemcpyDtoH_v2_ptds(void *dstHost, CUdeviceptr srcDevice,
                              size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoH_v2_ptds, dstHost,
                         srcDevice, ByteCount);
}

CUresult _cuMemcpyDtoH(void *dstHost, CUdeviceptr srcDevice, size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyDtoH_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyDtoH_v2),
                                       dstHost, srcDevice, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyDtoH))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoH, dstHost,
                                                srcDevice, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemcpyDtoH_v2(void *dstHost, CUdeviceptr srcDevice,
                         size_t ByteCount) {
  return _cuMemcpyDtoH(dstHost, srcDevice, ByteCount);
}

CUresult cuMemcpyDtoH(void *dstHost, CUdeviceptr srcDevice, size_t ByteCount) {
  return _cuMemcpyDtoH(dstHost, srcDevice, ByteCount);
}

CUresult cuMemcpyDtoHAsync_v2_ptsz(void *dstHost, CUdeviceptr srcDevice,
                                   size_t ByteCount, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoHAsync_v2_ptsz, dstHost,
                         srcDevice, ByteCount, hStream);
}

CUresult _cuMemcpyDtoHAsync(void *dstHost, CUdeviceptr srcDevice,
                           size_t ByteCount, CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyDtoHAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyDtoHAsync_v2), dstHost,
                                              srcDevice, ByteCount, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyDtoHAsync))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoHAsync, dstHost,
                                              srcDevice, ByteCount, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpyDtoHAsync_v2(void *dstHost, CUdeviceptr srcDevice,
                              size_t ByteCount, CUstream hStream) {
  return _cuMemcpyDtoHAsync(dstHost, srcDevice, ByteCount, hStream);
}

CUresult cuMemcpyDtoHAsync(void *dstHost, CUdeviceptr srcDevice,
                           size_t ByteCount, CUstream hStream) {
  return _cuMemcpyDtoHAsync(dstHost, srcDevice, ByteCount, hStream);
}

CUresult cuMemcpyDtoD_v2_ptds(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                              size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoD_v2_ptds, dstDevice,
                         srcDevice, ByteCount);
}

CUresult _cuMemcpyDtoD(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                      size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyDtoD_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyDtoD_v2),
                           dstDevice, srcDevice, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyDtoD))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoD,
                           dstDevice, srcDevice, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemcpyDtoD_v2(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                         size_t ByteCount) {
  return _cuMemcpyDtoD(dstDevice, srcDevice, ByteCount);
}

CUresult cuMemcpyDtoD(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                      size_t ByteCount) {
  return _cuMemcpyDtoD(dstDevice, srcDevice, ByteCount);
}

CUresult cuMemcpyDtoDAsync_v2_ptsz(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                                   size_t ByteCount, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoDAsync_v2_ptsz,
                         dstDevice, srcDevice, ByteCount, hStream);
}

CUresult _cuMemcpyDtoDAsync(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                           size_t ByteCount, CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyDtoDAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyDtoDAsync_v2),
                                  dstDevice, srcDevice, ByteCount, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyDtoDAsync))){
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoDAsync, dstDevice,
                                             srcDevice, ByteCount, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemcpyDtoDAsync_v2(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                              size_t ByteCount, CUstream hStream) {
  return _cuMemcpyDtoDAsync(dstDevice, srcDevice, ByteCount, hStream);
}

CUresult cuMemcpyDtoDAsync(CUdeviceptr dstDevice, CUdeviceptr srcDevice,
                           size_t ByteCount, CUstream hStream) {
  return _cuMemcpyDtoDAsync(dstDevice, srcDevice, ByteCount, hStream);
}

CUresult cuMemcpy2DUnaligned_v2_ptds(const CUDA_MEMCPY2D *pCopy) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy2DUnaligned_v2_ptds,
                         pCopy);
}

CUresult _cuMemcpy2DUnaligned(const CUDA_MEMCPY2D *pCopy) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy2DUnaligned_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy2DUnaligned_v2), pCopy);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpy2DUnaligned))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy2DUnaligned, pCopy);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpy2DUnaligned_v2(const CUDA_MEMCPY2D *pCopy) {
  return _cuMemcpy2DUnaligned(pCopy);
}

CUresult cuMemcpy2DUnaligned(const CUDA_MEMCPY2D *pCopy) {
  return _cuMemcpy2DUnaligned(pCopy);
}

CUresult cuMemcpy2DAsync_v2_ptsz(const CUDA_MEMCPY2D *pCopy, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy2DAsync_v2_ptsz, pCopy,
                         hStream);
}


CUresult _cuMemcpy2DAsync(const CUDA_MEMCPY2D *pCopy, CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpy2DAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpy2DAsync_v2), pCopy, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpy2DAsync))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy2DAsync, pCopy, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpy2DAsync_v2(const CUDA_MEMCPY2D *pCopy, CUstream hStream) {
  return _cuMemcpy2DAsync(pCopy, hStream);
}

CUresult cuMemcpy2DAsync(const CUDA_MEMCPY2D *pCopy, CUstream hStream) {
  return _cuMemcpy2DAsync(pCopy, hStream);
}

CUresult cuMemcpy3D_v2_ptds(const CUDA_MEMCPY3D *pCopy) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy3D_v2_ptds, pCopy);
}

CUresult _cuMemcpy3D(const CUDA_MEMCPY3D *pCopy) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy3D_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy3D_v2), pCopy);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpy3D))){
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy3D, pCopy);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpy3D_v2(const CUDA_MEMCPY3D *pCopy) {
  return _cuMemcpy3D(pCopy);
}

CUresult cuMemcpy3D(const CUDA_MEMCPY3D *pCopy) {
  return _cuMemcpy3D(pCopy);
}

CUresult cuMemcpy3DAsync_v2_ptsz(const CUDA_MEMCPY3D *pCopy, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy3DAsync_v2_ptsz, pCopy,
                         hStream);
}

CUresult _cuMemcpy3DAsync(const CUDA_MEMCPY3D *pCopy, CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpy3DAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpy3DAsync_v2), pCopy, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpy3DAsync))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy3DAsync, pCopy, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemcpy3DAsync_v2(const CUDA_MEMCPY3D *pCopy, CUstream hStream) {
  return _cuMemcpy3DAsync(pCopy, hStream);
}

CUresult cuMemcpy3DAsync(const CUDA_MEMCPY3D *pCopy, CUstream hStream) {
  return _cuMemcpy3DAsync(pCopy, hStream);
}

CUresult cuMemcpy3DPeer_ptds(const CUDA_MEMCPY3D_PEER *pCopy) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy3DPeer_ptds, pCopy);
}

CUresult cuMemcpy3DPeer(const CUDA_MEMCPY3D_PEER *pCopy) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy3DPeer), pCopy);
}

CUresult cuMemcpy3DPeerAsync_ptsz(const CUDA_MEMCPY3D_PEER *pCopy,
                                  CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy3DPeerAsync_ptsz, pCopy,
                         hStream);
}

CUresult cuMemcpy3DPeerAsync(const CUDA_MEMCPY3D_PEER *pCopy,
                             CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpy3DPeerAsync), pCopy,
                         hStream);
}

CUresult cuMemsetD8_v2_ptds(CUdeviceptr dstDevice, unsigned char uc, size_t N) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD8_v2_ptds, dstDevice, uc, N);
}

CUresult _cuMemsetD8(CUdeviceptr dstDevice, unsigned char uc, size_t N) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD8_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD8_v2), dstDevice, uc, N);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemsetD8))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD8, dstDevice, uc, N);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemsetD8_v2(CUdeviceptr dstDevice, unsigned char uc, size_t N) {
  return _cuMemsetD8(dstDevice, uc, N);
}

CUresult cuMemsetD8(CUdeviceptr dstDevice, unsigned char uc, size_t N) {
  return _cuMemsetD8(dstDevice, uc, N);
}

CUresult cuMemsetD8Async_ptsz(CUdeviceptr dstDevice, unsigned char uc, size_t N,
                              CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD8Async_ptsz, dstDevice,
                          uc, N, hStream);
}

CUresult cuMemsetD8Async(CUdeviceptr dstDevice, unsigned char uc, size_t N,
                         CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemsetD8Async),
                         dstDevice, uc, N, hStream);
}

CUresult cuMemsetD2D8_v2_ptds(CUdeviceptr dstDevice, size_t dstPitch,
                              unsigned char uc, size_t Width, size_t Height) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D8_v2_ptds, dstDevice,
                         dstPitch, uc, Width, Height);
}

CUresult _cuMemsetD2D8(CUdeviceptr dstDevice, size_t dstPitch,
                       unsigned char uc, size_t Width, size_t Height) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD2D8_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD2D8_v2),
                                 dstDevice, dstPitch, uc, Width, Height);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemsetD2D8))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D8, dstDevice,
                                            dstPitch, uc, Width, Height);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemsetD2D8_v2(CUdeviceptr dstDevice, size_t dstPitch,
                         unsigned char uc, size_t Width, size_t Height) {
  return _cuMemsetD2D8(dstDevice, dstPitch, uc, Width, Height);
}

CUresult cuMemsetD2D8(CUdeviceptr dstDevice, size_t dstPitch,
                      unsigned char uc, size_t Width, size_t Height) {
  return _cuMemsetD2D8(dstDevice, dstPitch, uc, Width, Height);
}

CUresult cuMemsetD2D8Async_ptsz(CUdeviceptr dstDevice, size_t dstPitch,
                                unsigned char uc, size_t Width, size_t Height,
                                CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D8Async_ptsz, dstDevice,
                         dstPitch, uc, Width, Height, hStream);
}

CUresult cuMemsetD2D8Async(CUdeviceptr dstDevice, size_t dstPitch,
                           unsigned char uc, size_t Width, size_t Height,
                           CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemsetD2D8Async), dstDevice,
                         dstPitch, uc, Width, Height, hStream);
}

CUresult cuFuncSetCacheConfig(CUfunction hfunc, CUfunc_cache config) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncSetCacheConfig, hfunc,
                         config);
}

CUresult cuFuncSetSharedMemConfig(CUfunction hfunc, CUsharedconfig config) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncSetSharedMemConfig, hfunc,
                         config);
}

CUresult cuFuncGetAttribute(int *pi, CUfunction_attribute attrib,
                            CUfunction hfunc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncGetAttribute, pi, attrib,
                         hfunc);
}

CUresult _cuArrayGetDescriptor(CUDA_ARRAY_DESCRIPTOR *pArrayDescriptor,
                              CUarray hArray) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArrayGetDescriptor_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry,cuArrayGetDescriptor_v2,
                                            pArrayDescriptor, hArray);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArrayGetDescriptor))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayGetDescriptor,
                                            pArrayDescriptor, hArray);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuArrayGetDescriptor_v2(CUDA_ARRAY_DESCRIPTOR *pArrayDescriptor,
                                 CUarray hArray) {
  return _cuArrayGetDescriptor(pArrayDescriptor, hArray);
}

CUresult cuArrayGetDescriptor(CUDA_ARRAY_DESCRIPTOR *pArrayDescriptor,
                              CUarray hArray) {
  return _cuArrayGetDescriptor(pArrayDescriptor, hArray);
}

CUresult _cuArray3DGetDescriptor(CUDA_ARRAY3D_DESCRIPTOR *pArrayDescriptor,
                                CUarray hArray) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArray3DGetDescriptor_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry,cuArray3DGetDescriptor_v2,
                                              pArrayDescriptor, hArray);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArray3DGetDescriptor))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArray3DGetDescriptor,
                                              pArrayDescriptor, hArray);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuArray3DGetDescriptor_v2(CUDA_ARRAY3D_DESCRIPTOR *pArrayDescriptor,
                                   CUarray hArray) {
  return _cuArray3DGetDescriptor(pArrayDescriptor, hArray);
}

CUresult cuArray3DGetDescriptor(CUDA_ARRAY3D_DESCRIPTOR *pArrayDescriptor,
                                CUarray hArray) {
  return _cuArray3DGetDescriptor(pArrayDescriptor, hArray);
}

CUresult cuArrayDestroy(CUarray hArray) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayDestroy, hArray);
}

CUresult cuMipmappedArrayGetLevel(CUarray *pLevelArray,
                                  CUmipmappedArray hMipmappedArray,
                                  unsigned int level) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMipmappedArrayGetLevel,
                         pLevelArray, hMipmappedArray, level);
}

CUresult cuMipmappedArrayDestroy(CUmipmappedArray hMipmappedArray) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMipmappedArrayDestroy,
                         hMipmappedArray);
}

CUresult cuTexRefCreate(CUtexref *pTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefCreate, pTexRef);
}

CUresult cuTexRefDestroy(CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefDestroy, hTexRef);
}

CUresult cuTexRefSetArray(CUtexref hTexRef, CUarray hArray,
                          unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetArray, hTexRef, hArray,
                         Flags);
}

CUresult cuTexRefSetMipmappedArray(CUtexref hTexRef,
                                   CUmipmappedArray hMipmappedArray,
                                   unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetMipmappedArray, hTexRef,
                         hMipmappedArray, Flags);
}

CUresult _cuTexRefSetAddress(size_t *ByteOffset, CUtexref hTexRef,
                             CUdeviceptr dptr, size_t bytes) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuTexRefSetAddress_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry,cuTexRefSetAddress_v2,
                                  ByteOffset, hTexRef, dptr, bytes);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuTexRefSetAddress))){
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetAddress, ByteOffset,
                                                hTexRef, dptr, bytes);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuTexRefSetAddress_v2(size_t *ByteOffset, CUtexref hTexRef,
                               CUdeviceptr dptr, size_t bytes) {
  return _cuTexRefSetAddress(ByteOffset, hTexRef, dptr, bytes);
}

CUresult cuTexRefSetAddress(size_t *ByteOffset, CUtexref hTexRef,
                            CUdeviceptr dptr, size_t bytes) {
  return _cuTexRefSetAddress(ByteOffset, hTexRef, dptr, bytes);
}

CUresult _cuTexRefSetAddress2D(CUtexref hTexRef,
                              const CUDA_ARRAY_DESCRIPTOR *desc,
                              CUdeviceptr dptr, size_t Pitch) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuTexRefSetAddress2D_v3))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetAddress2D_v3,
                                        hTexRef, desc, dptr, Pitch);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuTexRefSetAddress2D_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetAddress2D_v2, 
                                        hTexRef, desc, dptr, Pitch);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuTexRefSetAddress2D))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetAddress2D, 
                                        hTexRef, desc, dptr, Pitch);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuTexRefSetAddress2D_v3(CUtexref hTexRef,
                                 const CUDA_ARRAY_DESCRIPTOR *desc,
                                 CUdeviceptr dptr, size_t Pitch) {
  return _cuTexRefSetAddress2D(hTexRef, desc, dptr, Pitch);
}

CUresult cuTexRefSetAddress2D_v2(CUtexref hTexRef,
                                 const CUDA_ARRAY_DESCRIPTOR *desc,
                                 CUdeviceptr dptr, size_t Pitch) {
  return _cuTexRefSetAddress2D(hTexRef, desc, dptr, Pitch);
}

CUresult cuTexRefSetAddress2D(CUtexref hTexRef,
                              const CUDA_ARRAY_DESCRIPTOR *desc,
                              CUdeviceptr dptr, size_t Pitch) {
  return _cuTexRefSetAddress2D(hTexRef, desc, dptr, Pitch);
}

CUresult cuTexRefSetFormat(CUtexref hTexRef, CUarray_format fmt,
                           int NumPackedComponents) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetFormat, hTexRef, fmt,
                         NumPackedComponents);
}

CUresult cuTexRefSetAddressMode(CUtexref hTexRef, int dim, CUaddress_mode am) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetAddressMode, hTexRef,
                         dim, am);
}

CUresult cuTexRefSetFilterMode(CUtexref hTexRef, CUfilter_mode fm) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetFilterMode, hTexRef,
                         fm);
}

CUresult cuTexRefSetMipmapFilterMode(CUtexref hTexRef, CUfilter_mode fm) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetMipmapFilterMode,
                         hTexRef, fm);
}

CUresult cuTexRefSetMipmapLevelBias(CUtexref hTexRef, float bias) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetMipmapLevelBias,
                         hTexRef, bias);
}

CUresult cuTexRefSetMipmapLevelClamp(CUtexref hTexRef,
                                     float minMipmapLevelClamp,
                                     float maxMipmapLevelClamp) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetMipmapLevelClamp,
                         hTexRef, minMipmapLevelClamp, maxMipmapLevelClamp);
}

CUresult cuTexRefSetMaxAnisotropy(CUtexref hTexRef, unsigned int maxAniso) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetMaxAnisotropy, hTexRef,
                         maxAniso);
}

CUresult cuTexRefSetFlags(CUtexref hTexRef, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetFlags, hTexRef, Flags);
}

CUresult cuTexRefSetBorderColor(CUtexref hTexRef, float *pBorderColor) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefSetBorderColor, hTexRef,
                         pBorderColor);
}

CUresult cuTexRefGetBorderColor(float *pBorderColor, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetBorderColor,
                         pBorderColor, hTexRef);
}

CUresult cuSurfRefSetArray(CUsurfref hSurfRef, CUarray hArray,
                           unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuSurfRefSetArray, hSurfRef,
                         hArray, Flags);
}

CUresult cuTexObjectCreate(CUtexObject *pTexObject,
                           const CUDA_RESOURCE_DESC *pResDesc,
                           const CUDA_TEXTURE_DESC *pTexDesc,
                           const CUDA_RESOURCE_VIEW_DESC *pResViewDesc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexObjectCreate, pTexObject,
                         pResDesc, pTexDesc, pResViewDesc);
}

CUresult cuTexObjectDestroy(CUtexObject texObject) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexObjectDestroy, texObject);
}

CUresult cuTexObjectGetResourceDesc(CUDA_RESOURCE_DESC *pResDesc,
                                    CUtexObject texObject) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexObjectGetResourceDesc,
                         pResDesc, texObject);
}

CUresult cuTexObjectGetTextureDesc(CUDA_TEXTURE_DESC *pTexDesc,
                                   CUtexObject texObject) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexObjectGetTextureDesc,
                         pTexDesc, texObject);
}

CUresult cuTexObjectGetResourceViewDesc(CUDA_RESOURCE_VIEW_DESC *pResViewDesc,
                                        CUtexObject texObject) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexObjectGetResourceViewDesc,
                         pResViewDesc, texObject);
}

CUresult cuSurfObjectCreate(CUsurfObject *pSurfObject,
                            const CUDA_RESOURCE_DESC *pResDesc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuSurfObjectCreate, pSurfObject,
                         pResDesc);
}

CUresult cuSurfObjectDestroy(CUsurfObject surfObject) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuSurfObjectDestroy, surfObject);
}

CUresult cuSurfObjectGetResourceDesc(CUDA_RESOURCE_DESC *pResDesc,
                                     CUsurfObject surfObject) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuSurfObjectGetResourceDesc,
                         pResDesc, surfObject);
}

CUresult cuEventCreate(CUevent *phEvent, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventCreate, phEvent, Flags);
}

CUresult cuEventRecord(CUevent hEvent, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuEventRecord), hEvent, hStream);
}

CUresult cuEventRecord_ptsz(CUevent hEvent, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventRecord_ptsz, hEvent,
                         hStream);
}

CUresult cuEventQuery(CUevent hEvent) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventQuery, hEvent);
}

CUresult cuEventSynchronize(CUevent hEvent) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventSynchronize, hEvent);
}

CUresult cuEventDestroy_v2(CUevent hEvent) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventDestroy_v2, hEvent);
}

CUresult cuEventDestroy(CUevent hEvent) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventDestroy, hEvent);
}

CUresult cuEventElapsedTime(float *pMilliseconds, CUevent hStart,
                            CUevent hEnd) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventElapsedTime, pMilliseconds,
                         hStart, hEnd);
}

CUresult _cuStreamWaitValue32(CUstream stream, CUdeviceptr addr,
                             cuuint32_t value, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue32_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue32_v2),
                                           stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue32)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue32),
                                           stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult  cuStreamWaitValue32_v2(CUstream stream, CUdeviceptr addr,
                                 cuuint32_t value, unsigned int flags){
  return _cuStreamWaitValue32(stream, addr, value, flags);
}

CUresult cuStreamWaitValue32(CUstream stream, CUdeviceptr addr,
                             cuuint32_t value, unsigned int flags) {
  return _cuStreamWaitValue32(stream, addr, value, flags);
}

CUresult _cuStreamWaitValue32_ptsz(CUstream stream, CUdeviceptr addr,
                                  cuuint32_t value, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWaitValue32_v2_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWaitValue32_v2_ptsz,
                                           stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWaitValue32_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWaitValue32_ptsz,
                                           stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamWaitValue32_v2_ptsz(CUstream stream, CUdeviceptr addr,
                                 cuuint32_t value, unsigned int flags){
  return _cuStreamWaitValue32_ptsz(stream, addr, value, flags);
}

CUresult cuStreamWaitValue32_ptsz(CUstream stream, CUdeviceptr addr,
                                  cuuint32_t value, unsigned int flags) {
  return _cuStreamWaitValue32_ptsz(stream, addr, value, flags);
}

CUresult _cuStreamWriteValue32(CUstream stream, CUdeviceptr addr,
                              cuuint32_t value, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue32_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue32_v2),
                                        stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue32)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue32),
                                        stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult  cuStreamWriteValue32_v2(CUstream stream, CUdeviceptr addr,
                                  cuuint32_t value, unsigned int flags){
  return _cuStreamWriteValue32(stream, addr, value, flags);
}

CUresult cuStreamWriteValue32(CUstream stream, CUdeviceptr addr,
                              cuuint32_t value, unsigned int flags) {
  return _cuStreamWriteValue32(stream, addr, value, flags);
}

CUresult _cuStreamWriteValue32_ptsz(CUstream stream, CUdeviceptr addr,
                                   cuuint32_t value, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWriteValue32_v2_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWriteValue32_v2_ptsz,
                                        stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWriteValue32_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWriteValue32_ptsz,
                                        stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamWriteValue32_v2_ptsz(CUstream stream, CUdeviceptr addr,
                                   cuuint32_t value, unsigned int flags) {
  return _cuStreamWriteValue32_ptsz(stream, addr, value, flags);
}


CUresult cuStreamWriteValue32_ptsz(CUstream stream, CUdeviceptr addr,
                                   cuuint32_t value, unsigned int flags) {
  return _cuStreamWriteValue32_ptsz(stream, addr, value, flags);
}

CUresult _cuStreamBatchMemOp(CUstream stream, unsigned int count,
                            CUstreamBatchMemOpParams *paramArray,
                            unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBatchMemOp_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBatchMemOp_v2),
                                    stream, count, paramArray, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBatchMemOp)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBatchMemOp),
                                stream, count, paramArray, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamBatchMemOp_v2(CUstream stream, unsigned int count,
                               CUstreamBatchMemOpParams *paramArray,
                               unsigned int flags){
  return _cuStreamBatchMemOp(stream, count, paramArray, flags);
}

CUresult cuStreamBatchMemOp(CUstream stream, unsigned int count,
                            CUstreamBatchMemOpParams *paramArray,
                            unsigned int flags) {
  return _cuStreamBatchMemOp(stream, count, paramArray, flags);
}

CUresult _cuStreamBatchMemOp_ptsz(CUstream stream, unsigned int count,
                                 CUstreamBatchMemOpParams *paramArray,
                                 unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamBatchMemOp_v2_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamBatchMemOp_v2_ptsz,
                                    stream, count, paramArray, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamBatchMemOp_ptsz))){
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamBatchMemOp_ptsz,
                                stream, count, paramArray, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamBatchMemOp_v2_ptsz(CUstream stream, unsigned int count,
                                 CUstreamBatchMemOpParams *paramArray,
                                 unsigned int flags) {
  return _cuStreamBatchMemOp_ptsz(stream, count, paramArray, flags);
}

CUresult cuStreamBatchMemOp_ptsz(CUstream stream, unsigned int count,
                                 CUstreamBatchMemOpParams *paramArray,
                                 unsigned int flags) {
  return _cuStreamBatchMemOp_ptsz(stream, count, paramArray, flags);
}

CUresult cuStreamCreate(CUstream *phStream, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamCreate, phStream, Flags);
}

CUresult cuStreamCreateWithPriority(CUstream *phStream, unsigned int flags,
                                    int priority) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamCreateWithPriority,
                         phStream, flags, priority);
}

CUresult cuStreamGetPriority(CUstream hStream, int *priority) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetPriority), hStream,
                         priority);
}

CUresult cuStreamGetPriority_ptsz(CUstream hStream, int *priority) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetPriority_ptsz, hStream,
                         priority);
}

CUresult cuStreamGetFlags(CUstream hStream, unsigned int *flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetFlags), hStream, flags);
}

CUresult cuStreamGetFlags_ptsz(CUstream hStream, unsigned int *flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetFlags_ptsz, hStream,
                         flags);
}

CUresult _cuStreamDestroy(CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamDestroy_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry,  cuStreamDestroy_v2, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamDestroy))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamDestroy, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamDestroy_v2(CUstream hStream) {
  return _cuStreamDestroy(hStream);
}

CUresult cuStreamDestroy(CUstream hStream) {
  return _cuStreamDestroy(hStream);
}

CUresult cuStreamWaitEvent(CUstream hStream, CUevent hEvent, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitEvent),
                         hStream, hEvent, Flags);
}

CUresult cuStreamWaitEvent_ptsz(CUstream hStream, CUevent hEvent, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWaitEvent_ptsz, hStream,
                         hEvent, Flags);
}

CUresult cuStreamAddCallback(CUstream hStream, CUstreamCallback callback,
                             void *userData, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamAddCallback), hStream,
                         callback, userData, flags);
}

CUresult cuStreamAddCallback_ptsz(CUstream hStream, CUstreamCallback callback,
                                  void *userData, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamAddCallback_ptsz, hStream,
                         callback, userData, flags);
}

CUresult cuStreamSynchronize(CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamSynchronize), hStream);
}

CUresult cuStreamSynchronize_ptsz(CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamSynchronize_ptsz, hStream);
}

CUresult cuStreamQuery(CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamQuery), hStream);
}

CUresult cuStreamQuery_ptsz(CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamQuery_ptsz, hStream);
}

CUresult cuStreamAttachMemAsync(CUstream hStream, CUdeviceptr dptr,
                                size_t length, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamAttachMemAsync), hStream,
                         dptr, length, flags);
}

CUresult cuStreamAttachMemAsync_ptsz(CUstream hStream, CUdeviceptr dptr,
                                     size_t length, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamAttachMemAsync_ptsz,
                         hStream, dptr, length, flags);
}

CUresult cuDeviceCanAccessPeer(int *canAccessPeer, CUdevice dev,
                               CUdevice peerDev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceCanAccessPeer,
                         canAccessPeer, dev, peerDev);
}

//CUresult cuCtxEnablePeerAccess(CUcontext peerContext, unsigned int Flags) {
//  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxEnablePeerAccess, peerContext,
//                         Flags);
//}

//CUresult cuCtxDisablePeerAccess(CUcontext peerContext) {
//  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxDisablePeerAccess,
//                         peerContext);
//}

CUresult cuIpcGetEventHandle(CUipcEventHandle *pHandle, CUevent event) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuIpcGetEventHandle, pHandle,
                         event);
}

CUresult cuIpcOpenEventHandle(CUevent *phEvent, CUipcEventHandle handle) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuIpcOpenEventHandle, phEvent,
                         handle);
}

CUresult cuIpcGetMemHandle(CUipcMemHandle *pHandle, CUdeviceptr dptr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuIpcGetMemHandle, pHandle, dptr);
}

CUresult _cuIpcOpenMemHandle(CUdeviceptr *pdptr, CUipcMemHandle handle,
                            unsigned int Flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuIpcOpenMemHandle_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuIpcOpenMemHandle_v2, pdptr, handle, Flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuIpcOpenMemHandle))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuIpcOpenMemHandle, pdptr, handle, Flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuIpcOpenMemHandle_v2(CUdeviceptr *pdptr, CUipcMemHandle handle,
                               unsigned int Flags) {
  return _cuIpcOpenMemHandle(pdptr, handle, Flags);
}


CUresult cuIpcOpenMemHandle(CUdeviceptr *pdptr, CUipcMemHandle handle,
                            unsigned int Flags) {
  return _cuIpcOpenMemHandle(pdptr, handle, Flags);
}

CUresult cuIpcCloseMemHandle(CUdeviceptr dptr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuIpcCloseMemHandle, dptr);
}

CUresult _cuGLCtxCreate(CUcontext *pCtx, unsigned int Flags, CUdevice device) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGLCtxCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGLCtxCreate_v2, pCtx, Flags, device);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGLCtxCreate))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGLCtxCreate, pCtx, Flags, device);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGLCtxCreate_v2(CUcontext *pCtx, unsigned int Flags, CUdevice device) {
  return _cuGLCtxCreate(pCtx, Flags, device);
}

CUresult cuGLCtxCreate(CUcontext *pCtx, unsigned int Flags, CUdevice device) {
  return _cuGLCtxCreate(pCtx, Flags, device);
}

CUresult cuGLInit(void) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLInit);
}

CUresult _cuGLGetDevices(unsigned int *pCudaDeviceCount, CUdevice *pCudaDevices,
                         unsigned int cudaDeviceCount, CUGLDeviceList deviceList) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGLGetDevices_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry,  cuGLGetDevices_v2, pCudaDeviceCount,
                                     pCudaDevices, cudaDeviceCount, deviceList);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGLGetDevices))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGLGetDevices, pCudaDeviceCount,
                                     pCudaDevices, cudaDeviceCount, deviceList);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGLGetDevices_v2(unsigned int *pCudaDeviceCount, CUdevice *pCudaDevices,
                           unsigned int cudaDeviceCount, CUGLDeviceList deviceList) {
  return _cuGLGetDevices(pCudaDeviceCount, pCudaDevices, cudaDeviceCount,
                         deviceList);
}

CUresult cuGLGetDevices(unsigned int *pCudaDeviceCount, CUdevice *pCudaDevices,
                        unsigned int cudaDeviceCount, CUGLDeviceList deviceList) {
  return _cuGLGetDevices(pCudaDeviceCount, pCudaDevices, cudaDeviceCount,
                         deviceList);
}

CUresult cuGLRegisterBufferObject(GLuint buffer) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLRegisterBufferObject, buffer);
}

CUresult cuGLMapBufferObject_v2_ptds(CUdeviceptr *dptr, size_t *size,
                                     GLuint buffer) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLMapBufferObject_v2_ptds, dptr,
                         size, buffer);
}

CUresult _cuGLMapBufferObject(CUdeviceptr *dptr, size_t *size, GLuint buffer) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuGLMapBufferObject_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuGLMapBufferObject_v2),
                           dptr, size, buffer);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGLMapBufferObject))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGLMapBufferObject,
                           dptr, size, buffer);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGLMapBufferObject_v2(CUdeviceptr *dptr, size_t *size, GLuint buffer) {
  return _cuGLMapBufferObject(dptr, size, buffer);
}

CUresult cuGLMapBufferObject(CUdeviceptr *dptr, size_t *size, GLuint buffer) {
  return _cuGLMapBufferObject(dptr, size, buffer);
}

CUresult cuGLMapBufferObjectAsync_v2_ptsz(CUdeviceptr *dptr, size_t *size,
                                          GLuint buffer, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLMapBufferObjectAsync_v2_ptsz,
                         dptr, size, buffer, hStream);
}

CUresult _cuGLMapBufferObjectAsync(CUdeviceptr *dptr, size_t *size,
                                   GLuint buffer, CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuGLMapBufferObjectAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuGLMapBufferObjectAsync_v2), dptr,
                                               size, buffer, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGLMapBufferObjectAsync))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGLMapBufferObjectAsync, dptr, size, buffer, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGLMapBufferObjectAsync_v2(CUdeviceptr *dptr, size_t *size,
                                     GLuint buffer, CUstream hStream) {
  return _cuGLMapBufferObjectAsync(dptr, size, buffer, hStream);
}

CUresult cuGLMapBufferObjectAsync(CUdeviceptr *dptr, size_t *size,
                                  GLuint buffer, CUstream hStream) {
  return _cuGLMapBufferObjectAsync(dptr, size, buffer, hStream);
}

CUresult cuGLUnmapBufferObject(GLuint buffer) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLUnmapBufferObject, buffer);
}

CUresult cuGLUnmapBufferObjectAsync(GLuint buffer, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLUnmapBufferObjectAsync, buffer,
                         hStream);
}

CUresult cuGLUnregisterBufferObject(GLuint buffer) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLUnregisterBufferObject,
                         buffer);
}

CUresult cuGLSetBufferObjectMapFlags(GLuint buffer, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGLSetBufferObjectMapFlags,
                         buffer, Flags);
}

CUresult cuGraphicsGLRegisterImage(CUgraphicsResource *pCudaResource,
                                   GLuint image, GLenum target,
                                   unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsGLRegisterImage,
                         pCudaResource, image, target, Flags);
}

CUresult cuGraphicsGLRegisterBuffer(CUgraphicsResource *pCudaResource,
                                    GLuint buffer, unsigned int Flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsGLRegisterBuffer,
                         pCudaResource, buffer, Flags);
}

CUresult cuGraphicsUnregisterResource(CUgraphicsResource resource) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsUnregisterResource,
                         resource);
}

CUresult cuGraphicsMapResources_ptsz(unsigned int count,
                                     CUgraphicsResource *resources,
                                     CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsMapResources_ptsz, count,
                         resources, hStream);
}

CUresult cuGraphicsMapResources(unsigned int count,
                                CUgraphicsResource *resources,
                                CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuGraphicsMapResources), count,
                         resources, hStream);
}

CUresult cuGraphicsUnmapResources_ptsz(unsigned int count,
                                       CUgraphicsResource *resources,
                                       CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsUnmapResources_ptsz,
                         count, resources, hStream);
}

CUresult cuGraphicsUnmapResources(unsigned int count,
                                  CUgraphicsResource *resources,
                                  CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuGraphicsUnmapResources), count,
                         resources, hStream);
}

CUresult _cuGraphicsResourceSetMapFlags(CUgraphicsResource resource, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphicsResourceSetMapFlags_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsResourceSetMapFlags_v2, resource, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphicsResourceSetMapFlags))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsResourceSetMapFlags, resource, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGraphicsResourceSetMapFlags_v2(CUgraphicsResource resource, unsigned int flags) {
  return _cuGraphicsResourceSetMapFlags(resource, flags);
}

CUresult cuGraphicsResourceSetMapFlags(CUgraphicsResource resource, unsigned int flags) {
  return _cuGraphicsResourceSetMapFlags(resource, flags);
}

CUresult cuGraphicsSubResourceGetMappedArray(CUarray *pArray, CUgraphicsResource resource,
                                             unsigned int arrayIndex, unsigned int mipLevel) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsSubResourceGetMappedArray,
                         pArray, resource, arrayIndex, mipLevel);
}

CUresult cuGraphicsResourceGetMappedMipmappedArray(CUmipmappedArray *pMipmappedArray,
                                                   CUgraphicsResource resource) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsResourceGetMappedMipmappedArray,
                         pMipmappedArray, resource);
}

CUresult _cuGraphicsResourceGetMappedPointer(CUdeviceptr *pDevPtr, size_t *pSize,
                                            CUgraphicsResource resource) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphicsResourceGetMappedPointer_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsResourceGetMappedPointer_v2,
                                                         pDevPtr, pSize, resource);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphicsResourceGetMappedPointer))){
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsResourceGetMappedPointer,
                                                         pDevPtr, pSize, resource);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGraphicsResourceGetMappedPointer_v2(CUdeviceptr *pDevPtr, size_t *pSize,
                                               CUgraphicsResource resource) {
  return _cuGraphicsResourceGetMappedPointer(pDevPtr, pSize, resource);
}

CUresult cuGraphicsResourceGetMappedPointer(CUdeviceptr *pDevPtr, size_t *pSize,
                                            CUgraphicsResource resource) {
  return _cuGraphicsResourceGetMappedPointer(pDevPtr, pSize, resource);
}

CUresult cuProfilerInitialize(const char *configFile, const char *outputFile,
                              CUoutput_mode outputMode) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuProfilerInitialize, configFile,
                         outputFile, outputMode);
}

CUresult cuProfilerStart(void) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuProfilerStart);
}

CUresult cuProfilerStop(void) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuProfilerStop);
}

CUresult cuVDPAUGetDevice(CUdevice *pDevice, VdpDevice vdpDevice,
                          VdpGetProcAddress *vdpGetProcAddress) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuVDPAUGetDevice, pDevice,
                         vdpDevice, vdpGetProcAddress);
}

CUresult _cuVDPAUCtxCreate(CUcontext *pCtx, unsigned int flags,
                          CUdevice device, VdpDevice vdpDevice,
                          VdpGetProcAddress *vdpGetProcAddress) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuVDPAUCtxCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuVDPAUCtxCreate_v2, pCtx, flags,
                                           device, vdpDevice, vdpGetProcAddress);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuVDPAUCtxCreate))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuVDPAUCtxCreate, pCtx, flags,
                                        device, vdpDevice, vdpGetProcAddress);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuVDPAUCtxCreate_v2(CUcontext *pCtx, unsigned int flags,
                             CUdevice device, VdpDevice vdpDevice,
                             VdpGetProcAddress *vdpGetProcAddress) {
  return _cuVDPAUCtxCreate(pCtx, flags, device, vdpDevice, vdpGetProcAddress);
}

CUresult cuVDPAUCtxCreate(CUcontext *pCtx, unsigned int flags,
                          CUdevice device, VdpDevice vdpDevice,
                          VdpGetProcAddress *vdpGetProcAddress) {
  return _cuVDPAUCtxCreate(pCtx, flags, device, vdpDevice, vdpGetProcAddress);
}

CUresult cuGraphicsVDPAURegisterVideoSurface(CUgraphicsResource *pCudaResource,
                                             VdpVideoSurface vdpSurface, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsVDPAURegisterVideoSurface,
                         pCudaResource, vdpSurface, flags);
}

CUresult cuGraphicsVDPAURegisterOutputSurface(CUgraphicsResource *pCudaResource,
                                              VdpOutputSurface vdpSurface, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsVDPAURegisterOutputSurface,
                         pCudaResource, vdpSurface, flags);
}

//CUresult cuGetExportTable(const void **ppExportTable, const CUuuid *pExportTableId) {
//  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGetExportTable, ppExportTable, pExportTableId);
//}

CUresult cuOccupancyMaxActiveBlocksPerMultiprocessor(int *numBlocks, CUfunction func,
                                                     int blockSize, size_t dynamicSMemSize) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuOccupancyMaxActiveBlocksPerMultiprocessor,
                         numBlocks, func, blockSize, dynamicSMemSize);
}

CUresult cuMemAdvise(CUdeviceptr devPtr, size_t count, CUmem_advise advice, CUdevice device) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAdvise, devPtr, count, advice, device);
}

CUresult cuMemAdvise_v2(CUdeviceptr devPtr, size_t count, CUmem_advise advice, CUmemLocation location){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAdvise_v2, devPtr, count, advice, location);
}

CUresult cuMemPrefetchAsync_ptsz(CUdeviceptr devPtr, size_t count,
                                 CUdevice dstDevice, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPrefetchAsync_ptsz, devPtr,
                         count, dstDevice, hStream);
}

CUresult cuMemPrefetchAsync(CUdeviceptr devPtr, size_t count,
                            CUdevice dstDevice, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemPrefetchAsync), devPtr, count,
                         dstDevice, hStream);
}

CUresult cuMemPrefetchAsync_v2_ptsz(CUdeviceptr devPtr, size_t count, 
                            CUmemLocation location, unsigned int flags, CUstream hStream){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPrefetchAsync_v2_ptsz, devPtr, count,
                         location, flags, hStream);
}

CUresult cuMemPrefetchAsync_v2(CUdeviceptr devPtr, size_t count, 
                            CUmemLocation location, unsigned int flags, CUstream hStream){
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemPrefetchAsync_v2), devPtr, count,
                         location, flags, hStream);
}

CUresult cuMemRangeGetAttribute(void *data, size_t dataSize,
                                CUmem_range_attribute attribute,
                                CUdeviceptr devPtr, size_t count) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemRangeGetAttribute, data,
                         dataSize, attribute, devPtr, count);
}

CUresult cuMemRangeGetAttributes(void **data, size_t *dataSizes,
                                 CUmem_range_attribute *attributes,
                                 size_t numAttributes, CUdeviceptr devPtr,
                                 size_t count) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemRangeGetAttributes, data,
                         dataSizes, attributes, numAttributes, devPtr, count);
}

CUresult cuCtxSetLimit(CUlimit limit, size_t value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxSetLimit, limit, value);
}

CUresult cuModuleLoad(CUmodule *module, const char *fname) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleLoad, module, fname);
}

CUresult cuModuleLoadData(CUmodule *module, const void *image) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleLoadData, module, image);
}

CUresult cuModuleLoadFatBinary(CUmodule *module, const void *fatCubin) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleLoadFatBinary, module,
                         fatCubin);
}

CUresult cuGetErrorString(CUresult error, const char **pStr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGetErrorString, error, pStr);
}

CUresult cuGetErrorName(CUresult error, const char **pStr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGetErrorName, error, pStr);
}

CUresult cuCtxAttach(CUcontext *pctx, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxAttach, pctx, flags);
}

CUresult _cuCtxDestroy(CUcontext ctx) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxDestroy_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxDestroy_v2, ctx);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxDestroy))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxDestroy, ctx);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuCtxDestroy_v2(CUcontext ctx) {
  return _cuCtxDestroy(ctx);
}

CUresult cuCtxDestroy(CUcontext ctx) {
  return _cuCtxDestroy(ctx);
}

CUresult _cuCtxPopCurrent(CUcontext *pctx) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxPopCurrent_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxPopCurrent_v2, pctx);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxPopCurrent))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxPopCurrent, pctx);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuCtxPopCurrent_v2(CUcontext *pctx) {
  return _cuCtxPopCurrent(pctx);
}

CUresult cuCtxPopCurrent(CUcontext *pctx) {
  return _cuCtxPopCurrent(pctx);
}

CUresult _cuCtxPushCurrent(CUcontext ctx) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxPushCurrent_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxPushCurrent_v2, ctx);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuCtxPushCurrent))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxPushCurrent, ctx);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuCtxPushCurrent_v2(CUcontext ctx) {
  return _cuCtxPushCurrent(ctx);
}

CUresult cuCtxPushCurrent(CUcontext ctx) {
  return _cuCtxPushCurrent(ctx);
}

void cudbgApiInit(uint32_t arg) {
  return CUDA_ENTRY_DEBUG_VOID_CALL(cuda_library_entry, cudbgApiInit, arg);
}

void cudbgApiAttach(void) {
  return CUDA_ENTRY_DEBUG_VOID_CALL(cuda_library_entry, cudbgApiAttach);
}

void cudbgApiDetach(void) {
  return CUDA_ENTRY_DEBUG_VOID_CALL(cuda_library_entry, cudbgApiDetach);
}

CUDBGResult cudbgGetAPI(uint32_t major, uint32_t minor, uint32_t rev, CUDBGAPI *api) {
  return CUDA_ENTRY_DEBUG_RESULT_CALL(cuda_library_entry, cudbgGetAPI, major,
                                      minor, rev, api);
}

CUDBGResult cudbgGetAPIVersion(uint32_t *major, uint32_t *minor, uint32_t *rev) {
  return CUDA_ENTRY_DEBUG_RESULT_CALL(cuda_library_entry, cudbgGetAPIVersion,
                                      major, minor, rev);
}

CUDBGResult cudbgMain(int apiClientPid, uint32_t apiClientRevision,
                      int sessionId, int attachState,
                      int attachEventInitialized, int writeFd, int detachFd,
                      int attachStubInUse, int enablePreemptionDebugging) {
  return CUDA_ENTRY_DEBUG_RESULT_CALL(
      cuda_library_entry, cudbgMain, apiClientPid, apiClientRevision, sessionId,
      attachState, attachEventInitialized, writeFd, detachFd, attachStubInUse,
      enablePreemptionDebugging);
}

void cudbgReportDriverApiError(void) {
  return CUDA_ENTRY_DEBUG_VOID_CALL(cuda_library_entry,
                                    cudbgReportDriverApiError);
}

void cudbgReportDriverInternalError(void) {
  return CUDA_ENTRY_DEBUG_VOID_CALL(cuda_library_entry,
                                    cudbgReportDriverInternalError);
}

CUresult cuDeviceComputeCapability(int *major, int *minor, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceComputeCapability, major,
                         minor, dev);
}

CUresult cuDeviceGetProperties(CUdevprop *prop, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetProperties, prop, dev);
}

CUresult cuEGLInit(void) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLInit);
}

CUresult cuEGLApiInit(void) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLApiInit);
}

CUresult cuEGLStreamConsumerAcquireFrame(CUeglStreamConnection *conn,
                                         CUgraphicsResource *pCudaResource,
                                         CUstream *pStream,
                                         unsigned int timeout) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamConsumerAcquireFrame,
                         conn, pCudaResource, pStream, timeout);
}

CUresult cuEGLStreamConsumerConnect(CUeglStreamConnection *conn,
                                    EGLStreamKHR stream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamConsumerConnect, conn,
                         stream);
}

CUresult cuEGLStreamConsumerConnectWithFlags(CUeglStreamConnection *conn,
                                             EGLStreamKHR stream,
                                             unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamConsumerConnectWithFlags,
                         conn, stream, flags);
}

CUresult cuEGLStreamConsumerDisconnect(CUeglStreamConnection *conn) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamConsumerDisconnect,
                         conn);
}

CUresult cuEGLStreamConsumerReleaseFrame(CUeglStreamConnection *conn,
                                         CUgraphicsResource pCudaResource,
                                         CUstream *pStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamConsumerReleaseFrame,
                         conn, pCudaResource, pStream);
}

CUresult cuEGLStreamProducerConnect(CUeglStreamConnection *conn,
                                    EGLStreamKHR stream, EGLint width,
                                    EGLint height) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamProducerConnect, conn,
                         stream, width, height);
}

CUresult cuEGLStreamProducerDisconnect(CUeglStreamConnection *conn) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamProducerDisconnect,
                         conn);
}

CUresult cuEGLStreamProducerPresentFrame(CUeglStreamConnection *conn,
                                         CUeglFrame eglframe,
                                         CUstream *pStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamProducerPresentFrame,
                         conn, eglframe, pStream);
}

CUresult cuEGLStreamProducerReturnFrame(CUeglStreamConnection *conn,
                                        CUeglFrame *eglframe,
                                        CUstream *pStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEGLStreamProducerReturnFrame,
                         conn, eglframe, pStream);
}

CUresult cuFuncSetAttribute(CUfunction hfunc, CUfunction_attribute attrib,
                            int value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncSetAttribute, hfunc, attrib,
                         value);
}

CUresult cuFuncSetSharedSize(CUfunction hfunc, unsigned int bytes) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncSetSharedSize, hfunc, bytes);
}

CUresult cuGraphicsEGLRegisterImage(CUgraphicsResource *pCudaResource,
                                    EGLImageKHR image, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsEGLRegisterImage,
                         pCudaResource, image, flags);
}

CUresult cuGraphicsResourceGetMappedEglFrame(CUeglFrame *eglFrame,
                                             CUgraphicsResource resource,
                                             unsigned int index,
                                             unsigned int mipLevel) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphicsEGLRegisterImage,
                         cuGraphicsResourceGetMappedEglFrame, eglFrame,
                         resource, index, mipLevel);
}


CUresult cuLaunchCooperativeKernelMultiDevice(CUDA_LAUNCH_PARAMS *launchParamsList,
                                      unsigned int numDevices,  unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuLaunchCooperativeKernelMultiDevice, launchParamsList,
                         numDevices, flags);
}

CUresult _cuMemAllocHost(void **pp, size_t bytesize) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocHost_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocHost_v2, pp, bytesize);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocHost))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocHost, pp, bytesize);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemAllocHost_v2(void **pp, size_t bytesize) {
  return _cuMemAllocHost(pp, bytesize);
}

CUresult cuMemAllocHost(void **pp, size_t bytesize) {
  return _cuMemAllocHost(pp, bytesize);
}

CUresult _cuMemcpy2D(const CUDA_MEMCPY2D *pCopy) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy2D_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpy2D_v2), pCopy);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpy2D))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpy2D, pCopy);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpy2D_v2(const CUDA_MEMCPY2D *pCopy) {
  return _cuMemcpy2D(pCopy);
}

CUresult cuMemcpy2D(const CUDA_MEMCPY2D *pCopy) {
  return _cuMemcpy2D(pCopy);
}

CUresult cuMemcpyAtoA_v2_ptds(CUarray dstArray, size_t dstOffset,
                              CUarray srcArray, size_t srcOffset,
                              size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAtoA_v2_ptds, dstArray,
                         dstOffset, srcArray, srcOffset, ByteCount);
}

CUresult _cuMemcpyAtoA(CUarray dstArray, size_t dstOffset, CUarray srcArray,
                       size_t srcOffset, size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyAtoA_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyAtoA_v2), dstArray,
                               dstOffset, srcArray, srcOffset, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyAtoA))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAtoA, dstArray,
                               dstOffset, srcArray, srcOffset, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemcpyAtoA_v2(CUarray dstArray, size_t dstOffset, CUarray srcArray,
                         size_t srcOffset, size_t ByteCount) {
  return _cuMemcpyAtoA(dstArray, dstOffset, srcArray, srcOffset, ByteCount);
}

CUresult cuMemcpyAtoA(CUarray dstArray, size_t dstOffset, CUarray srcArray,
                      size_t srcOffset, size_t ByteCount) {
  return _cuMemcpyAtoA(dstArray, dstOffset, srcArray, srcOffset, ByteCount);
}

CUresult _cuMemcpyAtoD(CUdeviceptr dstDevice, CUarray srcArray,
                      size_t srcOffset, size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyAtoD_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyAtoD_v2), dstDevice, srcArray,
                                                   srcOffset, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyAtoD))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAtoD, dstDevice, srcArray,
                                                   srcOffset, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpyAtoD_v2(CUdeviceptr dstDevice, CUarray srcArray,
                         size_t srcOffset, size_t ByteCount) {
  return _cuMemcpyAtoD(dstDevice, srcArray, srcOffset, ByteCount);
}

CUresult cuMemcpyAtoD(CUdeviceptr dstDevice, CUarray srcArray,
                      size_t srcOffset, size_t ByteCount) {
  return _cuMemcpyAtoD(dstDevice, srcArray, srcOffset, ByteCount);
}


CUresult cuMemcpyAtoD_v2_ptds(CUdeviceptr dstDevice, CUarray srcArray,
                              size_t srcOffset, size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAtoD_v2_ptds, dstDevice,
                         srcArray, srcOffset, ByteCount);
}

CUresult cuMemcpyAtoH_v2_ptds(void *dstHost, CUarray srcArray, size_t srcOffset,
                              size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAtoH_v2_ptds, dstHost,
                         srcArray, srcOffset, ByteCount);
}

CUresult _cuMemcpyAtoH(void *dstHost, CUarray srcArray, size_t srcOffset,
                      size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyAtoH_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyAtoH_v2), dstHost, srcArray,
                                                    srcOffset, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyAtoH))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAtoH, dstHost, srcArray,
                                                    srcOffset, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpyAtoH_v2(void *dstHost, CUarray srcArray, size_t srcOffset,
                         size_t ByteCount) {
  return _cuMemcpyAtoH(dstHost, srcArray, srcOffset, ByteCount);
}

CUresult cuMemcpyAtoH(void *dstHost, CUarray srcArray, size_t srcOffset,
                      size_t ByteCount) {
  return _cuMemcpyAtoH(dstHost, srcArray, srcOffset, ByteCount);
}

CUresult cuMemcpyAtoHAsync_v2_ptsz(void *dstHost, CUarray srcArray,
                                   size_t srcOffset, size_t ByteCount,
                                   CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyAtoHAsync_v2_ptsz, dstHost,
                         srcArray, srcOffset, ByteCount, hStream);
}

CUresult _cuMemcpyAtoHAsync(void *dstHost, CUarray srcArray, size_t srcOffset,
                           size_t ByteCount, CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyAtoHAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyAtoHAsync_v2), dstHost,
                                     srcArray, srcOffset, ByteCount, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyAtoHAsync))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry,  cuMemcpyAtoHAsync, dstHost,
                                   srcArray, srcOffset, ByteCount, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpyAtoHAsync_v2(void *dstHost, CUarray srcArray, size_t srcOffset,
                              size_t ByteCount, CUstream hStream) {
  return _cuMemcpyAtoHAsync(dstHost, srcArray, srcOffset, ByteCount, hStream);
}

CUresult cuMemcpyAtoHAsync(void *dstHost, CUarray srcArray, size_t srcOffset,
                           size_t ByteCount, CUstream hStream) {
  return _cuMemcpyAtoHAsync(dstHost, srcArray, srcOffset, ByteCount, hStream);
}

CUresult cuMemcpyDtoA_v2_ptds(CUarray dstArray, size_t dstOffset,
                              CUdeviceptr srcDevice, size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoA_v2_ptds, dstArray,
                         dstOffset, srcDevice, ByteCount);
}

CUresult _cuMemcpyDtoA(CUarray dstArray, size_t dstOffset, CUdeviceptr srcDevice,
                      size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyDtoA_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyDtoA_v2), dstArray,
                                               dstOffset, srcDevice, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyDtoA))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyDtoA, dstArray, dstOffset,
                                                           srcDevice, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemcpyDtoA_v2(CUarray dstArray, size_t dstOffset,
                         CUdeviceptr srcDevice, size_t ByteCount) {
  return _cuMemcpyDtoA(dstArray, dstOffset, srcDevice, ByteCount);
}

CUresult cuMemcpyDtoA(CUarray dstArray, size_t dstOffset, CUdeviceptr srcDevice,
                      size_t ByteCount) {
  return _cuMemcpyDtoA(dstArray, dstOffset, srcDevice, ByteCount);
}

CUresult cuMemcpyHtoA_v2_ptds(CUarray dstArray, size_t dstOffset,
                              const void *srcHost, size_t ByteCount) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoA_v2_ptds, dstArray,
                         dstOffset, srcHost, ByteCount);
}

CUresult _cuMemcpyHtoA(CUarray dstArray, size_t dstOffset, const void *srcHost,
                      size_t ByteCount) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyHtoA_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemcpyHtoA_v2), dstArray,
                                            dstOffset, srcHost, ByteCount);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyHtoA))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoA, dstArray, dstOffset,
                                                       srcHost, ByteCount);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemcpyHtoA_v2(CUarray dstArray, size_t dstOffset,
                         const void *srcHost, size_t ByteCount) {
  return _cuMemcpyHtoA(dstArray, dstOffset, srcHost, ByteCount);
}

CUresult cuMemcpyHtoA(CUarray dstArray, size_t dstOffset, const void *srcHost,
                      size_t ByteCount) {
  return _cuMemcpyHtoA(dstArray, dstOffset, srcHost, ByteCount);
}

CUresult cuMemcpyHtoAAsync_v2_ptsz(CUarray dstArray, size_t dstOffset,
                                   const void *srcHost, size_t ByteCount,
                                   CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoAAsync_v2_ptsz,
                         dstArray, dstOffset, srcHost, ByteCount, hStream);
}

CUresult _cuMemcpyHtoAAsync(CUarray dstArray, size_t dstOffset,
                           const void *srcHost, size_t ByteCount,
                           CUstream hStream) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyHtoAAsync_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemcpyHtoAAsync_v2), dstArray,
                                       dstOffset, srcHost, ByteCount, hStream);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemcpyHtoAAsync))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemcpyHtoAAsync, dstArray,
                                    dstOffset, srcHost, ByteCount, hStream);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuMemcpyHtoAAsync_v2(CUarray dstArray, size_t dstOffset,
                              const void *srcHost, size_t ByteCount,
                              CUstream hStream) {
  return _cuMemcpyHtoAAsync(dstArray, dstOffset, srcHost, ByteCount, hStream);
}

CUresult cuMemcpyHtoAAsync(CUarray dstArray, size_t dstOffset,
                           const void *srcHost, size_t ByteCount,
                           CUstream hStream) {
  return _cuMemcpyHtoAAsync(dstArray, dstOffset, srcHost, ByteCount, hStream);
}

CUresult cuMemsetD16_v2_ptds(CUdeviceptr dstDevice, unsigned short us,
                             size_t N) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD16_v2_ptds, dstDevice, us,
                         N);
}

CUresult _cuMemsetD16(CUdeviceptr dstDevice, unsigned short us, size_t N) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD16_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD16_v2), dstDevice, us, N);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemsetD16))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD16, dstDevice, us, N);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemsetD16_v2(CUdeviceptr dstDevice, unsigned short us, size_t N) {
  return _cuMemsetD16(dstDevice, us, N);
}

CUresult cuMemsetD16(CUdeviceptr dstDevice, unsigned short us, size_t N) {
  return _cuMemsetD16(dstDevice, us, N);
}

CUresult cuMemsetD16Async_ptsz(CUdeviceptr dstDevice, unsigned short us,
                               size_t N, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD16Async_ptsz, dstDevice,
                         us, N, hStream);
}

CUresult cuMemsetD16Async(CUdeviceptr dstDevice, unsigned short us, size_t N,
                          CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemsetD16Async),
                         dstDevice, us, N, hStream);
}

CUresult cuMemsetD2D16_v2_ptds(CUdeviceptr dstDevice, size_t dstPitch,
                               unsigned short us, size_t Width, size_t Height) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D16_v2_ptds, dstDevice,
                         dstPitch, us, Width, Height);
}

CUresult _cuMemsetD2D16(CUdeviceptr dstDevice, size_t dstPitch,
                       unsigned short us, size_t Width, size_t Height) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD2D16_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD2D16_v2), dstDevice,
                                             dstPitch, us, Width, Height);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemsetD2D16))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D16, dstDevice,
                                             dstPitch, us, Width, Height);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemsetD2D16_v2(CUdeviceptr dstDevice, size_t dstPitch,
                          unsigned short us, size_t Width, size_t Height) {
  return _cuMemsetD2D16(dstDevice, dstPitch, us, Width, Height);
}

CUresult cuMemsetD2D16(CUdeviceptr dstDevice, size_t dstPitch,
                       unsigned short us, size_t Width, size_t Height) {
  return _cuMemsetD2D16(dstDevice, dstPitch, us, Width, Height);
}

CUresult cuMemsetD2D16Async_ptsz(CUdeviceptr dstDevice, size_t dstPitch,
                                 unsigned short us, size_t Width, size_t Height,
                                 CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D16Async_ptsz, dstDevice,
                         dstPitch, us, Width, Height, hStream);
}

CUresult cuMemsetD2D16Async(CUdeviceptr dstDevice, size_t dstPitch,
                            unsigned short us, size_t Width, size_t Height,
                            CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemsetD2D16Async), dstDevice,
                         dstPitch, us, Width, Height, hStream);
}

CUresult cuMemsetD2D32_v2_ptds(CUdeviceptr dstDevice, size_t dstPitch,
                               unsigned int ui, size_t Width, size_t Height) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D32_v2_ptds, dstDevice,
                         dstPitch, ui, Width, Height);
}

CUresult _cuMemsetD2D32(CUdeviceptr dstDevice, size_t dstPitch,
                       unsigned int ui, size_t Width, size_t Height) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD2D32_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD2D32_v2), dstDevice,
                                               dstPitch, ui, Width, Height);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemsetD2D32))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D32, dstDevice,
                                               dstPitch, ui, Width, Height);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemsetD2D32_v2(CUdeviceptr dstDevice, size_t dstPitch,
                          unsigned int ui, size_t Width, size_t Height) {
  return _cuMemsetD2D32(dstDevice, dstPitch, ui, Width, Height);
}

CUresult cuMemsetD2D32(CUdeviceptr dstDevice, size_t dstPitch,
                       unsigned int ui, size_t Width, size_t Height) {
  return _cuMemsetD2D32(dstDevice, dstPitch, ui, Width, Height);
}

CUresult cuMemsetD2D32Async_ptsz(CUdeviceptr dstDevice, size_t dstPitch,
                                 unsigned int ui, size_t Width, size_t Height,
                                 CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD2D32Async_ptsz, dstDevice,
                         dstPitch, ui, Width, Height, hStream);
}

CUresult cuMemsetD2D32Async(CUdeviceptr dstDevice, size_t dstPitch,
                            unsigned int ui, size_t Width, size_t Height,
                            CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemsetD2D32Async), dstDevice,
                         dstPitch, ui, Width, Height, hStream);
}

CUresult cuMemsetD32_v2_ptds(CUdeviceptr dstDevice, unsigned int ui, size_t N) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD32_v2_ptds, dstDevice, ui, N);
}

CUresult _cuMemsetD32(CUdeviceptr dstDevice, unsigned int ui, size_t N) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD32_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTDS(cuMemsetD32_v2), dstDevice, ui, N);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemsetD32))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD32, dstDevice, ui, N);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuMemsetD32_v2(CUdeviceptr dstDevice, unsigned int ui, size_t N) {
  return _cuMemsetD32(dstDevice, ui, N);
}

CUresult cuMemsetD32(CUdeviceptr dstDevice, unsigned int ui, size_t N) {
  return _cuMemsetD32(dstDevice, ui, N);
}

CUresult cuMemsetD32Async_ptsz(CUdeviceptr dstDevice, unsigned int ui, size_t N,
                               CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemsetD32Async_ptsz, dstDevice,
                         ui, N, hStream);
}

CUresult cuMemsetD32Async(CUdeviceptr dstDevice, unsigned int ui, size_t N,
                          CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemsetD32Async), dstDevice, ui, N,
                         hStream);
}

CUresult cuModuleLoadDataEx(CUmodule *module, const void *image,
                            unsigned int numOptions, CUjit_option *options,
                            void **optionValues) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleLoadDataEx, module, image,
                         numOptions, options, optionValues);
}

CUresult cuOccupancyMaxActiveBlocksPerMultiprocessorWithFlags(
                            int *numBlocks, CUfunction func, int blockSize, 
                            size_t dynamicSMemSize, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuOccupancyMaxActiveBlocksPerMultiprocessorWithFlags,
                         numBlocks, func, blockSize, dynamicSMemSize, flags);
}

CUresult cuOccupancyMaxActiveClusters(int* numClusters, CUfunction func, 
                            const CUlaunchConfig* config){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuOccupancyMaxActiveClusters,
                         numClusters, func, config);
}

CUresult cuOccupancyMaxPotentialBlockSize(int *minGridSize, int *blockSize,
                                 CUfunction func,
                                 CUoccupancyB2DSize blockSizeToDynamicSMemSize,
                                 size_t dynamicSMemSize, int blockSizeLimit) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuOccupancyMaxPotentialBlockSize,
                         minGridSize, blockSize, func, blockSizeToDynamicSMemSize,
                         dynamicSMemSize, blockSizeLimit);
}

CUresult cuOccupancyMaxPotentialBlockSizeWithFlags(
                        int *minGridSize, int *blockSize, CUfunction func,
                        CUoccupancyB2DSize blockSizeToDynamicSMemSize, size_t dynamicSMemSize,
                        int blockSizeLimit, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuOccupancyMaxPotentialBlockSizeWithFlags,
                         minGridSize, blockSize, func, blockSizeToDynamicSMemSize,
                         dynamicSMemSize, blockSizeLimit, flags);
}

CUresult cuOccupancyMaxPotentialClusterSize(int* clusterSize, CUfunction func, 
                                            const CUlaunchConfig* config){
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuOccupancyMaxPotentialClusterSize, 
                         clusterSize, func, config);
}

CUresult cuParamSetf(CUfunction hfunc, int offset, float value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuParamSetf, hfunc, offset, value);
}

CUresult cuParamSeti(CUfunction hfunc, int offset, unsigned int value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuParamSeti, hfunc, offset, value);
}

CUresult cuParamSetSize(CUfunction hfunc, unsigned int numbytes) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuParamSetSize, hfunc, numbytes);
}

CUresult cuParamSetTexRef(CUfunction hfunc, int texunit, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuParamSetTexRef, hfunc, texunit,
                         hTexRef);
}

CUresult cuParamSetv(CUfunction hfunc, int offset, void *ptr,
                     unsigned int numbytes) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuParamSetv, hfunc, offset, ptr,
                         numbytes);
}

CUresult cuPointerSetAttribute(const void *value, CUpointer_attribute attribute,
                               CUdeviceptr ptr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuPointerSetAttribute, value,
                         attribute, ptr);
}

CUresult _cuStreamWaitValue64(CUstream stream, CUdeviceptr addr,
                             cuuint64_t value, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue64_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue64_v2),
                                           stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue64)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWaitValue64),
                                        stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamWaitValue64_v2(CUstream stream, CUdeviceptr addr,
                                 cuuint64_t value, unsigned int flags){
  return _cuStreamWaitValue64(stream, addr, value, flags);
}

CUresult cuStreamWaitValue64(CUstream stream, CUdeviceptr addr,
                             cuuint64_t value, unsigned int flags) {
  return _cuStreamWaitValue64(stream, addr, value, flags);
}

CUresult _cuStreamWaitValue64_ptsz(CUstream stream, CUdeviceptr addr,
                                  cuuint64_t value, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWaitValue64_v2_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWaitValue64_v2_ptsz,
                                           stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWaitValue64_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWaitValue64_ptsz,
                                           stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamWaitValue64_v2_ptsz(CUstream stream, CUdeviceptr addr,
                                  cuuint64_t value, unsigned int flags) {
  return _cuStreamWaitValue64_ptsz(stream, addr, value, flags);
}

CUresult cuStreamWaitValue64_ptsz(CUstream stream, CUdeviceptr addr,
                                  cuuint64_t value, unsigned int flags) {
  return _cuStreamWaitValue64_ptsz(stream, addr, value, flags);
}

CUresult _cuStreamWriteValue64(CUstream stream, CUdeviceptr addr,
                                cuuint64_t value, unsigned int flags){
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue64_v2)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue64_v2),
                                           stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue64)))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamWriteValue64),
                                           stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamWriteValue64_v2(CUstream stream, CUdeviceptr addr,
                                  cuuint64_t value, unsigned int flags){
  return _cuStreamWriteValue64(stream, addr, value, flags);
}

CUresult cuStreamWriteValue64(CUstream stream, CUdeviceptr addr,
                              cuuint64_t value, unsigned int flags) {
  return _cuStreamWriteValue64(stream, addr, value, flags);
}

CUresult _cuStreamWriteValue64_ptsz(CUstream stream, CUdeviceptr addr,
                                   cuuint64_t value, unsigned int flags) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWriteValue64_v2_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWriteValue64_v2_ptsz,
                                           stream, addr, value, flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamWriteValue64_ptsz))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamWriteValue64_ptsz,
                                           stream, addr, value, flags);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuStreamWriteValue64_v2_ptsz(CUstream stream, CUdeviceptr addr,
                                   cuuint64_t value, unsigned int flags) {
  return _cuStreamWriteValue64_ptsz(stream, addr, value, flags);
}

CUresult cuStreamWriteValue64_ptsz(CUstream stream, CUdeviceptr addr,
                                   cuuint64_t value, unsigned int flags) {
  return _cuStreamWriteValue64_ptsz(stream, addr, value, flags);
}

CUresult cuSurfRefGetArray(CUarray *phArray, CUsurfref hSurfRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuSurfRefGetArray, phArray,
                         hSurfRef);
}

CUresult _cuTexRefGetAddress(CUdeviceptr *pdptr, CUtexref hTexRef) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuTexRefGetAddress_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetAddress_v2, pdptr, hTexRef);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuTexRefGetAddress))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetAddress, pdptr, hTexRef);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuTexRefGetAddress_v2(CUdeviceptr *pdptr, CUtexref hTexRef) {
  return _cuTexRefGetAddress(pdptr, hTexRef);
}

CUresult cuTexRefGetAddress(CUdeviceptr *pdptr, CUtexref hTexRef) {
  return _cuTexRefGetAddress(pdptr, hTexRef);
}

CUresult cuTexRefGetAddressMode(CUaddress_mode *pam, CUtexref hTexRef,
                                int dim) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetAddressMode, pam,
                         hTexRef, dim);
}

CUresult cuTexRefGetArray(CUarray *phArray, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetArray, phArray,
                         hTexRef);
}

CUresult cuTexRefGetFilterMode(CUfilter_mode *pfm, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetFilterMode, pfm,
                         hTexRef);
}

CUresult cuTexRefGetFlags(unsigned int *pFlags, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetFlags, pFlags, hTexRef);
}

CUresult cuTexRefGetFormat(CUarray_format *pFormat, int *pNumChannels,
                           CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetFormat, pFormat,
                         pNumChannels, hTexRef);
}

CUresult cuTexRefGetMaxAnisotropy(int *pmaxAniso, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetMaxAnisotropy,
                         pmaxAniso, hTexRef);
}

CUresult cuTexRefGetMipmapFilterMode(CUfilter_mode *pfm, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetMipmapFilterMode, pfm,
                         hTexRef);
}

CUresult cuTexRefGetMipmapLevelBias(float *pbias, CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetMipmapLevelBias, pbias,
                         hTexRef);
}

CUresult cuTexRefGetMipmapLevelClamp(float *pminMipmapLevelClamp,
                                     float *pmaxMipmapLevelClamp,
                                     CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetMipmapLevelClamp,
                         pminMipmapLevelClamp, pmaxMipmapLevelClamp, hTexRef);
}

CUresult cuTexRefGetMipmappedArray(CUmipmappedArray *phMipmappedArray,
                                   CUtexref hTexRef) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuTexRefGetMipmappedArray,
                         phMipmappedArray, hTexRef);
}

CUresult cuDeviceGetAttribute(int *pi, CUdevice_attribute attrib, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetAttribute, pi, attrib, dev);
}

CUresult cuDestroyExternalMemory(CUexternalMemory extMem) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDestroyExternalMemory, extMem);
}

CUresult cuDestroyExternalSemaphore(CUexternalSemaphore extSem) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDestroyExternalSemaphore, extSem);
}

CUresult cuDeviceGetUuid_v2(CUuuid *uuid, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetUuid_v2, uuid, dev);
}

CUresult cuDeviceGetUuid(CUuuid *uuid, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetUuid, uuid, dev);
}

CUresult cuExternalMemoryGetMappedBuffer(
    CUdeviceptr *devPtr, CUexternalMemory extMem,
    const CUDA_EXTERNAL_MEMORY_BUFFER_DESC *bufferDesc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuExternalMemoryGetMappedBuffer,
                         devPtr, extMem, bufferDesc);
}

CUresult cuExternalMemoryGetMappedMipmappedArray(
    CUmipmappedArray *mipmap, CUexternalMemory extMem,
    const CUDA_EXTERNAL_MEMORY_MIPMAPPED_ARRAY_DESC *mipmapDesc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuExternalMemoryGetMappedMipmappedArray, mipmap,
                         extMem, mipmapDesc);
}

CUresult cuGraphAddChildGraphNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                                  const CUgraphNode *dependencies,
                                  size_t numDependencies, CUgraph childGraph) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddChildGraphNode,
                         phGraphNode, hGraph, dependencies, numDependencies,
                         childGraph);
}

CUresult cuGraphAddDependencies(CUgraph hGraph, const CUgraphNode *from,
                                const CUgraphNode *to, size_t numDependencies) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddDependencies, hGraph,
                         from, to, numDependencies, numDependencies);
}

CUresult cuGraphAddEmptyNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                             const CUgraphNode *dependencies,
                             size_t numDependencies) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddEmptyNode, phGraphNode,
                         hGraph, dependencies, numDependencies);
}

CUresult cuGraphAddHostNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                            const CUgraphNode *dependencies,
                            size_t numDependencies,
                            const CUDA_HOST_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddHostNode, phGraphNode,
                         hGraph, dependencies, numDependencies, nodeParams);
}

CUresult _cuGraphAddKernelNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                              const CUgraphNode *dependencies,
                              size_t numDependencies,
                              const CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphAddKernelNode_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddKernelNode_v2, phGraphNode,
                                  hGraph, dependencies, numDependencies, nodeParams);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphAddKernelNode))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddKernelNode, phGraphNode,
                                  hGraph, dependencies, numDependencies, nodeParams);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGraphAddKernelNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                              const CUgraphNode *dependencies,
                              size_t numDependencies,
                              const CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  return _cuGraphAddKernelNode(phGraphNode, hGraph, dependencies, numDependencies, nodeParams);
}

CUresult cuGraphAddKernelNode_v2(CUgraphNode *phGraphNode, CUgraph hGraph,
                              const CUgraphNode *dependencies,
                              size_t numDependencies,
                              const CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  return _cuGraphAddKernelNode(phGraphNode, hGraph, dependencies, numDependencies, nodeParams);
}

CUresult cuGraphAddMemcpyNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                              const CUgraphNode *dependencies,
                              size_t numDependencies,
                              const CUDA_MEMCPY3D *copyParams, CUcontext ctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddMemcpyNode, phGraphNode,
                         hGraph, dependencies, numDependencies, copyParams,
                         ctx);
}

CUresult cuGraphAddMemsetNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                              const CUgraphNode *dependencies,
                              size_t numDependencies,
                              const CUDA_MEMSET_NODE_PARAMS *memsetParams,
                              CUcontext ctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddMemsetNode, phGraphNode,
                         hGraph, dependencies, numDependencies, memsetParams,
                         ctx);
}

CUresult cuGraphChildGraphNodeGetGraph(CUgraphNode hNode, CUgraph *phGraph) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphChildGraphNodeGetGraph,
                         hNode, phGraph);
}

CUresult cuGraphClone(CUgraph *phGraphClone, CUgraph originalGraph) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphClone, phGraphClone,
                         originalGraph);
}

CUresult cuGraphCreate(CUgraph *phGraph, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphCreate, phGraph, flags);
}

CUresult cuGraphDestroy(CUgraph hGraph) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphDestroy, hGraph);
}

CUresult cuGraphDestroyNode(CUgraphNode hNode) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphDestroyNode, hNode);
}

CUresult cuGraphExecDestroy(CUgraphExec hGraphExec) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecDestroy, hGraphExec);
}

CUresult cuGraphGetEdges(CUgraph hGraph, CUgraphNode *from, CUgraphNode *to,
                         size_t *numEdges) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphGetEdges, hGraph, from, to,
                         numEdges);
}

CUresult cuGraphGetNodes(CUgraph hGraph, CUgraphNode *nodes, size_t *numNodes) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphGetNodes, hGraph, nodes,
                         numNodes);
}

CUresult cuGraphGetRootNodes(CUgraph hGraph, CUgraphNode *rootNodes,
                             size_t *numRootNodes) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphGetRootNodes, hGraph,
                         rootNodes, numRootNodes);
}

CUresult cuGraphHostNodeGetParams(CUgraphNode hNode,
                                  CUDA_HOST_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphHostNodeGetParams, hNode,
                         nodeParams);
}

CUresult cuGraphHostNodeSetParams(CUgraphNode hNode,
                                  const CUDA_HOST_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphHostNodeSetParams, hNode,
                         nodeParams);
}

CUresult _cuGraphInstantiate(CUgraphExec *phGraphExec, CUgraph hGraph,
                            CUgraphNode *phErrorNode, char *logBuffer,
                            size_t bufferSize) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphInstantiate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphInstantiate_v2, phGraphExec,
                                       hGraph, phErrorNode, logBuffer, bufferSize);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphInstantiate))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphInstantiate, phGraphExec,
                                    hGraph, phErrorNode, logBuffer, bufferSize);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGraphInstantiate_v2(CUgraphExec *phGraphExec, CUgraph hGraph,
                               CUgraphNode *phErrorNode, char *logBuffer,
                               size_t bufferSize) {
  return _cuGraphInstantiate(phGraphExec, hGraph, phErrorNode, logBuffer, bufferSize);
}


CUresult cuGraphInstantiate(CUgraphExec *phGraphExec, CUgraph hGraph,
                            CUgraphNode *phErrorNode, char *logBuffer,
                            size_t bufferSize) {
  return _cuGraphInstantiate(phGraphExec, hGraph, phErrorNode, logBuffer, bufferSize);
}

CUresult _cuGraphKernelNodeGetParams(CUgraphNode hNode,
                                    CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphKernelNodeGetParams_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphKernelNodeGetParams_v2,
                                        hNode, nodeParams);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphKernelNodeGetParams))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphKernelNodeGetParams,
                                        hNode, nodeParams);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuGraphKernelNodeGetParams(CUgraphNode hNode,
                                    CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  return _cuGraphKernelNodeGetParams(hNode, nodeParams);
}

CUresult cuGraphKernelNodeGetParams_v2(CUgraphNode hNode,
                                    CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  return _cuGraphKernelNodeGetParams(hNode, nodeParams);
}

CUresult _cuGraphKernelNodeSetParams(CUgraphNode hNode,
                                    const CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphKernelNodeSetParams_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphKernelNodeSetParams_v2,
                                        hNode, nodeParams);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphKernelNodeSetParams))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphKernelNodeSetParams,
                                        hNode, nodeParams);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}


CUresult cuGraphKernelNodeSetParams(CUgraphNode hNode,
                                    const CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  return _cuGraphKernelNodeSetParams(hNode, nodeParams);
}

CUresult cuGraphKernelNodeSetParams_v2(CUgraphNode hNode,
                                    const CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  return _cuGraphKernelNodeSetParams(hNode, nodeParams);
}

CUresult cuGraphLaunch(CUgraphExec hGraphExec, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuGraphLaunch), hGraphExec,
                         hStream);
}

CUresult cuGraphLaunch_ptsz(CUgraphExec hGraphExec, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphLaunch_ptsz, hGraphExec,
                         hStream);
}

CUresult cuGraphMemcpyNodeGetParams(CUgraphNode hNode,
                                    CUDA_MEMCPY3D *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphMemcpyNodeGetParams, hNode,
                         nodeParams);
}

CUresult cuGraphMemcpyNodeSetParams(CUgraphNode hNode,
                                    const CUDA_MEMCPY3D *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphMemcpyNodeSetParams, hNode,
                         nodeParams);
}

CUresult cuGraphMemsetNodeGetParams(CUgraphNode hNode,
                                    CUDA_MEMSET_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphMemsetNodeGetParams, hNode,
                         nodeParams);
}

CUresult cuGraphMemsetNodeSetParams(CUgraphNode hNode,
                                    const CUDA_MEMSET_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphMemsetNodeSetParams, hNode,
                         nodeParams);
}

CUresult cuGraphNodeFindInClone(CUgraphNode *phNode, CUgraphNode hOriginalNode,
                                CUgraph hClonedGraph) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphNodeFindInClone, phNode,
                         hOriginalNode, hClonedGraph);
}

CUresult cuGraphNodeGetDependencies(CUgraphNode hNode,
                                    CUgraphNode *dependencies,
                                    size_t *numDependencies) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphNodeGetDependencies, hNode,
                         dependencies, numDependencies);
}

CUresult cuGraphNodeGetDependentNodes(CUgraphNode hNode,
                                      CUgraphNode *dependentNodes,
                                      size_t *numDependentNodes) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphNodeGetDependentNodes,
                         hNode, dependentNodes, numDependentNodes);
}

CUresult cuGraphNodeGetType(CUgraphNode hNode, CUgraphNodeType *type) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphNodeGetType, hNode, type);
}

CUresult cuGraphRemoveDependencies(CUgraph hGraph, const CUgraphNode *from,
                                   const CUgraphNode *to,
                                   size_t numDependencies) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphRemoveDependencies, hGraph,
                         from, to, numDependencies);
}

CUresult cuImportExternalMemory(CUexternalMemory *extMem_out,
                       const CUDA_EXTERNAL_MEMORY_HANDLE_DESC *memHandleDesc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuImportExternalMemory, extMem_out,
                         memHandleDesc);
}

CUresult cuImportExternalSemaphore( CUexternalSemaphore *extSem_out,
               const CUDA_EXTERNAL_SEMAPHORE_HANDLE_DESC *semHandleDesc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuImportExternalSemaphore,
                         extSem_out, semHandleDesc);
}

CUresult cuLaunchHostFunc(CUstream hStream, CUhostFn fn, void *userData) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchHostFunc), hStream, fn,
                         userData);
}

CUresult cuLaunchHostFunc_ptsz(CUstream hStream, CUhostFn fn, void *userData) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchHostFunc_ptsz, hStream, fn,
                         userData);
}

CUresult cuSignalExternalSemaphoresAsync(
                    const CUexternalSemaphore *extSemArray,
                    const CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS *paramsArray,
                    unsigned int numExtSems, CUstream stream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuSignalExternalSemaphoresAsync),
                         extSemArray, paramsArray, numExtSems, stream);
}

CUresult cuSignalExternalSemaphoresAsync_ptsz(
                    const CUexternalSemaphore *extSemArray,
                    const CUDA_EXTERNAL_SEMAPHORE_SIGNAL_PARAMS *paramsArray,
                    unsigned int numExtSems, CUstream stream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuSignalExternalSemaphoresAsync_ptsz, extSemArray,
                         paramsArray, numExtSems, stream);
}

//CUresult cuStreamEndCapture(CUstream hStream, CUgraph *phGraph) {
//  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamEndCapture), hStream,
//                         phGraph);
//}
//
//CUresult cuStreamEndCapture_ptsz(CUstream hStream, CUgraph *phGraph) {
//  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamEndCapture_ptsz, hStream,
//                         phGraph);
//}

CUresult cuStreamGetCtx(CUstream hStream, CUcontext *pctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCtx), hStream, pctx);
}

CUresult cuStreamGetCtx_ptsz(CUstream hStream, CUcontext *pctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetCtx_ptsz, hStream,
                         pctx);
}

CUresult cuStreamGetCtx_v2(CUstream hStream, CUcontext *pCtx, CUgreenCtx *pGreenCtx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCtx_v2), hStream,
                          pCtx, pGreenCtx);
}

CUresult cuStreamGetCtx_v2_ptsz(CUstream hStream, CUcontext *pCtx, CUgreenCtx *pGreenCtx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetCtx_v2_ptsz, hStream,
                         pCtx, pGreenCtx);
}

CUresult cuGreenCtxStreamCreate(CUstream* phStream, CUgreenCtx greenCtx,
                                unsigned int flags, int priority) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGreenCtxStreamCreate, phStream,
                         greenCtx, flags, priority);
}

CUresult cuStreamIsCapturing(CUstream hStream,
                             CUstreamCaptureStatus *captureStatus) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamIsCapturing), hStream,
                         captureStatus);
}

CUresult cuStreamIsCapturing_ptsz(CUstream hStream,
                                  CUstreamCaptureStatus *captureStatus) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamIsCapturing_ptsz, hStream,
                         captureStatus);
}

CUresult cuWaitExternalSemaphoresAsync(
                    const CUexternalSemaphore *extSemArray,
                    const CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS *paramsArray,
                    unsigned int numExtSems, CUstream stream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuWaitExternalSemaphoresAsync),
                         extSemArray, paramsArray, numExtSems, stream);
}

CUresult cuWaitExternalSemaphoresAsync_ptsz(
                    const CUexternalSemaphore *extSemArray,
                    const CUDA_EXTERNAL_SEMAPHORE_WAIT_PARAMS *paramsArray,
                    unsigned int numExtSems, CUstream stream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuWaitExternalSemaphoresAsync_ptsz,
                         extSemArray, paramsArray, numExtSems, stream);
}

CUresult cuGraphExecKernelNodeSetParams(CUgraphExec hGraphExec,
               CUgraphNode hNode, const CUDA_KERNEL_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecKernelNodeSetParams,
                         hGraphExec, hNode, nodeParams);
}

//CUresult _cuStreamBeginCapture(CUstream hStream, CUstreamCaptureMode mode) {
//  CUresult ret;
//  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBeginCapture_v2)))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBeginCapture_v2),
//                                                         hStream, mode);
//  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBeginCapture)))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamBeginCapture),
//                                                         hStream, mode);
//  } else {
//    ret = CUDA_ERROR_NOT_FOUND;
//  }
//  return ret;
//}
//
//CUresult cuStreamBeginCapture_v2(CUstream hStream, CUstreamCaptureMode mode) {
//  return _cuStreamBeginCapture(hStream, mode);
//}
//
//CUresult cuStreamBeginCapture(CUstream hStream, CUstreamCaptureMode mode) {
//  return _cuStreamBeginCapture(hStream, mode);
//}

//CUresult _cuStreamBeginCapture_ptsz(CUstream hStream, CUstreamCaptureMode mode) {
//  CUresult ret;
//  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamBeginCapture_v2_ptsz))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamBeginCapture_v2_ptsz,
//                                                        hStream, mode);
//  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamBeginCapture_ptsz))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamBeginCapture_ptsz,
//                                                        hStream, mode);
//  } else {
//    ret = CUDA_ERROR_NOT_FOUND;
//  }
//  return ret;
//}
//
//CUresult cuStreamBeginCapture_v2_ptsz(CUstream hStream, CUstreamCaptureMode mode) {
//  return _cuStreamBeginCapture_ptsz(hStream, mode);
//}
//
//CUresult cuStreamBeginCapture_ptsz(CUstream hStream, CUstreamCaptureMode mode) {
//  return _cuStreamBeginCapture_ptsz(hStream, mode);
//}

//CUresult _cuStreamGetCaptureInfo(CUstream hStream,
//                                CUstreamCaptureStatus *captureStatus,
//                                cuuint64_t *id) {
//  CUresult ret;
//  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCaptureInfo_v2)))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCaptureInfo_v2),
//                                                           hStream, captureStatus, id);
//  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCaptureInfo)))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCaptureInfo),
//                                                           hStream, captureStatus, id);
//  } else {
//    ret = CUDA_ERROR_NOT_FOUND;
//  }
//  return ret;
//}
//
//CUresult cuStreamGetCaptureInfo_v2(CUstream hStream,
//                                   CUstreamCaptureStatus *captureStatus,
//                                   cuuint64_t *id) {
//  return _cuStreamGetCaptureInfo(hStream, captureStatus, id);
//}
//
//CUresult cuStreamGetCaptureInfo(CUstream hStream,
//                                CUstreamCaptureStatus *captureStatus,
//                                cuuint64_t *id) {
//  return _cuStreamGetCaptureInfo(hStream, captureStatus, id);
//}

//CUresult _cuStreamGetCaptureInfo_ptsz(CUstream hStream,
//                                     CUstreamCaptureStatus *captureStatus,
//                                     cuuint64_t *id) {
//  CUresult ret;
//  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamGetCaptureInfo_v2_ptsz))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetCaptureInfo_v2_ptsz,
//                           hStream, captureStatus, id);
//  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuStreamGetCaptureInfo_ptsz))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetCaptureInfo_ptsz,
//                           hStream, captureStatus, id);
//  } else {
//    ret = CUDA_ERROR_NOT_FOUND;
//  }
//  return ret;
//}
//
//CUresult cuStreamGetCaptureInfo_v2_ptsz(CUstream hStream,
//                                        CUstreamCaptureStatus *captureStatus,
//                                        cuuint64_t *id) {
//  return _cuStreamGetCaptureInfo_ptsz(hStream, captureStatus, id);
//}
//
//CUresult cuStreamGetCaptureInfo_ptsz(CUstream hStream,
//                                     CUstreamCaptureStatus *captureStatus,
//                                     cuuint64_t *id) {
//  return _cuStreamGetCaptureInfo_ptsz(hStream, captureStatus, id);
//}

CUresult cuThreadExchangeStreamCaptureMode(CUstreamCaptureMode *mode) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuThreadExchangeStreamCaptureMode,
                         mode);
}

CUresult cuDeviceGetNvSciSyncAttributes(void *nvSciSyncAttrList, CUdevice dev,
                                        int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetNvSciSyncAttributes,
                         nvSciSyncAttrList, dev, flags);
}

CUresult cuGraphExecHostNodeSetParams(CUgraphExec hGraphExec, CUgraphNode hNode,
                                      const CUDA_HOST_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecHostNodeSetParams,
                         hGraphExec, hNode, nodeParams);
}

CUresult cuGraphExecMemcpyNodeSetParams(CUgraphExec hGraphExec,
                                        CUgraphNode hNode,
                                        const CUDA_MEMCPY3D *copyParams,
                                        CUcontext ctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecMemcpyNodeSetParams,
                         hGraphExec, hNode, copyParams, ctx);
}

CUresult cuGraphExecMemsetNodeSetParams(CUgraphExec hGraphExec, CUgraphNode hNode,
                       const CUDA_MEMSET_NODE_PARAMS *memsetParams, CUcontext ctx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecMemsetNodeSetParams,
                         hGraphExec, hNode, memsetParams, ctx);
}

//CUresult _cuGraphExecUpdate(CUgraphExec hGraphExec, CUgraph hGraph,
//                           CUgraphNode *hErrorNode_out,
//                           CUgraphExecUpdateResult *updateResult_out) {
//  CUresult ret;
//  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphExecUpdate_v2))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecUpdate_v2,
//                    hGraphExec, hGraph, hErrorNode_out, updateResult_out);
//  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuGraphExecUpdate))) {
//    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecUpdate,
//                    hGraphExec, hGraph, hErrorNode_out, updateResult_out);
//  } else {
//    ret = CUDA_ERROR_NOT_FOUND;
//  }
//  return ret;
//}
//
//CUresult cuGraphExecUpdate(CUgraphExec hGraphExec, CUgraph hGraph,
//                           CUgraphNode *hErrorNode_out,
//                           CUgraphExecUpdateResult *updateResult_out) {
//  return _cuGraphExecUpdate(hGraphExec, hGraph, hErrorNode_out, updateResult_out);
//}
//
//CUresult cuGraphExecUpdate_v2(CUgraphExec hGraphExec, CUgraph hGraph,
//                           CUgraphNode *hErrorNode_out,
//                           CUgraphExecUpdateResult *updateResult_out) {
//  return _cuGraphExecUpdate(hGraphExec, hGraph, hErrorNode_out, updateResult_out);
//}

CUresult cuCtxResetPersistingL2Cache(void) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxResetPersistingL2Cache);
}

CUresult cuFuncGetModule(CUmodule *hmod, CUfunction hfunc) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncGetModule);
}


CUresult cuGraphKernelNodeCopyAttributes(CUgraphNode dst, CUgraphNode src) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphKernelNodeCopyAttributes,
                         dst, src);
}

CUresult cuGraphKernelNodeGetAttribute(CUgraphNode hNode,
                                       CUkernelNodeAttrID attr,
                                       CUkernelNodeAttrValue *value_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphKernelNodeGetAttribute,
                         hNode, attr, value_out);
}

CUresult cuGraphKernelNodeSetAttribute(CUgraphNode hNode,
                                       CUkernelNodeAttrID attr,
                                       const CUkernelNodeAttrValue *value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphKernelNodeSetAttribute,
                         hNode, attr, value);
}

CUresult cuOccupancyAvailableDynamicSMemPerBlock(size_t *dynamicSmemSize,
                                                 CUfunction func, int numBlocks,
                                                 int blockSize) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuOccupancyAvailableDynamicSMemPerBlock,
                         dynamicSmemSize, func, numBlocks, blockSize);
}

CUresult cuStreamCopyAttributes(CUstream dst, CUstream src) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamCopyAttributes), dst, src);
}

CUresult cuStreamCopyAttributes_ptsz(CUstream dst, CUstream src) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamCopyAttributes_ptsz, dst,
                         src);
}

CUresult cuStreamGetAttribute(CUstream hStream, CUstreamAttrID attr,
                              CUstreamAttrValue *value_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetAttribute), hStream,
                         attr, value_out);
}

CUresult cuStreamGetAttribute_ptsz(CUstream hStream, CUstreamAttrID attr,
                                   CUstreamAttrValue *value_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetAttribute_ptsz, hStream,
                         attr, value_out);
}

CUresult cuStreamSetAttribute(CUstream hStream, CUstreamAttrID attr,
                              const CUstreamAttrValue *value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamSetAttribute), hStream,
                         attr, value);
}

CUresult cuStreamSetAttribute_ptsz(CUstream hStream, CUstreamAttrID attr,
                                   const CUstreamAttrValue *value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamSetAttribute_ptsz, hStream,
                         attr, value);
}

/** 11. 2 */
CUresult cuArrayGetPlane(CUarray *pPlaneArray, CUarray hArray,
                         unsigned int planeIdx) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayGetPlane, pPlaneArray,
                         hArray, planeIdx);
}

CUresult cuArrayGetSparseProperties(CUDA_ARRAY_SPARSE_PROPERTIES *sparseProperties,
                                    CUarray array) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayGetSparseProperties,
                         sparseProperties, array);
}

CUresult cuDeviceGetDefaultMemPool(CUmemoryPool *pool_out, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetDefaultMemPool,
                         pool_out, dev);
}

CUresult cuDeviceGetLuid(char *luid, unsigned int *deviceNodeMask, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetLuid, luid,
                         deviceNodeMask, dev);
}

CUresult cuDeviceGetMemPool(CUmemoryPool *pool, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetMemPool, pool, dev);
}

CUresult cuDeviceGetTexture1DLinearMaxWidth(size_t *maxWidthInElements, CUarray_format format,
                                            unsigned numChannels, CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetTexture1DLinearMaxWidth,
                         maxWidthInElements, format, numChannels, dev);
}

CUresult cuDeviceSetMemPool(CUdevice dev, CUmemoryPool pool) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceSetMemPool, dev, pool);
}

CUresult cuEventRecordWithFlags(CUevent hEvent, CUstream hStream, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuEventRecordWithFlags), hEvent, hStream, flags);
}

CUresult cuEventRecordWithFlags_ptsz(CUevent hEvent, CUstream hStream, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuEventRecordWithFlags_ptsz, hEvent, hStream, flags);
}

CUresult cuGraphAddEventRecordNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                                   const CUgraphNode *dependencies,
                                   size_t numDependencies, CUevent event) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddEventRecordNode,
                         phGraphNode, hGraph, dependencies, numDependencies, event);
}

CUresult cuGraphAddEventWaitNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                                 const CUgraphNode *dependencies,
                                 size_t numDependencies, CUevent event) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddEventWaitNode, phGraphNode, hGraph,
                         dependencies, numDependencies, event);
}

CUresult cuGraphAddExternalSemaphoresSignalNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                                                const CUgraphNode *dependencies, size_t numDependencies,
                                                const CUDA_EXT_SEM_SIGNAL_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuGraphAddExternalSemaphoresSignalNode, phGraphNode,
                         hGraph, dependencies, numDependencies, nodeParams);
}

CUresult cuGraphAddExternalSemaphoresWaitNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                                              const CUgraphNode *dependencies,size_t numDependencies,
                                              const CUDA_EXT_SEM_WAIT_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuGraphAddExternalSemaphoresWaitNode, phGraphNode,
                         hGraph, dependencies, numDependencies, nodeParams);
}

CUresult cuGraphEventRecordNodeGetEvent(CUgraphNode hNode, CUevent *event_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphEventRecordNodeGetEvent,
                         hNode, event_out);
}

CUresult cuGraphEventRecordNodeSetEvent(CUgraphNode hNode, CUevent event) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphEventRecordNodeSetEvent,
                         hNode, event);
}

CUresult cuGraphEventWaitNodeGetEvent(CUgraphNode hNode, CUevent *event_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphEventWaitNodeGetEvent,
                         hNode, event_out);
}

CUresult cuGraphEventWaitNodeSetEvent(CUgraphNode hNode, CUevent event) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphEventWaitNodeSetEvent,
                         hNode, event);
}

CUresult cuGraphExecChildGraphNodeSetParams(CUgraphExec hGraphExec,
                                            CUgraphNode hNode,
                                            CUgraph childGraph) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecChildGraphNodeSetParams,
                         hGraphExec, hNode, childGraph);
}

CUresult cuGraphExecEventRecordNodeSetEvent(CUgraphExec hGraphExec,
                                            CUgraphNode hNode, CUevent event) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecEventRecordNodeSetEvent,
                         hGraphExec, hNode, event);
}

CUresult cuGraphExecEventWaitNodeSetEvent(CUgraphExec hGraphExec,
                                          CUgraphNode hNode, CUevent event) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecEventWaitNodeSetEvent,
                         hGraphExec, hNode, event);
}

CUresult cuGraphExecExternalSemaphoresSignalNodeSetParams(CUgraphExec hGraphExec, CUgraphNode hNode,
                                                          const CUDA_EXT_SEM_SIGNAL_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecExternalSemaphoresSignalNodeSetParams,
                         hGraphExec, hNode, nodeParams);
}

CUresult cuGraphExecExternalSemaphoresWaitNodeSetParams(CUgraphExec hGraphExec, CUgraphNode hNode,
                                                        const CUDA_EXT_SEM_WAIT_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecExternalSemaphoresWaitNodeSetParams,
                         hGraphExec, hNode, nodeParams);
}

CUresult cuGraphExternalSemaphoresSignalNodeGetParams(CUgraphNode hNode,
                         CUDA_EXT_SEM_SIGNAL_NODE_PARAMS *params_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuGraphExternalSemaphoresSignalNodeGetParams, hNode,
                         params_out);
}

CUresult cuGraphExternalSemaphoresSignalNodeSetParams(CUgraphNode hNode,
                         const CUDA_EXT_SEM_SIGNAL_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuGraphExternalSemaphoresSignalNodeSetParams, hNode,
                         nodeParams);
}

CUresult cuGraphExternalSemaphoresWaitNodeGetParams(CUgraphNode hNode,
                         CUDA_EXT_SEM_WAIT_NODE_PARAMS *params_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuGraphExternalSemaphoresWaitNodeGetParams, hNode,
                         params_out);
}

CUresult cuGraphExternalSemaphoresWaitNodeSetParams(CUgraphNode hNode,
                         const CUDA_EXT_SEM_WAIT_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuGraphExternalSemaphoresWaitNodeSetParams, hNode,
                         nodeParams);
}

CUresult cuGraphUpload(CUgraphExec hGraphExec, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuGraphUpload), hGraphExec,
                         hStream);
}

CUresult cuGraphUpload_ptsz(CUgraphExec hGraphExec, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphUpload_ptsz, hGraphExec,
                         hStream);
}

CUresult cuMemMapArrayAsync_ptsz(CUarrayMapInfo *mapInfoList,
                                 unsigned int count, CUstream hStream) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemMapArrayAsync_ptsz,
                         mapInfoList, count, hStream);
}

CUresult cuMemPoolCreate(CUmemoryPool *pool, const CUmemPoolProps *poolProps) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolCreate, pool, poolProps);
}

CUresult cuMemPoolDestroy(CUmemoryPool pool) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolDestroy, pool);
}

CUresult cuMemPoolExportPointer(CUmemPoolPtrExportData *shareData_out,
                                CUdeviceptr ptr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolExportPointer,
                         shareData_out, ptr);
}

CUresult cuMemPoolExportToShareableHandle(void *handle_out, CUmemoryPool pool,
                                          CUmemAllocationHandleType handleType,
                                          unsigned long long flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolExportToShareableHandle,
                         handle_out, pool, handleType, flags);
}

CUresult cuMemPoolGetAccess(CUmemAccess_flags *flags, CUmemoryPool memPool,
                            CUmemLocation *location) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolGetAccess, flags, memPool,
                         location);
}

CUresult cuMemPoolGetAttribute(CUmemoryPool pool, CUmemPool_attribute attr,
                               void *value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolGetAttribute, pool, attr,
                         value);
}

CUresult cuMemPoolImportFromShareableHandle(CUmemoryPool *pool_out, void *handle,
                                            CUmemAllocationHandleType handleType,
                                            unsigned long long flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolImportFromShareableHandle,
                         pool_out, handle, handleType, flags);
}

CUresult cuMemPoolImportPointer(CUdeviceptr *ptr_out, CUmemoryPool pool,
                                CUmemPoolPtrExportData *shareData) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolImportPointer, ptr_out,
                         pool, shareData);
}
CUresult cuMemPoolSetAccess(CUmemoryPool pool, const CUmemAccessDesc *map,
                            size_t count) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolSetAccess, pool, map,
                         count);
}

CUresult cuMemPoolSetAttribute(CUmemoryPool pool, CUmemPool_attribute attr,
                               void *value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolSetAttribute, pool, attr,
                         value);
}
CUresult cuMemPoolTrimTo(CUmemoryPool pool, size_t minBytesToKeep) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemPoolTrimTo, pool,
                         minBytesToKeep);
}

CUresult cuMipmappedArrayGetSparseProperties(
    CUDA_ARRAY_SPARSE_PROPERTIES *sparseProperties, CUmipmappedArray mipmap) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuMipmappedArrayGetSparseProperties, sparseProperties,
                         mipmap);
}

CUresult cuCtxGetExecAffinity(CUexecAffinityParam *pExecAffinity,
                              CUexecAffinityType type) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetExecAffinity,
                         pExecAffinity, type);
}

CUresult cuDeviceGetExecAffinitySupport(int *pi, CUexecAffinityType type,
                                        CUdevice dev) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetExecAffinitySupport, pi,
                         type, dev);
}

CUresult cuDeviceGetGraphMemAttribute(CUdevice device,
                                      CUgraphMem_attribute attr, void *value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetGraphMemAttribute,
                         device, attr, value);
}

CUresult cuDeviceGraphMemTrim(CUdevice device) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGraphMemTrim, device);
}

CUresult cuDeviceSetGraphMemAttribute(CUdevice device,
                                      CUgraphMem_attribute attr, void *value) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceSetGraphMemAttribute,
                         device, attr, value);
}

CUresult cuFlushGPUDirectRDMAWrites(CUflushGPUDirectRDMAWritesTarget target,
                                    CUflushGPUDirectRDMAWritesScope scope) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuFlushGPUDirectRDMAWrites,
                         target, scope);
}

CUresult cuGraphAddMemAllocNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                                const CUgraphNode *dependencies,
                                size_t numDependencies,
                                CUDA_MEM_ALLOC_NODE_PARAMS *nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddMemAllocNode, phGraphNode,
                         hGraph, dependencies, numDependencies, nodeParams);
}

CUresult cuGraphAddMemFreeNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                               const CUgraphNode *dependencies,
                               size_t numDependencies, CUdeviceptr dptr) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddMemFreeNode, phGraphNode,
                         hGraph, dependencies, numDependencies, dptr);
}

CUresult cuGraphDebugDotPrint(CUgraph hGraph, const char *path,
                                       unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphDebugDotPrint, hGraph,
                         path, flags);
}

CUresult cuGraphInstantiateWithFlags(CUgraphExec *phGraphExec, CUgraph hGraph,
                                      unsigned long long flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphInstantiateWithFlags,
                         phGraphExec, hGraph, flags);
}

CUresult cuGraphMemAllocNodeGetParams(CUgraphNode hNode,
                                      CUDA_MEM_ALLOC_NODE_PARAMS *params_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphMemAllocNodeGetParams,
                         hNode, params_out);
}

CUresult cuGraphMemFreeNodeGetParams(CUgraphNode hNode, CUdeviceptr *dptr_out) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphMemFreeNodeGetParams, hNode,
                         dptr_out);
}

CUresult cuGraphReleaseUserObject(CUgraph graph, CUuserObject object,
                                  unsigned int count) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphReleaseUserObject, graph,
                         object, count);
}

CUresult cuGraphRetainUserObject(CUgraph graph, CUuserObject object,
                                 unsigned int count, unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphRetainUserObject, graph,
                         object, count, flags);
}

CUresult cuStreamUpdateCaptureDependencies(CUstream hStream,
                                           CUgraphNode *dependencies,
                                           size_t numDependencies,
                                           unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamUpdateCaptureDependencies),
                         hStream, dependencies, numDependencies, flags);
}

CUresult cuStreamUpdateCaptureDependencies_ptsz(CUstream hStream,
                                                CUgraphNode *dependencies,
                                                size_t numDependencies,
                                                unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry,
                         cuStreamUpdateCaptureDependencies_ptsz, hStream,
                         dependencies, numDependencies, flags);
}

CUresult cuUserObjectCreate(CUuserObject *object_out, void *ptr,
                            CUhostFn destroy, unsigned int initialRefcount,
                            unsigned int flags) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuUserObjectCreate, object_out,
                         ptr, destroy, initialRefcount, flags);
}

CUresult cuUserObjectRelease(CUuserObject object, unsigned int count) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuUserObjectRelease, object,
                         count);
}

CUresult cuUserObjectRetain(CUuserObject object, unsigned int count) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuUserObjectRetain, object, count);
}

CUresult  cuArrayGetMemoryRequirements(CUDA_ARRAY_MEMORY_REQUIREMENTS *memoryRequirements,
                                       CUarray array, CUdevice device){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayGetMemoryRequirements,
                          memoryRequirements, array, device);
}

CUresult  cuMipmappedArrayGetMemoryRequirements(CUDA_ARRAY_MEMORY_REQUIREMENTS *memoryRequirements,
                                                CUmipmappedArray mipmap, CUdevice device){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMipmappedArrayGetMemoryRequirements,
                          memoryRequirements, mipmap, device);
}

CUresult  cuGraphAddBatchMemOpNode(CUgraphNode *phGraphNode, CUgraph hGraph,
                                   const CUgraphNode *dependencies, size_t numDependencies,
                                   const CUDA_BATCH_MEM_OP_NODE_PARAMS *nodeParams){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddBatchMemOpNode, phGraphNode,
                          hGraph, dependencies, numDependencies, nodeParams);
}

CUresult  cuGraphBatchMemOpNodeGetParams(CUgraphNode hNode,
                                         CUDA_BATCH_MEM_OP_NODE_PARAMS *nodeParams_out){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphBatchMemOpNodeGetParams, hNode, nodeParams_out);
}

CUresult  cuGraphBatchMemOpNodeSetParams(CUgraphNode hNode,
                                         const CUDA_BATCH_MEM_OP_NODE_PARAMS *nodeParams){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphBatchMemOpNodeSetParams, hNode, nodeParams);
}

CUresult  cuGraphExecBatchMemOpNodeSetParams(CUgraphExec hGraphExec, CUgraphNode hNode,
                                             const CUDA_BATCH_MEM_OP_NODE_PARAMS *nodeParams){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecBatchMemOpNodeSetParams, hGraphExec,
                          hNode, nodeParams);
}

CUresult  cuGraphNodeGetEnabled(CUgraphExec hGraphExec, CUgraphNode hNode, unsigned int *isEnabled){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphNodeGetEnabled, hGraphExec, hNode, isEnabled);
}

CUresult  cuGraphNodeSetEnabled(CUgraphExec hGraphExec, CUgraphNode hNode, unsigned int isEnabled){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphNodeSetEnabled, hGraphExec, hNode, isEnabled);
}

//CUresult cuModuleGetLoadingMode(CUmoduleLoadingMode *mode){
//  return CUDA_ENTRY_CHECK(cuda_library_entry, cuModuleGetLoadingMode, mode);
//}

CUresult cuMemGetHandleForAddressRange(void *handle, CUdeviceptr dptr, size_t size,
                                       CUmemRangeHandleType handleType, unsigned long long flags){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetHandleForAddressRange, handle,
                          dptr, size, handleType, flags);
}

CUresult cuGraphAddNode_v2(CUgraphNode* phGraphNode, CUgraph hGraph,
                           const CUgraphNode* dependencies, const CUgraphEdgeData* dependencyData,
                           size_t numDependencies, CUgraphNodeParams* nodeParams) {
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddNode_v2, phGraphNode, hGraph,
                          dependencies, dependencyData, numDependencies, nodeParams);
}

CUresult cuGraphAddNode(CUgraphNode* phGraphNode, CUgraph hGraph, const CUgraphNode* dependencies,
                        size_t numDependencies, CUgraphNodeParams* nodeParams){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphAddNode, phGraphNode, hGraph,
                          dependencies, numDependencies, nodeParams);
}

CUresult cuGraphExecGetFlags(CUgraphExec hGraphExec, cuuint64_t* flags){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecGetFlags, hGraphExec, flags);
}

CUresult cuGraphExecNodeSetParams(CUgraphExec hGraphExec, CUgraphNode hNode, CUgraphNodeParams* nodeParams){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecNodeSetParams, hGraphExec, hNode, nodeParams);
}

CUresult cuGraphInstantiateWithParams (CUgraphExec* phGraphExec, CUgraph hGraph,
                                       CUDA_GRAPH_INSTANTIATE_PARAMS* instantiateParams ){
  return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuGraphInstantiateWithParams),
                          phGraphExec, hGraph, instantiateParams);
}

CUresult cuGraphInstantiateWithParams_ptsz (CUgraphExec* phGraphExec, CUgraph hGraph,
                                            CUDA_GRAPH_INSTANTIATE_PARAMS* instantiateParams ){
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphInstantiateWithParams_ptsz,
                                        phGraphExec, hGraph, instantiateParams);
}

CUresult cuGraphNodeSetParams(CUgraphNode hNode, CUgraphNodeParams* nodeParams){
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphNodeSetParams, hNode, nodeParams);
}

CUresult cuStreamGetId(CUstream hStream, unsigned long long* streamId){
    return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetId), hStream, streamId);
}

CUresult cuStreamGetId_ptsz(CUstream hStream, unsigned long long* streamId){
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamGetId_ptsz, hStream, streamId);
}

CUresult cuCoredumpGetAttribute(CUcoredumpSettings attrib, void *value, size_t *size) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuCoredumpGetAttribute, attrib, value, size);
}

CUresult cuCoredumpGetAttributeGlobal(CUcoredumpSettings attrib, void *value, size_t *size) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuCoredumpGetAttributeGlobal, attrib, value, size);
}

CUresult cuCoredumpSetAttribute(CUcoredumpSettings attrib, void *value, size_t *size) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuCoredumpSetAttribute, attrib, value, size);
}

CUresult cuCoredumpSetAttributeGlobal(CUcoredumpSettings attrib, void *value, size_t *size) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuCoredumpSetAttributeGlobal, attrib, value, size);
}

CUresult cuCtxGetId(CUcontext ctx, unsigned long long *ctxId) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetId, ctx, ctxId);
}

CUresult cuCtxSetFlags(unsigned int flags) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxSetFlags, flags);
}

CUresult cuKernelGetAttribute(int *pi, CUfunction_attribute attrib, CUkernel kernel, CUdevice dev) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuKernelGetAttribute, pi, attrib, kernel, dev);
}

CUresult cuKernelGetFunction(CUfunction *pFunc, CUkernel kernel) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuKernelGetFunction, pFunc, kernel);
}

CUresult cuKernelSetAttribute(CUfunction_attribute attrib, int val, CUkernel kernel, CUdevice dev) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuKernelSetAttribute, attrib, val, kernel, dev);
}

CUresult cuKernelSetCacheConfig(CUkernel kernel, CUfunc_cache config, CUdevice dev) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuKernelSetCacheConfig, kernel, config, dev);
}
//
//CUresult cuLibraryGetGlobal(CUdeviceptr *dptr, size_t *bytes, CUlibrary library, const char *name) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryGetGlobal, dptr, bytes, library, name);
//}
//
//CUresult cuLibraryGetKernel(CUkernel *pKernel, CUlibrary library, const char *name) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryGetKernel, pKernel, library, name);
//}
//
//CUresult cuLibraryGetManaged(CUdeviceptr *dptr, size_t *bytes, CUlibrary library, const char *name) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryGetManaged, dptr, bytes, library, name);
//}
//
//CUresult cuLibraryGetModule(CUmodule *pMod, CUlibrary library) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryGetModule, pMod, library);
//}
//
//CUresult cuLibraryGetUnifiedFunction(void **fptr, CUlibrary library, const char *symbol) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryGetUnifiedFunction, fptr, library, symbol);
//}
//
//CUresult cuLibraryLoadData(CUlibrary *library, const void *code, CUjit_option *jitOptions,
//                            void **jitOptionsValues, unsigned int numJitOptions,
//                            CUlibraryOption *libraryOptions, void **libraryOptionValues,
//                            unsigned int numLibraryOptions) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryLoadData, library, code,
//                            jitOptions, jitOptionsValues, numJitOptions, libraryOptions,
//                            libraryOptionValues, numLibraryOptions);
//}
//
//CUresult cuLibraryLoadFromFile(CUlibrary *library, const char *fileName, CUjit_option *jitOptions,
//                               void **jitOptionsValues, unsigned int numJitOptions,
//                               CUlibraryOption *libraryOptions, void **libraryOptionValues,
//                               unsigned int numLibraryOptions) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryLoadFromFile, library, fileName,
//                            jitOptions, jitOptionsValues, numJitOptions, libraryOptions,
//                            libraryOptionValues, numLibraryOptions);
//}
//
//CUresult cuLibraryUnload(CUlibrary library) {
//    return CUDA_ENTRY_CHECK(cuda_library_entry, cuLibraryUnload, library);
//}

CUresult cuMulticastAddDevice(CUmemGenericAllocationHandle mcHandle, CUdevice dev) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMulticastAddDevice, mcHandle, dev);
}

CUresult cuMulticastBindAddr(CUmemGenericAllocationHandle mcHandle, size_t mcOffset,
                             CUdeviceptr memptr, size_t size, unsigned long long flags) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMulticastBindAddr, mcHandle,
                            mcOffset, memptr, size, flags);
}

CUresult cuMulticastBindMem(CUmemGenericAllocationHandle mcHandle, size_t mcOffset,
                            CUmemGenericAllocationHandle memHandle, size_t memOffset,
                            size_t size, unsigned long long flags) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMulticastBindMem, mcHandle,
                            mcOffset, memHandle, memOffset, size, flags);
}

CUresult cuMulticastCreate(CUmemGenericAllocationHandle *mcHandle, const CUmulticastObjectProp *prop) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMulticastCreate, mcHandle, prop);
}

CUresult cuMulticastGetGranularity(size_t *granularity, const CUmulticastObjectProp *prop,
                                   CUmulticastGranularity_flags option) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMulticastGetGranularity, granularity, prop, option);
}

CUresult cuMulticastUnbind(CUmemGenericAllocationHandle mcHandle, CUdevice dev, size_t mcOffset, size_t size) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMulticastUnbind, mcHandle, dev, mcOffset, size);
}

CUresult cuTensorMapEncodeIm2col(CUtensorMap *tensorMap, CUtensorMapDataType tensorDataType,
                                    cuuint32_t tensorRank, void *globalAddress, const cuuint64_t *globalDim,
                                    const cuuint64_t *globalStrides, const int *pixelBoxLowerCorner,
                                    const int *pixelBoxUpperCorner, cuuint32_t channelsPerPixel,
                                    cuuint32_t pixelsPerColumn, const cuuint32_t *elementStrides,
                                    CUtensorMapInterleave interleave, CUtensorMapSwizzle swizzle,
                                    CUtensorMapL2promotion l2Promotion, CUtensorMapFloatOOBfill oobFill) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuTensorMapEncodeIm2col, tensorMap, tensorDataType,
                            tensorRank, globalAddress, globalDim, globalStrides, pixelBoxLowerCorner,
                            pixelBoxUpperCorner, channelsPerPixel, pixelsPerColumn, elementStrides,
                            interleave, swizzle, l2Promotion, oobFill);
}

CUresult cuTensorMapEncodeTiled(CUtensorMap *tensorMap, CUtensorMapDataType tensorDataType,
                                cuuint32_t tensorRank, void *globalAddress, const cuuint64_t *globalDim,
                                const cuuint64_t *globalStrides, const cuuint32_t *boxDim,
                                const cuuint32_t *elementStrides, CUtensorMapInterleave interleave,
                                CUtensorMapSwizzle swizzle, CUtensorMapL2promotion l2Promotion,
                                CUtensorMapFloatOOBfill oobFill) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuTensorMapEncodeTiled, tensorMap, tensorDataType,
                            tensorRank, globalAddress, globalDim, globalStrides, boxDim, elementStrides,
                            interleave, swizzle, l2Promotion, oobFill);
}

CUresult cuTensorMapReplaceAddress(CUtensorMap *tensorMap, void *globalAddress) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuTensorMapReplaceAddress, tensorMap, globalAddress);
}

CUresult cuMemMap(CUdeviceptr ptr, size_t size, size_t offset,
                  CUmemGenericAllocationHandle handle, unsigned long long flags) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemMap, ptr, size, offset, handle, flags);
}

CUresult cuMemUnmap(CUdeviceptr ptr, size_t size) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemUnmap, ptr, size);
}

CUresult cuMemAddressReserve(CUdeviceptr *ptr, size_t size, size_t alignment,
                             CUdeviceptr addr, unsigned long long flags) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAddressReserve, ptr, size, alignment, addr, flags);
}

CUresult cuMemAddressFree(CUdeviceptr ptr, size_t size) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAddressFree, ptr, size);
}

CUresult cuMemExportToShareableHandle(void *shareableHandle, CUmemGenericAllocationHandle handle,
                                      CUmemAllocationHandleType handleType, unsigned long long flags) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemExportToShareableHandle, shareableHandle, handle,
                            handleType, flags);
}

CUresult cuMemGetAccess(unsigned long long *flags, const CUmemLocation *location, CUdeviceptr ptr) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetAccess, flags, location, ptr);
}

CUresult cuMemGetAllocationGranularity(size_t *granularity, const CUmemAllocationProp *prop,
                                        CUmemAllocationGranularity_flags option) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetAllocationGranularity,
                            granularity, prop, option);
}

CUresult cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp *prop, CUmemGenericAllocationHandle handle) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetAllocationPropertiesFromHandle, prop, handle);
}

CUresult cuMemImportFromShareableHandle(CUmemGenericAllocationHandle *handle,
                                        void *osHandle, CUmemAllocationHandleType shHandleType) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemImportFromShareableHandle,
                            handle, osHandle, shHandleType);
}

CUresult cuMemMapArrayAsync(CUarrayMapInfo *mapInfoList, unsigned int count, CUstream hStream) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemMapArrayAsync), mapInfoList, count, hStream);
}

CUresult cuMemRelease(CUmemGenericAllocationHandle handle) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemRelease, handle);
}

CUresult cuMemRetainAllocationHandle(CUmemGenericAllocationHandle *handle, void *addr) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemRetainAllocationHandle, handle, addr);
}

CUresult cuMemSetAccess(CUdeviceptr ptr, size_t size, const CUmemAccessDesc *desc, size_t count) {
    return CUDA_ENTRY_CHECK(cuda_library_entry, cuMemSetAccess, ptr, size, desc, count);
}
