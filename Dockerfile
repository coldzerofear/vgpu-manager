FROM nvidia/cuda:12.4.1-devel-ubuntu20.04 as builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG APT_MIRROR

ARG GOLANG_VERSION=1.23.1

RUN echo "Asia/Shanghai" > /etc/timezone && \
    ln -fs /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN if [ -n "$APT_MIRROR" ]; then sed -i "s@http://archive.ubuntu.com@${APT_MIRROR}@g" /etc/apt/sources.list ; fi && \
    apt-get -y update && apt-get -y install --no-install-recommends make cmake g++ ca-certificates wget && \
    rm -rf /var/lib/apt/lists/*

RUN wget -nv -O - https://golang.google.cn/dl/go${GOLANG_VERSION}.${TARGETOS}-${TARGETARCH}.tar.gz \
    | tar -C /usr/local -xz

# Compile vgpu driver library files
WORKDIR /vgpu-controller

COPY library/ .

RUN chmod +x build.sh && ./build.sh

# Compile the vgpu manager binary file
WORKDIR /go/src/vgpu-manager

ENV GOPATH=/go
ENV PATH=$GOPATH/bin:/usr/local/go/bin:$PATH
ENV GO111MODULE=on
ENV GOPROXY=https://goproxy.cn,direct

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

ARG GIT_BRANCH
ARG GIT_COMMIT
ARG GIT_TREE_STATE
ARG BUILD_DATE

# Copy the go source
COPY cmd cmd/
COPY pkg pkg/

RUN	CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_CFLAGS="-D_GNU_SOURCE -D_FORTIFY_SOURCE=2 -O2 -ftrapv" \
        CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' go build -ldflags=" \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitBranch=${GIT_BRANCH} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitCommit=${GIT_COMMIT}  \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitTreeState=${GIT_TREE_STATE} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.buildDate=${BUILD_DATE}" \
        -o bin/scheduler cmd/scheduler/*.go && \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_CFLAGS="-D_GNU_SOURCE -D_FORTIFY_SOURCE=2 -O2 -ftrapv" \
        CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' go build -ldflags=" \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitBranch=${GIT_BRANCH} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitCommit=${GIT_COMMIT} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitTreeState=${GIT_TREE_STATE} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.buildDate=${BUILD_DATE}" \
        -o bin/deviceplugin cmd/device-plugin/*.go && \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_CFLAGS="-D_GNU_SOURCE -D_FORTIFY_SOURCE=2 -O2 -ftrapv" \
        CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' go build -ldflags=" \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitBranch=${GIT_BRANCH} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitCommit=${GIT_COMMIT} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitTreeState=${GIT_TREE_STATE} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.buildDate=${BUILD_DATE}" \
        -o bin/monitor cmd/monitor/*.go && \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_CFLAGS="-D_GNU_SOURCE -D_FORTIFY_SOURCE=2 -O2 -ftrapv" \
        CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' go build -ldflags=" \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitBranch=${GIT_BRANCH} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitCommit=${GIT_COMMIT}  \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.gitTreeState=${GIT_TREE_STATE} \
        -X github.com/coldzerofear/vgpu-manager/pkg/version.buildDate=${BUILD_DATE}" \
        -o bin/webhook cmd/webhook/*.go

FROM quay.io/jitesoft/ubuntu:20.04

ENV NVIDIA_DISABLE_REQUIRE="true"

WORKDIR /
COPY --from=builder /go/src/vgpu-manager/bin/scheduler /usr/bin/scheduler
COPY --from=builder /go/src/vgpu-manager/bin/deviceplugin /usr/bin/deviceplugin
COPY --from=builder /go/src/vgpu-manager/bin/monitor /usr/bin/monitor
COPY --from=builder /go/src/vgpu-manager/bin/webhook /usr/bin/webhook

COPY scripts scripts/
RUN chmod +x /scripts/* && mkdir -p /installed
COPY --from=builder /vgpu-controller/build/libvgpu-control.so /installed/libvgpu-control.so
COPY --from=builder /vgpu-controller/build/mem_occupy_tool /installed/mem_occupy_tool
COPY --from=builder /vgpu-controller/build/mem_managed_tool /installed/mem_managed_tool
COPY --from=builder /vgpu-controller/build/mem_view_tool /installed/mem_view_tool
COPY --from=builder /vgpu-controller/build/extract_container_pids /installed/extract_container_pids

RUN echo '/etc/vgpu-manager/driver/libvgpu-control.so' > /installed/ld.so.preload