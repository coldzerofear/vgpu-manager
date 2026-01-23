# Copyright 2026 The vGPU-Manager Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Image URL to use all building/pushing image targets
TAG ?= latest
REGISTRY ?= coldzerofear
IMG = $(REGISTRY)/vgpu-manager:$(TAG)
DRA_IMG = $(REGISTRY)/vgpu-manager-dra:$(TAG)
BASE_IMG = $(REGISTRY)/vgpu-manager-base:$(TAG)
APT_MIRROR ?= https://mirrors.aliyun.com
VERSION ?= $(shell cat VERSION)

# Git info
GIT_BRANCH := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "Not a git repo")
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo "Not a git tree")
BUILD_DATE := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
ifeq ($(strip $(shell git status --porcelain 2>/dev/null)),)
  GIT_TREE_STATE?=clean
else
  GIT_TREE_STATE?=dirty
endif

GOLANG_VERSION := $(shell grep -E '^go [0-9]' go.mod | grep -oE '[0-9][0-9.]*')
TOOLKIT_CONTAINER_IMAGE := $(shell chmod +x ./scripts/toolkit-container-image.sh && ./scripts/toolkit-container-image.sh)

print-%:
	@echo $($*)