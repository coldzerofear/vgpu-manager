/*
 * Shared helpers for Vulkan layer unit tests.
 *
 * Each test driver is a standalone executable that links the relevant
 * src/vulkan/ source files directly. The library expects external
 * symbols defined in cuda_hook.c / loader.c / lock.c — none of which
 * we want to compile into the test binary (they pull in CUDA / NVML /
 * kernel locks). stubs.c provides those symbols, plus controllable
 * mocks for the budget / rate-limit / NVML resolver helpers.
 *
 * No GPU / no CUDA / no NVML is required to build or run any test.
 */
#ifndef VGPU_VK_TEST_HELPERS_H
#define VGPU_VK_TEST_HELPERS_H

#include <stddef.h>
#include <stdint.h>

#include <vulkan/vulkan.h>

#include "include/hook.h"     /* resource_data_t */
#include "include/budget.h"   /* vgpu_path_t, vgpu_budget_kind_t */

#ifdef __cplusplus
extern "C" {
#endif

/* The real g_vgpu_config lives in loader.c; tests get their own
 * instance from stubs.c. Direct access lets tests configure
 * per-device fields (uuid, real_memory, total_memory, memory_limit,
 * memory_oversold, core_limit) before exercising the layer. */
extern resource_data_t  vgpu_test_config;
extern resource_data_t *g_vgpu_config;

/* Reset vgpu_test_config to all-zero and reseat g_vgpu_config to
 * point at it. Call at the start of every test that touches config
 * state. */
void vgpu_test_reset_config(void);

/* ---- Mocks: vgpu_check_alloc_budget ----------------------------- */

/* Forced result for vgpu_check_alloc_budget. Set by tests; read by
 * the stub. Defaults to VGPU_PATH_GPU. */
extern vgpu_path_t vgpu_test_budget_forced_path;

/* Recorded call args for the most recent vgpu_check_alloc_budget. */
extern int    vgpu_test_budget_call_count;
extern int    vgpu_test_budget_last_host_index;
extern size_t vgpu_test_budget_last_request_size;
extern vgpu_budget_kind_t vgpu_test_budget_last_kind;
extern int    vgpu_test_budget_last_allow_uva;

/* The stub returns *out_lock_fd = vgpu_test_budget_lock_fd_to_set
 * so tests can assert that hooks_alloc.c calls unlock_gpu_device only
 * when the fd is >= 0. */
extern int vgpu_test_budget_lock_fd_to_set;

/* ---- Mocks: vgpu_rate_limit_by_host_index ----------------------- */

extern int vgpu_test_throttle_call_count;
extern int vgpu_test_throttle_last_host_index;
extern int vgpu_test_throttle_last_kernel_size;

/* ---- Mocks: lock_gpu_device / unlock_gpu_device ----------------- */

extern int vgpu_test_unlock_call_count;
extern int vgpu_test_unlock_last_fd;

/* ---- Mocks: get_host_device_index_by_uuid_bytes ----------------- */

/* The stub matches `uuid` against vgpu_test_config.devices[*].uuid
 * via the canonical "GPU-..." string form. Tests configure
 * vgpu_test_config.devices[i].uuid + .memory_limit and the resolver
 * returns i. */

/* ---- Reset everything at start of test ------------------------- */
void vgpu_test_reset_all(void);

/* ---- Test reporting --------------------------------------------- */

/* Print "ok: <message>" to stdout. The runner script greps "^ok:"
 * to count successes. */
void vgpu_test_pass(const char *fmt, ...);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VK_TEST_HELPERS_H */
