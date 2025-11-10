#!/usr/bin/env sh
# This script builds cross-compiled binaries so CI can publish releases reliably.
set -eu

APP_NAME="bakery"
OUTPUT_DIR="${1:-dist}"

rm -rf "${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"

OS_LIST="linux darwin windows freebsd openbsd"
ARCH_LIST="amd64 arm64"

for os in ${OS_LIST}; do
  for arch in ${ARCH_LIST}; do
    echo "Building ${APP_NAME} for ${os}/${arch}"
    BIN_NAME="${APP_NAME}-${os}-${arch}"
    EXT=""
    if [ "${os}" = "windows" ]; then
      EXT=".exe"
    fi
    GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
      go build -ldflags="-s -w" -o "${OUTPUT_DIR}/${BIN_NAME}${EXT}" ./cmd/server
  done
done
