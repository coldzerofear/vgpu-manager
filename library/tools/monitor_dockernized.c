/*
 * Tencent is pleased to support the open source community by making TKEStack available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use
 * this file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

//
// Created by thomas on 5/17/18.
//

#include <fcntl.h>
#include <string.h>
#include <sys/time.h>
#include <unistd.h>

#include "include/hook.h"
#include "include/nvml-helper.h"

extern entry_t nvml_library_entry[];
extern char driver_version[];
extern resource_data_t g_vgpu_config;

int split_str(char *line, char *key, char *value, char d)
{
  int index = 0;
  for (index = 0; index < strlen(line) && line[index] != d; index++){
  }

  if (index == strlen(line)){
    key[0] = '\0';
    value = '\0';
    return 1;
  }

  int start = 0, i = 0;
  // trim head
  for (; start < index && (line[start] == ' ' || line[start] == '\t'); start++){
  }

  for (i = 0; start < index; i++, start++) {
    key[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (key[i - 1] == '\0' || key[i - 1] == '\n' || key[i - 1] == '\t'); i--){
  }
  key[i] = '\0';

  start = index + 1;
  i = 0;

  // trim head
  for (; start < strlen(line) && (line[start] == ' ' || line[start] == '\t'); start++){
  }

  for (i = 0; start < strlen(line); i++, start++) {
    value[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (value[i - 1] == '\0' || value[i - 1] == '\n' || value[i - 1] == '\t'); i--){
  }
  value[i] = '\0';
  return 0;
}


int read_cgroup(char *pidpath, char *cgroup_key, char *cgroup_value)
{
  char buff[255];
  FILE *f = fopen(pidpath, "rb");
  if (f == NULL) {
    LOGGER(VERBOSE, "read file %s failed\n", pidpath);
    return 1;
  }

  while (fgets(buff, 255, f)) {
    int index = 0;
    for (; index < strlen(buff) && buff[index] != ':'; index++) {
    }
    if (index == strlen(buff))
      continue;
    char key[128], value[128];
    if (split_str(&buff[index + 1], key, value, ':') != 0)
      continue;
    if (strcmp(key, cgroup_key) == 0) {
      strcpy(cgroup_value, value);
      fclose(f);
      return 0;
    }
  }
  fclose(f);
  return 1;
}

int check_in_pod() {
  if (access(HOST_PROC_PATH, F_OK) != -1) {
      return 0;
  } else {
      return 1;
  }
}

int check_pod_pid(unsigned int pid) {
  if (pid == 0) {
    return 1;
  }
  char pidpath[128] = "";
  sprintf(pidpath, HOST_CGROUP_PID_PATH, pid);

  char pod_cg[256];
  char process_cg[256];

  if ((read_cgroup(PID_SELF_CGROUP_PATH, "memory", pod_cg) == 0)
      && (read_cgroup(pidpath, "memory", process_cg) == 0)) {
    LOGGER(VERBOSE, "pod cg: %s\nprocess_cg: %s", pod_cg, process_cg);
    if (strstr(process_cg, pod_cg) != NULL) {
      LOGGER(VERBOSE, "cgroup match");
      return 0;
    }
  }
  LOGGER(VERBOSE, "cgroup mismatch");
  return 1;
}

int main(void) {
  int ret = 0;

  int i = 0, j = 0, k = 0;

  int device_num = 0;
  nvmlDevice_t dev;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  unsigned int size_on_device = MAX_PIDS;

  struct timeval cur;
  size_t microsec;
  nvmlProcessUtilizationSample_t processes_sample[MAX_PIDS];
  int processes_num = MAX_PIDS;

  int sm_util = 0;
  uint64_t memory = 0;
  nvmlProcessInfo_t *process_match = NULL;
  nvmlProcessUtilizationSample_t *sample_match = NULL;

  load_necessary_data();

  NVML_ENTRY_CALL(nvml_library_entry, nvmlInit);
  fprintf(stderr, "Device\tProcess\tUtilization\tMemory\n");
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetCount, &device_num);
  if (unlikely(ret)) {
    LOGGER(ERROR, "Get device number return %d", ret);
    return 1;
  }

  for (i = 0; i < device_num; i++) {
    ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, i, &dev);
    if (unlikely(ret)) {
      LOGGER(ERROR, "Get device %d return %d", i, ret);
      continue;
    }
    
    size_on_device = MAX_PIDS;
    ret = NVML_ENTRY_CALL(nvml_library_entry,
                          nvmlDeviceGetComputeRunningProcesses, dev,
                          &size_on_device, pids_on_device);
    if (unlikely(ret)) {
      LOGGER(ERROR, "Get process gpu memory return %d", ret);
      continue;
    }

    processes_num = MAX_PIDS;
    gettimeofday(&cur, NULL);
    microsec = (cur.tv_sec - 1) * 1000UL * 1000UL + cur.tv_usec;
    ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetProcessUtilization,
                          dev, processes_sample, &processes_num, microsec);
    if (unlikely(ret)) {
      LOGGER(ERROR, "Get process utilization return %d", ret);
      processes_num = 0;
      // continue;
    }

    if (likely(check_in_pod() == 0)) {
      // 当宿主机进程目录存在, 代表运行于容器中
      for (j = 0; j < size_on_device; j++) {
        process_match = NULL;
        sample_match = NULL;
        // 校验pid是否是当前容器的pid，匹配上了就增加到已使用内存
        if (likely(check_pod_pid(pids_on_device[j].pid) == 1)) {
           continue;
        }
        process_match = &pids_on_device[j];
        for (k = 0; k < processes_num; k++) {
          if (processes_sample[k].pid == process_match->pid) {
            sample_match = &processes_sample[k];
            break;
          }
        }
        if (process_match) {
          memory = process_match->usedGpuMemory;
          memory >>= 20;
          if (sample_match) {
            sm_util = sample_match->smUtil;
          } else {
            sm_util = 0;
          }
          fprintf(stderr, "%-6d\t%d\t%-11d\t%-6" PRIu64 " MB\n", i, process_match->pid, sm_util,
                  memory);
        }
      }
    } else {
      // 没有找到宿主机进程目录，表示运行于物理机，添加所有进程的已使用内存
      for (j = 0; j < size_on_device; j++) {
        process_match = NULL;
        sample_match = NULL;
        process_match = &pids_on_device[j];
        for (k = 0; k < processes_num; k++) {
          if (processes_sample[k].pid == process_match->pid) {
            sample_match = &processes_sample[k];
            break;
          }
        }
        memory = process_match->usedGpuMemory;
        memory >>= 20;
        if (sample_match) {
          sm_util = sample_match->smUtil;
        } else {
          sm_util = 0;
        }
        fprintf(stderr, "%-6d\t%d\t%-11d\t%-6" PRIu64 " MB\n", i, process_match->pid, sm_util,
                memory);
      }
    }
  }

  NVML_ENTRY_CALL(nvml_library_entry, nvmlShutdown);
}
