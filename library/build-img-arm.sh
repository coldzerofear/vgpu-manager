#!/bin/bash
set -o errexit
set -o pipefail
set -o nounset
set -o xtrace

ROOT=$(cd $(dirname ${BASH_SOURCE[0]}) && pwd -P)
IMAGE_FILE=${IMAGE_FILE:-"registry.tydic.com/dcloud/vcuda:12.2.0"}

function cleanup() {
    rm -rf ${ROOT}/cuda-control.tar
}

trap cleanup EXIT SIGTERM SIGINT

function build_img() {
    readonly local commit=$(git log --oneline | wc -l | sed -e 's,^[ \t]*,,')
    readonly local version=$(<"${ROOT}/VERSION")

    rm -rf ${ROOT}/build
    mkdir ${ROOT}/build
    git archive -o ${ROOT}/build/cuda-control.tar --format=tar --prefix=cuda-control/ HEAD
    cp ${ROOT}/vcuda.spec ${ROOT}/build
    cp ${ROOT}/Dockerfile ${ROOT}/build
    (
      cd ${ROOT}/build
      docker build ${BUILD_FLAGS:-} -f Dockerfile.arm64 --build-arg version=${version} --build-arg commit=${commit} -t ${IMAGE_FILE} --platform=linux/arm64 --progress=plain  -o type=docker .
    )
}

build_img
