# vgpu-manager library (libvgpu-control.so) build entry.
#
# Thin wrapper around cmake + the hack/ scripts. Everything below is a
# convenience target for things you would otherwise do by hand:
#
#   make build                   build the .so
#   make docker-build            build the .so inside a CUDA docker image
#   make build-in-docker         same, but via an ephemeral run (no Dockerfile)
#   make check                   run all static checks (no GPU needed)
#   make check-exports           verify ABI-conflict symbol exports on the built .so
#   make test                    build and run the smoke tests under test/
#   make help                    show this listing
#
.DEFAULT_GOAL := help

LIBRARY_DIR     := $(dir $(abspath $(firstword $(MAKEFILE_LIST))))
BUILD_DIR       ?= $(LIBRARY_DIR)build
CUDA_HOME       ?= /usr/local/cuda
CMAKE           ?= cmake
DOCKER          ?= docker
PYTHON          ?= python3
BASH            ?= bash
CMAKE_BUILD_TYPE ?= Release
J               ?= $(shell nproc 2>/dev/null | awk '{print int(($$0+1)/2)}')
IMG             ?= vgpu-control:dev
CUDA_BASE_IMAGE ?= nvidia/cuda:12.9.1-cudnn-devel-ubuntu20.04
TEST_DEVICE_ID  ?= 0

SO_FILE         := $(BUILD_DIR)/libvgpu-control.so

SHELL := /usr/bin/env bash
.SHELLFLAGS := -o pipefail -ec

# -------- Help ------------------------------------------------------------
.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	      /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
	      /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: configure
configure: ## Run cmake to (re)configure the build tree.
	@mkdir -p $(BUILD_DIR)
	cd $(BUILD_DIR) && CUDA_HOME=$(CUDA_HOME) $(CMAKE) \
	    -DCMAKE_BUILD_TYPE=$(CMAKE_BUILD_TYPE) \
	    $(CMAKE_EXTRA_ARGS) \
	    $(LIBRARY_DIR)

.PHONY: build
build: configure ## Build libvgpu-control.so.
	$(MAKE) -C $(BUILD_DIR) -j$(J) vgpu-control

.PHONY: tools
tools: configure ## Build all aux tools (mem_occupy_tool, mem_view_tool, etc.).
	$(MAKE) -C $(BUILD_DIR) -j$(J)

.PHONY: all
all: build tools ## Build the .so and every aux tool.

.PHONY: build-vulkan
build-vulkan: ## Build libvgpu-control.so with the Vulkan implicit layer module.
	@mkdir -p $(BUILD_DIR)
	cd $(BUILD_DIR) && CUDA_HOME=$(CUDA_HOME) $(CMAKE) \
	    -DCMAKE_BUILD_TYPE=$(CMAKE_BUILD_TYPE) \
	    -DBUILD_VULKAN_LAYER=ON \
	    $(CMAKE_EXTRA_ARGS) \
	    $(LIBRARY_DIR)
	$(MAKE) -C $(BUILD_DIR) -j$(J) vgpu-control

.PHONY: clean
clean: ## Remove the build tree.
	rm -rf $(BUILD_DIR)

##@ Docker

.PHONY: docker-build
docker-build: ## Build libvgpu-control.so inside $(IMG) using the local Dockerfile.
	$(DOCKER) build -t $(IMG) $(LIBRARY_DIR)

.PHONY: build-in-docker
build-in-docker: ## Build the .so in an ephemeral CUDA container (no local toolkit required).
	$(DOCKER) run -i --rm \
	    -v $(LIBRARY_DIR):/vgpu-library \
	    -w /vgpu-library \
	    -e DEBIAN_FRONTEND=noninteractive \
	    $(CUDA_BASE_IMAGE) \
	    sh -c "apt-get -y update && apt-get -y install make cmake $(PYTHON) && sh ./build.sh"

##@ Checks (no GPU needed)

.PHONY: check
check: check-hook-consistency check-struct-layout ## Run every static check.

.PHONY: check-hook-consistency
check-hook-consistency: ## Verify dispatch table / enum / wrapper alignment + ABI-conflict coverage.
	$(PYTHON) $(LIBRARY_DIR)hack/check_cuda_hook_consistency.py

.PHONY: check-struct-layout
check-struct-layout: ## Compare cuda-subset.h struct layouts vs real <cuda.h> on this host.
	$(PYTHON) $(LIBRARY_DIR)hack/check_struct_layout.py --cuda-home $(CUDA_HOME)

.PHONY: check-exports
check-exports: $(SO_FILE) ## Verify the built .so exports both ABI versions of every conflict family.
	$(BASH) $(LIBRARY_DIR)hack/check_exported_symbols.sh $(SO_FILE)

$(SO_FILE): build

.PHONY: check-all
check-all: check check-exports ## Static + post-build symbol-export checks.

##@ Tests (need GPU + NVIDIA driver)

.PHONY: build-tests
build-tests: ## Compile smoke tests under test/.
	@mkdir -p $(BUILD_DIR)
	cd $(BUILD_DIR) && CUDA_HOME=$(CUDA_HOME) $(CMAKE) \
	    -DCMAKE_BUILD_TYPE=$(CMAKE_BUILD_TYPE) \
	    -DBUILD_TESTS=ON \
	    -DTEST_DEVICE_ID=$(TEST_DEVICE_ID) \
	    $(CMAKE_EXTRA_ARGS) \
	    $(LIBRARY_DIR)
	$(MAKE) -C $(BUILD_DIR) -j$(J)

.PHONY: test
test: build-tests ## Build then run all tests with LD_PRELOAD of libvgpu-control.so.
	TEST_DEVICE_ID=$(TEST_DEVICE_ID) \
	    $(BASH) $(BUILD_DIR)/test/run_all_tests.sh $(BUILD_DIR)/test $(SO_FILE)

.PHONY: test-cuda13-abi
test-cuda13-abi: build-tests ## Run just the CUDA 13 ABI-conflict smoke test.
	LD_PRELOAD=$(SO_FILE) TEST_DEVICE_ID=$(TEST_DEVICE_ID) \
	    $(BUILD_DIR)/test/test_cuda13_abi

##@ Vulkan unit tests (no GPU / no CUDA needed)

.PHONY: test-vulkan-unit
test-vulkan-unit: ## Build + run the Vulkan layer unit tests under test/vulkan/.
	@mkdir -p $(BUILD_DIR)
	cd $(BUILD_DIR) && CUDA_HOME=$(CUDA_HOME) $(CMAKE) \
	    -DCMAKE_BUILD_TYPE=$(CMAKE_BUILD_TYPE) \
	    -DBUILD_VULKAN_LAYER=ON \
	    -DBUILD_TESTS=ON \
	    $(CMAKE_EXTRA_ARGS) \
	    $(LIBRARY_DIR)
	$(MAKE) -C $(BUILD_DIR) -j$(J) vgpu-control
	$(MAKE) -C $(BUILD_DIR) -j$(J) test_dispatch test_queue_index \
	    test_physdev_index test_memprops_clamp test_alloc_budget \
	    test_alloc_import test_submit_throttle test_layer_init \
	    vgpu_vk_copy_runner
	$(BASH) $(BUILD_DIR)/test/vulkan/run_unit_tests.sh \
	    $(BUILD_DIR)/test/vulkan $(SO_FILE)

.PHONY: check-vulkan-layer
check-vulkan-layer: $(SO_FILE) ## Sanity-check the built .so for Vulkan-layer hygiene.
	$(BASH) $(LIBRARY_DIR)hack/check_vulkan_layer.sh \
	    $(SO_FILE) $(LIBRARY_DIR)deploy/vgpu_manager_implicit_layer.json
