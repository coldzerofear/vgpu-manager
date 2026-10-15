# Copyright 2024-2026 coldzerofear
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Image URL to use all building/pushing image targets
TAG ?= latest
REGISTRY ?= ghcr.io/coldzerofear
IMG = $(REGISTRY)/vgpu-manager:$(TAG)
DRA_IMG = $(REGISTRY)/vgpu-manager-dra-driver:$(TAG)
BASE_IMG = $(REGISTRY)/vgpu-manager-base:$(TAG)
VERSION ?= $(shell cat VERSION)

# The architecture the images are built for. Dockerfile.base downloads a Go
# toolchain and sets GOARCH from these, and a plain `docker build` does not
# reliably inject the automatic platform args into a stage that declares them
# with a default -- so they are passed explicitly. Native builds on an arm64
# machine (a runner, a workstation) would otherwise fetch an amd64 toolchain.
TARGETOS   ?= $(GOOS)
TARGETARCH ?= $(shell go env GOARCH 2>/dev/null || dpkg --print-architecture 2>/dev/null || echo amd64)

CUDA_BASE_IMAGE ?= nvidia/cuda:13.4.2-devel-rockylinux8

# Git info
GIT_BRANCH := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "Not a git repo")
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo "Not a git tree")
BUILD_DATE := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
ifeq ($(strip $(shell git status --porcelain 2>/dev/null)),)
  GIT_TREE_STATE?=clean
else
  GIT_TREE_STATE?=dirty
endif

# The toolchain the images are built with, taken from go.mod's `toolchain`
# directive and falling back to the `go` directive when there is none.
#
# These are not the same thing, and reading the second as the first is what
# shipped every image built with Go 1.26.0 - the first patch release of that
# minor. `go 1.26.0` states the language version the source needs; it says
# nothing about which compiler to use. govulncheck found 41 reachable
# vulnerabilities in those binaries, 39 of them standard-library fixes that
# had already shipped in 1.26.1 through 1.26.9 (net/http, crypto/x509,
# html/template, os). The toolchain directive is the one Go itself reads, so
# `go build` here, setup-go in CI and the image build now agree on it.
#
# To take a Go patch release: `go mod edit -toolchain=go1.26.N`.
GOLANG_VERSION := $(shell grep -E '^toolchain go[0-9]' go.mod | grep -oE '[0-9][0-9.]*' \
                          || grep -E '^go [0-9]' go.mod | grep -oE '[0-9][0-9.]*')

# The language version, kept separate so the guard below can tell them apart.
GOLANG_LANG_VERSION := $(shell grep -E '^go [0-9]' go.mod | grep -oE '[0-9][0-9.]*')

# A toolchain from a different minor than the source targets builds the images
# with something the code was never checked against. The nightly job's image
# drifted exactly this way while a comment asked someone to keep it in step.
ifneq ($(shell echo $(GOLANG_VERSION) | cut -d. -f1,2),$(shell echo $(GOLANG_LANG_VERSION) | cut -d. -f1,2))
  $(error GOLANG_VERSION=$(GOLANG_VERSION) is not a patch of go.mod's go $(GOLANG_LANG_VERSION))
endif
TOOLKIT_CONTAINER_IMAGE := $(shell chmod +x ./scripts/toolkit-container-image.sh && ./scripts/toolkit-container-image.sh)

# nvVersion represents the version of dra-driver-nvidia-gpu that this project is based on
NVVERSION := v0.5.1

print-%:
	@echo $($*)
