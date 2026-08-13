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
 * Vendored from lupine's `checkpoint_provider.h` (Apache-2.0) to keep
 * library-remote buildable without a lupine checkout.
 *
 * This is the ABI contract between lupine-server and the external
 * "checkpoint provider" plugin (dlopen'd per connection child as
 * `liblupinecr.so`, or via LUPINE_CHECKPOINT_LIBRARY). library-remote embeds
 * an implementation of this provider so the same libvgpu-remote.so both:
 *   - LD_PRELOAD'd into lupine_driver_server to hook cu[A-Z]* / nvml[A-Z]*
 *     (C-1), and
 *   - dlopen'd by lupine as the checkpoint provider, whose restore() injects
 *     the per-session VGPU_CONFIG_PATH env before the first CUDA RPC (C-2).
 *
 * Design ref: docs/remote_gpu_pool_research_design.md sec.4.3.3 / 4.3.3.1.
 */

#ifndef VGPU_CHECKPOINT_PROVIDER_H
#define VGPU_CHECKPOINT_PROVIDER_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define LUPINE_CHECKPOINT_PROVIDER_ABI_VERSION 1u
#define LUPINE_CHECKPOINT_PROVIDER_SYMBOL "lupinecr_get_lupine_provider_v1"

typedef struct lupine_checkpoint_provider_v1 {
  size_t struct_size;
  uint32_t abi_version;

  /* Called in a freshly forked connection process before its first CUDA call.
   * Providers can begin observing RM/UVM activity here. */
  int (*start)(void);

  /* Restores the named connection before its first CUDA RPC is dispatched.
   * A missing checkpoint is success; malformed or unrestorable state fails the
   * connection rather than allowing it to continue with empty GPU memory. */
  int (*restore)(const char *connection_id);

  /* Writes a complete checkpoint for the current connection. The identifier
   * is null when the client did not supply one. The provider owns storage,
   * file layout, and any fallback policy for unnamed connections. */
  int (*checkpoint)(const char *connection_id);

  /* Stops observation and releases provider-owned process state. */
  void (*stop)(void);
} lupine_checkpoint_provider_v1;

typedef const lupine_checkpoint_provider_v1 *(
    *lupine_checkpoint_provider_get_v1_fn)(void);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_CHECKPOINT_PROVIDER_H */
