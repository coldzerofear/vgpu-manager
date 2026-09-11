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
IMG             ?= coldzerofear/vgpu-control:dev
CUDA_BASE_IMAGE ?= nvidia/cuda:12.9.1-cudnn-devel-ubuntu20.04
APT_MIRROR      ?= https://mirrors.aliyun.com
TEST_DEVICE_ID  ?= 0
TEST_TIMEOUT    ?= 120

# Derived from the sources rather than hand-listed. The GPU side already
# discovers its binaries by glob; the list here used to be written out by hand,
# which is how test_dlsym_entry ended up compiled by build-tests and run by
# nothing. Helper sources in test/nogpu/ are not named test_*.c, so they stay
# out of this on their own.
NOGPU_TESTS     := $(patsubst %.c,%,$(notdir $(wildcard $(LIBRARY_DIR)test/nogpu/test_*.c)))

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

.PHONY: clean
clean: ## Remove the build tree.
	rm -rf $(BUILD_DIR)

##@ Docker

.PHONY: docker-build
docker-build: ## Build libvgpu-control.so inside $(IMG) using the local Dockerfile.
	$(DOCKER) build --build-arg NVIDIA_CUDA_IMAGE=$(CUDA_BASE_IMAGE) --build-arg APT_MIRROR=$(APT_MIRROR) -t $(IMG) $(LIBRARY_DIR)

.PHONY: build-in-docker
build-in-docker: ## Build the .so in an ephemeral CUDA container (no local toolkit required).
	$(DOCKER) run -i --rm \
	    -v $(LIBRARY_DIR):/vgpu-library \
	    -w /vgpu-library \
	    -e DEBIAN_FRONTEND=noninteractive \
	    $(CUDA_BASE_IMAGE) \
	    sh -c "apt-get -y update && apt-get -y install make cmake $(PYTHON) && bash ./build.sh"

##@ Checks (no GPU needed)

.PHONY: check
check: check-hook-consistency check-struct-layout check-session-paths ## Run every static check.

.PHONY: check-hook-consistency
check-hook-consistency: ## Verify dispatch table / enum / wrapper alignment + ABI-conflict coverage.
	$(PYTHON) $(LIBRARY_DIR)hack/check_cuda_hook_consistency.py

.PHONY: check-struct-layout
check-struct-layout: ## Compare cuda-subset.h struct layouts vs real <cuda.h> on this host.
	$(PYTHON) $(LIBRARY_DIR)hack/check_struct_layout.py --cuda-home $(CUDA_HOME)

.PHONY: check-session-paths
check-session-paths: ## Reject compile-time paths that bypass session_path().
	$(BASH) $(LIBRARY_DIR)hack/check_session_paths.sh

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
	    -DBUILD_SM_NODE_TESTS=ON \
	    -DTEST_DEVICE_ID=$(TEST_DEVICE_ID) \
	    $(CMAKE_EXTRA_ARGS) \
	    $(LIBRARY_DIR)
	$(MAKE) -C $(BUILD_DIR) -j$(J)

# Same PASS/SKIP/FAIL vocabulary as test/run_all_tests.sh, 77 included: a test
# whose preconditions were absent must not report success. No LD_PRELOAD here --
# these need no driver, and test_dlsym_entry installs its own before re-execing.
# An empty list is a failure, not a pass: it means the glob stopped matching.
.PHONY: test-nogpu
test-nogpu: build-tests ## Run the GPU-less checks (no GPU, no CUDA needed).
	@fail=0; ran=0; \
	for t in $(NOGPU_TESTS); do \
	  bin=$(BUILD_DIR)/$$t; \
	  printf '=== %-36s ' "$$t"; \
	  if [[ ! -x $$bin ]]; then echo 'FAIL (not built)'; fail=1; continue; fi; \
	  ran=$$((ran + 1)); log=$$(mktemp); rc=0; \
	  timeout $(TEST_TIMEOUT) $$bin >$$log 2>&1 || rc=$$?; \
	  case $$rc in \
	    0)  echo 'PASS' ;; \
	    77) echo 'SKIP'; sed 's/^/  /' $$log ;; \
	    *)  echo "FAIL (rc=$$rc)"; sed 's/^/  /' $$log; fail=1 ;; \
	  esac; \
	  rm -f $$log; \
	done; \
	if [[ $$ran -eq 0 ]]; then echo 'FAIL: no GPU-less tests were run'; fail=1; fi; \
	exit $$fail

.PHONY: test
# Runs the GPU-less checks first, deliberately. They are seconds long, need no
# driver, and cover what reading the code cannot: cross-process CAS, the refill
# election, sampling ownership and the fork hazard for sm_node; slot uniqueness
# under concurrent registration, byte-range lock exclusion at the real computed
# offsets, and record-preserving compaction for vmem_node. Running them here is
# also the only thing that makes them run at all -- a target nobody invokes is a
# test that quietly rots, which is the failure mode this suite exists to avoid.
#
# Depends on test-nogpu rather than repeating it: two prerequisites here would
# be eligible to run in parallel under -j, and the tests would race the build.
test: test-nogpu ## Build then run all tests with LD_PRELOAD of libvgpu-control.so.
	TEST_DEVICE_ID=$(TEST_DEVICE_ID) \
	    $(BASH) $(BUILD_DIR)/test/run_tests_with_env.sh $(BUILD_DIR)/test $(SO_FILE)

.PHONY: test-cuda13-abi
test-cuda13-abi: build-tests ## Run just the CUDA 13 ABI-conflict smoke test.
	LD_PRELOAD=$(SO_FILE) TEST_DEVICE_ID=$(TEST_DEVICE_ID) \
	    $(BUILD_DIR)/test/test_cuda13_abi
