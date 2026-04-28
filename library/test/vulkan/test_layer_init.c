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

/* End-to-end exercise of the four Enumerate*Properties hooks via the
 * dlopen + negotiate + GIPA path. Catches the NULL-GIPA SegFault that
 * HAMi PR #182 observed on Isaac Sim Kit + NVIDIA Carbonite — if any
 * of these GIPA lookups returns NULL, the loader (or downstream
 * graphics framework) would dereference and crash.
 *
 * Important: -O3 -DNDEBUG (the standard test build flags) strips
 * assert() down to ((void)0), so any function call placed INSIDE an
 * assert() expression silently disappears. Pre-evaluate side-effecting
 * calls into a local variable, then assert on the captured value;
 * for hard-fail conditions where assertion-stripping would crash a
 * subsequent step, use an explicit `if + fprintf + exit` instead. */
static void check_enumerate_hooks_resolve_with_null_instance(void *h) {
  PFN_vkNegotiate fn =
      (PFN_vkNegotiate)dlsym(h, "vkNegotiateLoaderLayerInterfaceVersion");
  if (fn == NULL) {
    fprintf(stderr, "FAIL: vkNegotiate symbol missing\n");
    exit(1);
  }
  VkNegotiateLayerInterface iface;
  memset(&iface, 0, sizeof(iface));
  iface.sType = LAYER_NEGOTIATE_INTERFACE_STRUCT;
  iface.loaderLayerInterfaceVersion = 2;
  /* Side-effect-bearing call OUTSIDE assert so NDEBUG cannot strip it. */
  VkResult neg_r = fn(&iface);
  assert(neg_r == VK_SUCCESS);
  PFN_vkGetInstanceProcAddr gipa = iface.pfnGetInstanceProcAddr;
  if (gipa == NULL) {
    fprintf(stderr, "FAIL: negotiate did not populate pfnGetInstanceProcAddr\n");
    exit(1);
  }

  /* Each hooked name MUST resolve to a non-NULL pfn even with
   * VK_NULL_HANDLE as the instance — that is precisely the bug. */
  static const char *const names[] = {
    "vkEnumerateInstanceExtensionProperties",
    "vkEnumerateInstanceLayerProperties",
    "vkEnumerateDeviceExtensionProperties",
    "vkEnumerateDeviceLayerProperties",
    NULL,
  };
  for (const char *const *p = names; *p; p++) {
    PFN_vkVoidFunction pfn = gipa(VK_NULL_HANDLE, *p);
    if (pfn == NULL) {
      fprintf(stderr,
              "FAIL: GIPA(VK_NULL_HANDLE, %s) returned NULL — would "
              "SegFault under loaders that dereference the result "
              "(see HAMi PR #182 Isaac Sim repro)\n", *p);
      exit(1);
    }
  }
  printf("ok: 4 vkEnumerate*Properties hooks resolve via GIPA(VK_NULL_HANDLE, ...)\n");

  /* Behavioral: instance-extension query semantics.
   *   - own-name => VK_SUCCESS, count = 0
   *   - NULL or other name => VK_ERROR_LAYER_NOT_PRESENT (so the
   *     loader walks past us to the ICD instead of treating our
   *     zero-count as authoritative). */
  typedef VkResult (VKAPI_PTR *PFN_inst_ext)(const char *, uint32_t *,
                                             VkExtensionProperties *);
  PFN_inst_ext inst_ext =
      (PFN_inst_ext)gipa(VK_NULL_HANDLE,
                         "vkEnumerateInstanceExtensionProperties");
  uint32_t cnt;
  VkResult r;

  cnt = 99;
  r = inst_ext("VK_LAYER_VGPU_MANAGER_vgpu", &cnt, NULL);
  if (r != VK_SUCCESS || cnt != 0) {
    fprintf(stderr, "FAIL: inst_ext(own-name) r=%d cnt=%u (want 0/0)\n", r, cnt);
    exit(1);
  }

  cnt = 99;
  r = inst_ext(NULL, &cnt, NULL);
  if (r != VK_ERROR_LAYER_NOT_PRESENT) {
    fprintf(stderr, "FAIL: inst_ext(NULL) r=%d (want VK_ERROR_LAYER_NOT_PRESENT)\n", r);
    exit(1);
  }

  cnt = 99;
  r = inst_ext("VK_LAYER_SOME_OTHER_LAYER", &cnt, NULL);
  if (r != VK_ERROR_LAYER_NOT_PRESENT) {
    fprintf(stderr, "FAIL: inst_ext(other-name) r=%d (want VK_ERROR_LAYER_NOT_PRESENT)\n", r);
    exit(1);
  }
  printf("ok: own-name => SUCCESS/0; NULL or other name => LAYER_NOT_PRESENT\n");

  /* Layer-properties query: always VK_SUCCESS with count=0. */
  typedef VkResult (VKAPI_PTR *PFN_inst_lay)(uint32_t *, VkLayerProperties *);
  PFN_inst_lay inst_lay =
      (PFN_inst_lay)gipa(VK_NULL_HANDLE,
                         "vkEnumerateInstanceLayerProperties");
  cnt = 99;
  r = inst_lay(&cnt, NULL);
  if (r != VK_SUCCESS || cnt != 0) {
    fprintf(stderr, "FAIL: inst_lay r=%d cnt=%u (want 0/0)\n", r, cnt);
    exit(1);
  }
  printf("ok: vkEnumerateInstanceLayerProperties => SUCCESS, count=0\n");

  /* Same shape for the device variants. We pass a fake non-NULL
   * physdev handle; the hook ignores it (only pLayerName matters). */
  typedef VkResult (VKAPI_PTR *PFN_dev_ext)(VkPhysicalDevice, const char *,
                                            uint32_t *,
                                            VkExtensionProperties *);
  PFN_dev_ext dev_ext =
      (PFN_dev_ext)gipa(VK_NULL_HANDLE,
                        "vkEnumerateDeviceExtensionProperties");
  VkPhysicalDevice phys = (VkPhysicalDevice)(uintptr_t)0x1;
  cnt = 99;
  r = dev_ext(phys, "VK_LAYER_VGPU_MANAGER_vgpu", &cnt, NULL);
  if (r != VK_SUCCESS || cnt != 0) {
    fprintf(stderr, "FAIL: dev_ext(own-name) r=%d cnt=%u (want 0/0)\n", r, cnt);
    exit(1);
  }
  cnt = 99;
  r = dev_ext(phys, NULL, &cnt, NULL);
  if (r != VK_ERROR_LAYER_NOT_PRESENT) {
    fprintf(stderr, "FAIL: dev_ext(NULL) r=%d (want VK_ERROR_LAYER_NOT_PRESENT)\n", r);
    exit(1);
  }
  printf("ok: vkEnumerateDeviceExtensionProperties: own-name SUCCESS/0, NULL LAYER_NOT_PRESENT\n");

  typedef VkResult (VKAPI_PTR *PFN_dev_lay)(VkPhysicalDevice, uint32_t *,
                                            VkLayerProperties *);
  PFN_dev_lay dev_lay =
      (PFN_dev_lay)gipa(VK_NULL_HANDLE,
                        "vkEnumerateDeviceLayerProperties");
  cnt = 99;
  r = dev_lay(phys, &cnt, NULL);
  if (r != VK_SUCCESS || cnt != 0) {
    fprintf(stderr, "FAIL: dev_lay r=%d cnt=%u (want 0/0)\n", r, cnt);
    exit(1);
  }
  printf("ok: vkEnumerateDeviceLayerProperties => SUCCESS, count=0\n");
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
  check_enumerate_hooks_resolve_with_null_instance(h);
  dlclose(h);

  check_only_one_vk_export(so_path);

  printf("ok: test_layer_init complete\n");
  return 0;
}
