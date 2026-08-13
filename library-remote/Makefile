# vgpu-manager library (libvgpu-remote.so) build entry.
#
# Thin wrapper around cmake + the hack/ scripts. Everything below is a
# convenience target for things you would otherwise do by hand:
#
#   make build                   build the .so
#   make docker-build            build the .so inside a CUDA docker image
#   make build-in-docker         same, but via an ephemeral run (no Dockerfile)
#   make help                    show this listing
#
.DEFAULT_GOAL := help

LIBRARY_DIR     := $(dir $(abspath $(firstword $(MAKEFILE_LIST))))
HACK_DIR        ?= $(LIBRARY_DIR)../library/hack
BUILD_DIR       ?= $(LIBRARY_DIR)build
SO_FILE         ?= $(BUILD_DIR)/libvgpu-remote.so
CUDA_HOME       ?= /usr/local/cuda
CMAKE           ?= cmake
DOCKER          ?= docker
PYTHON          ?= python3
BASH            ?= bash
CMAKE_BUILD_TYPE ?= Release
J               ?= $(shell nproc 2>/dev/null | awk '{print int(($$0+1)/2)}')
IMG             ?= coldzerofear/vgpu-remote:dev
CUDA_BASE_IMAGE ?= nvidia/cuda:12.9.1-cudnn-devel-ubuntu20.04
APT_MIRROR      ?= https://mirrors.aliyun.com

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
build: configure ## Build libvgpu-remote.so.
	$(MAKE) -C $(BUILD_DIR) -j$(J) vgpu-remote

.PHONY: clean
clean: ## Remove the build tree.
	rm -rf $(BUILD_DIR)

##@ Checks (no GPU needed)

# The checkers live in ../library/hack and take the tree to check as an
# argument -- one copy validates both trees, so they cannot drift apart.
.PHONY: check
check: check-hook-consistency check-struct-layout check-no-constructors ## Run every static check.

.PHONY: check-hook-consistency
check-hook-consistency: ## Verify dispatch table / enum / wrapper alignment + ABI-conflict coverage.
	$(PYTHON) $(HACK_DIR)/check_cuda_hook_consistency.py --root $(LIBRARY_DIR)

.PHONY: check-struct-layout
check-struct-layout: ## Compare cuda-subset.h struct layouts vs real <cuda.h> on this host.
	$(PYTHON) $(HACK_DIR)/check_struct_layout.py --root $(LIBRARY_DIR) --cuda-home $(CUDA_HOME)

.PHONY: check-no-constructors
check-no-constructors: ## Reject __attribute__((constructor/destructor)) in src/.
	$(BASH) $(HACK_DIR)/check_no_constructors.sh $(LIBRARY_DIR)

.PHONY: check-exports
check-exports: $(SO_FILE) ## Verify the built .so exports the documented surface + the provider entry point.
	$(BASH) $(HACK_DIR)/check_exported_symbols.sh $(SO_FILE)

$(SO_FILE): build

.PHONY: check-all
check-all: check check-exports ## Static + post-build symbol-export checks.

##@ Tools & tests (no GPU needed)

# Compiled with plain cc rather than through cmake: both link a handful of
# driver-free sources, and needing a CUDA toolkit to build a config-file writer
# would defeat the point of having one.
SESSION_SRCS := $(LIBRARY_DIR)src/session.c $(LIBRARY_DIR)src/config_io.c \
                $(LIBRARY_DIR)src/util.c $(LIBRARY_DIR)src/lock.c $(LIBRARY_DIR)src/metrics.c
CFLAGS_NOGPU := -Wall -Wshadow -I$(LIBRARY_DIR) -I$(LIBRARY_DIR)include -D_GNU_SOURCE

.PHONY: session-cli
session-cli: ## Build vgpu-session-config (writes a session quota region by hand).
	@mkdir -p $(BUILD_DIR)
	$(CC) -o $(BUILD_DIR)/vgpu-session-config $(CFLAGS_NOGPU) \
	    $(LIBRARY_DIR)tools/session_config.c $(SESSION_SRCS)

# test_sm_node_shared / test_vmem_region_concurrency use their own temp files
# rather than the derived paths -- they cover the cross-process semantics of the
# shared regions (CAS refill election, sampling ownership, the fork hazard, slot
# allocation, compaction), which is what the container-wide token bucket rests
# on and what reading the code cannot confirm. Keeping them here also pins the
# region ABI after this tree dropped the AIMD state from sm_node_dev_t.
NOGPU_TESTS := test_session_paths test_sm_node_shared test_vmem_region_concurrency

.PHONY: test-nogpu
test-nogpu: ## Run session path, config round-trip and shared-region checks.
	@mkdir -p $(BUILD_DIR)
	@for t in $(NOGPU_TESTS); do \
	    echo "==> $$t"; \
	    $(CC) -o $(BUILD_DIR)/$$t $(CFLAGS_NOGPU) \
	        $(LIBRARY_DIR)test/nogpu/$$t.c $(SESSION_SRCS) || exit 1; \
	    $(BUILD_DIR)/$$t || exit 1; \
	done

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
	    sh -c "apt-get -y update && apt-get -y install make cmake $(PYTHON) && sh ./build.sh"
