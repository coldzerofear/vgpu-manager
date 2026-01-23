# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker
GOOS ?= linux

include $(CURDIR)/versions.mk

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
  GOBIN=$(shell go env GOPATH)/bin
else
  GOBIN=$(shell go env GOBIN)
endif

VERSION_PKG = github.com/coldzerofear/vgpu-manager/pkg
CGO_CFLAGS = -D_GNU_SOURCE -D_FORTIFY_SOURCE=2 -O2 -ftrapv
CGO_LDFLAGS_ALLOW = -Wl,--unresolved-symbols=ignore-in-object-files
GO_BUILD_LDFLAGS = -X $(VERSION_PKG)/version.version=${VERSION} \
                   -X $(VERSION_PKG)/version.gitBranch=${GIT_BRANCH} \
                   -X $(VERSION_PKG)/version.gitCommit=${GIT_COMMIT} \
                   -X $(VERSION_PKG)/version.gitTreeState=${GIT_TREE_STATE} \
                   -X $(VERSION_PKG)/version.buildDate=${BUILD_DATE}

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	CGO_ENABLED=1 go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	CGO_ENABLED=1 GOOS=$(GOOS) CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS_ALLOW="$(CGO_LDFLAGS_ALLOW)" \
    go test -ldflags="$(GO_BUILD_LDFLAGS)" ./... -coverprofile cover.out

.PHONY: generate
generate: ## API code generation.
	protoc --go_out=. --go-grpc_out=. pkg/api/registry/api.proto

##@ Build

.PHONY: build
build: fmt vet ## Build binary.
	CGO_ENABLED=1 GOOS=$(GOOS) CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS_ALLOW="$(CGO_LDFLAGS_ALLOW)" \
        go build -ldflags="$(GO_BUILD_LDFLAGS)" -o bin/device-scheduler cmd/device-scheduler/*.go
	CGO_ENABLED=1 GOOS=$(GOOS) CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS_ALLOW="$(CGO_LDFLAGS_ALLOW)" \
        go build -ldflags="$(GO_BUILD_LDFLAGS)" -o bin/device-plugin cmd/device-plugin/*.go
	CGO_ENABLED=1 GOOS=$(GOOS) CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS_ALLOW="$(CGO_LDFLAGS_ALLOW)" \
        go build -ldflags="$(GO_BUILD_LDFLAGS)" -o bin/device-monitor cmd/device-monitor/*.go
	CGO_ENABLED=1 GOOS=$(GOOS) CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS_ALLOW="$(CGO_LDFLAGS_ALLOW)" \
        go build -ldflags="$(GO_BUILD_LDFLAGS)" -o bin/device-webhook cmd/device-webhook/*.go
	CGO_ENABLED=0 GOOS=$(GOOS) go build -ldflags="$(GO_BUILD_LDFLAGS)" -o bin/device-client cmd/device-client/*.go
	CGO_ENABLED=1 GOOS=$(GOOS) CGO_LDFLAGS_ALLOW="$(CGO_LDFLAGS_ALLOW)" \
        go build -ldflags="$(GO_BUILD_LDFLAGS)" -o bin/kubelet-plugin cmd/kubelet-plugin/*.go

.PHONY: docker-build-base
docker-build-base: ## Build base docker image.
	$(CONTAINER_TOOL) build --build-arg GIT_BRANCH="${GIT_BRANCH}" --build-arg APT_MIRROR="${APT_MIRROR}" \
      --build-arg GIT_COMMIT="${GIT_COMMIT}" --build-arg GIT_TREE_STATE="${GIT_TREE_STATE}" \
      --build-arg BUILD_VERSION="${VERSION}" --build-arg BUILD_DATE="${BUILD_DATE}" \
      --build-arg GOLANG_VERSION="${GOLANG_VERSION}" -t "${BASE_IMG}" -f Dockerfile.base .

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image.
	$(CONTAINER_TOOL) build --build-arg BASE_BUILD_IMAGE="${BASE_IMG}" -t "${IMG}" -f Dockerfile .

.PHONY: docker-build-dra
docker-build-dra: ## Build dra driver docker image.
	$(CONTAINER_TOOL) build --build-arg BASE_BUILD_IMAGE="${BASE_IMG}" \
	  --build-arg TOOLKIT_CONTAINER_IMAGE="${TOOLKIT_CONTAINER_IMAGE}" -t "${DRA_IMG}" -f Dockerfile.dra .

.PHONY: docker-build-all
docker-build-all: docker-build-base docker-build docker-build-dra ## Build all docker image.

.PHONY: docker-push
docker-push: ## Push docker image.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-push-dra
docker-push-dra: ## Push dra driver docker image.
	$(CONTAINER_TOOL) push ${DRA_IMG}

.PHONY: docker-push-all
docker-push-all: docker-push docker-push-dra ## Push all docker image.

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed '/--platform=/! s/^[[:space:]]*FROM[[:space:]]/FROM --platform=\$$\{BUILDPLATFORM\} /' Dockerfile > Dockerfile.cross
	sed '/--platform=/! s/^[[:space:]]*FROM[[:space:]]/FROM --platform=\$$\{BUILDPLATFORM\} /' Dockerfile.base > Dockerfile.base.cross
	- $(CONTAINER_TOOL) buildx create --name vgpu-manager-builder
	$(CONTAINER_TOOL) buildx use vgpu-manager-builder
	- $(CONTAINER_TOOL) buildx build --platform=$(PLATFORMS) --build-arg GIT_BRANCH="${GIT_BRANCH}" \
      --build-arg APT_MIRROR="${APT_MIRROR}" --build-arg GIT_COMMIT="${GIT_COMMIT}" --build-arg GIT_TREE_STATE="${GIT_TREE_STATE}" \
	  --build-arg BUILD_VERSION="${VERSION}" --build-arg BUILD_DATE="${BUILD_DATE}" --build-arg GOLANG_VERSION="${GOLANG_VERSION}" \
	  --tag "${BASE_IMG}" -f Dockerfile.base.cross .
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --build-arg BASE_BUILD_IMAGE="${BASE_IMG}" \
	  --tag "${IMG}" -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm vgpu-manager-builder
	rm -f Dockerfile.cross Dockerfile.base.cross