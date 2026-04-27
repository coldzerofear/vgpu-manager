/*
 * Shared stubs for Vulkan layer unit tests. See test_helpers.h.
 *
 * Each stub here replaces a symbol that lives in
 * cuda_hook.c / loader.c / lock.c — pulled in by the layer .c files
 * but not desirable in the test binary. Stubs are configurable via
 * the vgpu_test_* globals so individual test drivers can assert on
 * call counts / argument captures and force return values.
 */
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "include/hook.h"
#include "include/budget.h"
#include "include/cuda-subset.h"

#include "test_helpers.h"

/* ---- g_vgpu_config ---------------------------------------------- */

resource_data_t  vgpu_test_config;
resource_data_t *g_vgpu_config = NULL;

void vgpu_test_reset_config(void) {
  memset(&vgpu_test_config, 0, sizeof(vgpu_test_config));
  g_vgpu_config = &vgpu_test_config;
}

/* ---- vgpu_check_alloc_budget mock ------------------------------- */

vgpu_path_t        vgpu_test_budget_forced_path        = VGPU_PATH_GPU;
int                vgpu_test_budget_call_count         = 0;
int                vgpu_test_budget_last_host_index    = -2;
size_t             vgpu_test_budget_last_request_size  = 0;
vgpu_budget_kind_t vgpu_test_budget_last_kind          = VGPU_BUDGET_KIND_VIRTUAL;
int                vgpu_test_budget_last_allow_uva     = -1;
int                vgpu_test_budget_lock_fd_to_set     = -1;

vgpu_path_t vgpu_check_alloc_budget(int host_index,
                                    size_t request_size,
                                    vgpu_budget_kind_t kind,
                                    int allow_uva,
                                    int *lock_fd) {
  vgpu_test_budget_call_count++;
  vgpu_test_budget_last_host_index   = host_index;
  vgpu_test_budget_last_request_size = request_size;
  vgpu_test_budget_last_kind         = kind;
  vgpu_test_budget_last_allow_uva    = allow_uva;
  if (lock_fd) *lock_fd = vgpu_test_budget_lock_fd_to_set;
  return vgpu_test_budget_forced_path;
}

/* prepare_memory_allocation is exported by cuda_hook.c. Vulkan tests
 * never call it but the symbol must exist for the hook layer to link.
 * (Provide a hard fail here so any accidental use is caught.) */
vgpu_path_t prepare_memory_allocation(CUdevice device,
                                      size_t request_size,
                                      vgpu_budget_kind_t kind,
                                      int allow_uva,
                                      int *host_index,
                                      int *lock_fd) {
  (void)device; (void)request_size; (void)kind; (void)allow_uva;
  (void)host_index; (void)lock_fd;
  fprintf(stderr, "FATAL: prepare_memory_allocation called from Vulkan "
                  "unit test — should not happen\n");
  abort();
}

/* ---- vgpu_rate_limit_by_host_index mock ------------------------- */

int vgpu_test_throttle_call_count       = 0;
int vgpu_test_throttle_last_host_index  = -2;
int vgpu_test_throttle_last_kernel_size = 0;

void vgpu_rate_limit_by_host_index(int kernel_size, int host_index) {
  vgpu_test_throttle_call_count++;
  vgpu_test_throttle_last_host_index  = host_index;
  vgpu_test_throttle_last_kernel_size = kernel_size;
}

void vgpu_ensure_sm_watcher_started(void) {
  /* No-op: layer.c calls this at vk_layer_CreateInstance success.
   * The dispatch / hook tests do not exercise vk_layer_CreateInstance
   * end-to-end, but stubs the symbol regardless so the layer .c files
   * link cleanly. */
}

/* ---- lock_gpu_device / unlock_gpu_device ------------------------ */

int vgpu_test_unlock_call_count = 0;
int vgpu_test_unlock_last_fd    = -1;

int lock_gpu_device(int host_index) {
  (void)host_index;
  return -1;   /* Tests reach the lock path only via the budget mock,
                  which sets *lock_fd directly. */
}

void unlock_gpu_device(int fd) {
  vgpu_test_unlock_call_count++;
  vgpu_test_unlock_last_fd = fd;
}

/* ---- UUID-bytes -> host_index resolver -------------------------- */

/* Format the 16-byte UUID into the canonical "GPU-..." string and
 * scan vgpu_test_config.devices[i].uuid for a match. */
void get_host_device_index_by_uuid_bytes(const uint8_t uuid[16],
                                         int *host_index) {
  if (uuid == NULL || host_index == NULL || g_vgpu_config == NULL) return;
  char uuid_str[UUID_BUFFER_SIZE];
  snprintf(uuid_str, sizeof(uuid_str),
           "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
           uuid[0x0], uuid[0x1], uuid[0x2], uuid[0x3],
           uuid[0x4], uuid[0x5], uuid[0x6], uuid[0x7],
           uuid[0x8], uuid[0x9], uuid[0xA], uuid[0xB],
           uuid[0xC], uuid[0xD], uuid[0xE], uuid[0xF]);
  for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
    if (strcmp(g_vgpu_config->devices[i].uuid, uuid_str) == 0) {
      *host_index = i;
      return;
    }
  }
}

/* ---- Other globals not exercised by the hook code paths --------- */

/* layer.c's vk_layer_CreateInstance reaches load_necessary_data and
 * init_devices_mapping; the dispatch / hook unit tests do not
 * exercise CreateInstance end-to-end. Stubbed for link cleanliness
 * in case a future test does. */
void load_necessary_data(void) {}
void init_devices_mapping(void) {}

/* LOGGER macro is fully self-contained inside hook.h via file-static
 * helpers (_level_names, get_logger_print_level). No external symbol
 * needed; each TU includes its own copy. */

/* ---- reset all -------------------------------------------------- */

void vgpu_test_reset_all(void) {
  vgpu_test_reset_config();
  vgpu_test_budget_forced_path        = VGPU_PATH_GPU;
  vgpu_test_budget_call_count         = 0;
  vgpu_test_budget_last_host_index    = -2;
  vgpu_test_budget_last_request_size  = 0;
  vgpu_test_budget_last_kind          = VGPU_BUDGET_KIND_VIRTUAL;
  vgpu_test_budget_last_allow_uva     = -1;
  vgpu_test_budget_lock_fd_to_set     = -1;
  vgpu_test_throttle_call_count       = 0;
  vgpu_test_throttle_last_host_index  = -2;
  vgpu_test_throttle_last_kernel_size = 0;
  vgpu_test_unlock_call_count         = 0;
  vgpu_test_unlock_last_fd            = -1;
}

/* ---- pass reporting --------------------------------------------- */

void vgpu_test_pass(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  fputs("ok: ", stdout);
  vprintf(fmt, ap);
  fputc('\n', stdout);
  va_end(ap);
  fflush(stdout);
}
