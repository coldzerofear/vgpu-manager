/*
 * Negotiate-protocol test (HAMi parity).
 *
 * Loads the built libvgpu-control.so via dlopen, looks up
 * vkNegotiateLoaderLayerInterfaceVersion (the only ELF-exported Vulkan
 * symbol), and asserts the v2 protocol contract.
 *
 * The path to the .so is taken from VGPU_TEST_SO env var, set by the
 * runner script.
 *
 * Why RTLD_LAZY (not RTLD_NOW):
 *   loader.c references the glibc-private symbol `_dl_sym` as a
 *   fallback for the LD_PRELOAD dlsym-interception trick. That symbol
 *   is exported by ld.so under version GLIBC_PRIVATE and is reachable
 *   on the LD_PRELOAD load path (which production uses) but is NOT
 *   reachable through a regular dlopen — the user-mode symbol lookup
 *   filter excludes GLIBC_PRIVATE versions.
 *
 *   Under RTLD_NOW, dlopen would eagerly try to resolve `_dl_sym` and
 *   abort with "undefined symbol: _dl_sym". Under RTLD_LAZY, function
 *   symbols stay in the PLT until first call. This test only calls
 *   vkNegotiateLoaderLayerInterfaceVersion — which is a pure
 *   pVersionStruct setter that never touches the CUDA path that
 *   contains the `_dl_sym` reference. So the symbol is never
 *   resolved and the dlopen succeeds.
 */
#include <assert.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <vulkan/vulkan.h>
#include <vulkan/vk_layer.h>

typedef VkResult (VKAPI_PTR *PFN_vkNegotiate)(VkNegotiateLayerInterface *);

static void check_negotiate_basic(void *h) {
  PFN_vkNegotiate fn =
      (PFN_vkNegotiate)dlsym(h, "vkNegotiateLoaderLayerInterfaceVersion");
  if (fn == NULL) {
    fprintf(stderr, "FAIL: vkNegotiateLoaderLayerInterfaceVersion not "
                    "exported by libvgpu-control.so\n");
    exit(1);
  }

  /* Happy path: loader presents v2; layer accepts and fills callbacks. */
  VkNegotiateLayerInterface iface;
  memset(&iface, 0, sizeof(iface));
  iface.sType = LAYER_NEGOTIATE_INTERFACE_STRUCT;
  iface.loaderLayerInterfaceVersion = 2;

  VkResult r = fn(&iface);
  assert(r == VK_SUCCESS);
  assert(iface.loaderLayerInterfaceVersion == 2);
  assert(iface.pfnGetInstanceProcAddr != NULL);
  assert(iface.pfnGetDeviceProcAddr != NULL);
  /* v2 layers do not use the back-channel pfn_GetPhysicalDeviceProcAddr;
   * physical-device hooks are routed via the GIPA chain. */
  assert(iface.pfnGetPhysicalDeviceProcAddr == NULL);
  printf("ok: negotiate v2 happy path\n");
}

static void check_negotiate_clamps_high_version(void *h) {
  PFN_vkNegotiate fn =
      (PFN_vkNegotiate)dlsym(h, "vkNegotiateLoaderLayerInterfaceVersion");
  /* Loader presenting "v3" or higher: layer should clamp to v2. */
  VkNegotiateLayerInterface iface;
  memset(&iface, 0, sizeof(iface));
  iface.sType = LAYER_NEGOTIATE_INTERFACE_STRUCT;
  iface.loaderLayerInterfaceVersion = 5;

  VkResult r = fn(&iface);
  assert(r == VK_SUCCESS);
  assert(iface.loaderLayerInterfaceVersion == 2);
  printf("ok: negotiate clamps loaderLayerInterfaceVersion>2 to 2\n");
}

static void check_negotiate_rejects_v1(void *h) {
  PFN_vkNegotiate fn =
      (PFN_vkNegotiate)dlsym(h, "vkNegotiateLoaderLayerInterfaceVersion");
  VkNegotiateLayerInterface iface;
  memset(&iface, 0, sizeof(iface));
  iface.sType = LAYER_NEGOTIATE_INTERFACE_STRUCT;
  iface.loaderLayerInterfaceVersion = 1;

  VkResult r = fn(&iface);
  assert(r == VK_ERROR_INITIALIZATION_FAILED);
  printf("ok: negotiate rejects v1 (legacy)\n");
}

static void check_negotiate_rejects_null(void *h) {
  PFN_vkNegotiate fn =
      (PFN_vkNegotiate)dlsym(h, "vkNegotiateLoaderLayerInterfaceVersion");
  VkResult r = fn(NULL);
  assert(r == VK_ERROR_INITIALIZATION_FAILED);
  printf("ok: negotiate rejects NULL pVersionStruct\n");
}

static void check_only_one_vk_export(const char *so_path) {
  /* Use a symbol-list pipe: dlsym only finds named symbols, but we
   * want to assert no OTHER vk* symbol is exported. Easiest is to
   * shell out to `nm`; runner does this directly via
   * check_vulkan_layer.sh. Here, we just dlsym a few common names
   * and assert they are NOT resolvable. */
  void *h = dlopen(so_path, RTLD_LAZY | RTLD_LOCAL);
  assert(h != NULL);
  static const char *forbidden[] = {
    "vkAllocateMemory",
    "vkFreeMemory",
    "vkQueueSubmit",
    "vkCreateInstance",
    "vkGetPhysicalDeviceMemoryProperties",
    NULL,
  };
  for (const char **p = forbidden; *p; p++) {
    void *sym = dlsym(h, *p);
    /* dlsym should return NULL for hidden-visibility / unexported
     * symbols. RTLD_LOCAL ensures we do not pick up unrelated symbols
     * already in the process global table. */
    if (sym != NULL) {
      fprintf(stderr,
              "FAIL: %s is exported by libvgpu-control.so — would let "
              "the dynamic linker bypass the Vulkan loader\n", *p);
      exit(1);
    }
  }
  dlclose(h);
  printf("ok: no Vulkan entry point other than negotiate is ELF-exported\n");
}

int main(void) {
  const char *so_path = getenv("VGPU_TEST_SO");
  if (so_path == NULL || *so_path == '\0') {
    fprintf(stderr, "FAIL: VGPU_TEST_SO env var not set; "
                    "expected path to libvgpu-control.so\n");
    return 1;
  }

  void *h = dlopen(so_path, RTLD_LAZY);
  if (h == NULL) {
    fprintf(stderr, "FAIL: dlopen(%s) failed: %s\n", so_path, dlerror());
    return 1;
  }

  check_negotiate_basic(h);
  check_negotiate_clamps_high_version(h);
  check_negotiate_rejects_v1(h);
  check_negotiate_rejects_null(h);
  dlclose(h);

  check_only_one_vk_export(so_path);

  printf("ok: test_layer_init complete\n");
  return 0;
}
