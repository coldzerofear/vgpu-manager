#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "include/hook.h"

int main() {
    char container_id[FILENAME_MAX] = {0};
    extract_container_id(container_id, FILENAME_MAX);
    if (strlen(container_id) > 0) {
        printf("Container ID: %s\n", container_id);
    } else {
        printf("No container ID found.\n");
    }
    char *pod_uid = getenv("VGPU_POD_UID");
    if (pod_uid == NULL) {
        return 0;
    } 
    for (char *p = pod_uid; *p != '\0'; p++) {
        if (*p == '-') {
            *p = '_';
        }
    }
    char full_path[FILENAME_MAX];
    snprintf(full_path, sizeof(full_path), "/etc/vgpu-manager/host_cgroup/kubepods-burstable-pod%s.slice", pod_uid);
    #define HOST_CGROUP_PROCS_PATH (full_path)
    LOGGER(INFO, "%s",HOST_CGROUP_PROCS_PATH);
    char v2_container_id[FILENAME_MAX] = {0};
    extract_container_id_v2(full_path, v2_container_id, FILENAME_MAX);
    if (strlen(v2_container_id)) {
        printf("v2 Container ID: %s\n", v2_container_id);
    } else {
        printf("v2 No container ID found.\n");
    }
    return 0;
}