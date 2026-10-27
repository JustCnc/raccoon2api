#!/usr/bin/env bash
# Cross-compile the plugin as a c-shared library inside the official Go image,
# so the host needs no Go toolchain or C compiler.
#
#   ./build.sh                # all targets
#   ./build.sh linux/amd64    # one target
#
# Output: dist/plugins/<goos>/<goarch>/raccoon2api.{so,dylib,dll}
set -euo pipefail

cd "$(dirname "$0")"

NAME=raccoon2api
GO_IMAGE=${GO_IMAGE:-golang:1.26-bookworm}
# Prebuilt image with every cross toolchain (see build.Dockerfile). Building it
# once makes later builds take seconds and keeps apt off the critical path.
BUILD_IMAGE=${BUILD_IMAGE:-raccoon2api-build:latest}
OUT_DIR=${OUT_DIR:-$(pwd)/dist/plugins}
GOMODCACHE=${GOMODCACHE:-$HOME/go/pkg/mod}
TARGETS=${*:-"linux/amd64 linux/arm64 windows/amd64"}

mkdir -p "$GOMODCACHE" "$OUT_DIR"

# Container path conversion for Git Bash / MSYS on Windows.
to_container_path() {
  local p="$1"
  if [[ "$p" =~ ^[A-Za-z]:[/\\] ]]; then
    p="/$(echo "${p:0:1}" | tr 'A-Z' 'a-z')${p:2}"
    p="${p//\\//}"
  fi
  printf '%s' "$p"
}

SRC_PATH=$(to_container_path "$(pwd)")
OUT_PATH=$(to_container_path "$OUT_DIR")
CACHE_PATH=$(to_container_path "$GOMODCACHE")

ensure_build_image() {
  if docker image inspect "$BUILD_IMAGE" >/dev/null 2>&1; then
    GO_IMAGE="$BUILD_IMAGE"
    return 0
  fi
  echo "==> building $BUILD_IMAGE (one-time, installs cross toolchains)"
  if docker build -q -f build.Dockerfile -t "$BUILD_IMAGE" . >/dev/null 2>&1; then
    GO_IMAGE="$BUILD_IMAGE"
    return 0
  fi
  echo "    image build failed, falling back to $GO_IMAGE with per-build apt" >&2
  return 0
}

run_target() {
  local target="$1"
  local goos="${target%%/*}"
  local goarch="${target##*/}"
  local ext="so"
  local cc="gcc"
  local pkg=""
  case "$goos" in
    linux)
      ext="so"
      if [[ "$goarch" == "arm64" ]]; then
        cc="aarch64-linux-gnu-gcc"
        pkg="gcc-aarch64-linux-gnu libc6-dev-arm64-cross"
      fi
      ;;
    windows)
      ext="dll"
      cc="x86_64-w64-mingw32-gcc"
      pkg="gcc-mingw-w64-x86-64"
      ;;
    darwin)
      ext="dylib"
      ;;
    *)
      echo "unsupported target: $target" >&2
      return 1
      ;;
  esac

  if [[ "$GO_IMAGE" == "$BUILD_IMAGE" ]]; then
    pkg=""
  fi

  echo "==> $target"
  MSYS_NO_PATHCONV=1 docker run --rm \
    -v "${SRC_PATH}:/src:ro" \
    -v "${OUT_PATH}:/out" \
    -v "${CACHE_PATH}:/gomodcache" \
    -e GOMODCACHE=/gomodcache \
    -e GOFLAGS=-mod=mod \
    -e CGO_ENABLED=1 \
    -e GOOS="$goos" \
    -e GOARCH="$goarch" \
    -e CC="$cc" \
    ${BUILD_PROXY:+-e HTTP_PROXY="$BUILD_PROXY" -e HTTPS_PROXY="$BUILD_PROXY"} \
    -w /src \
    "$GO_IMAGE" \
    bash -c "set -e; ${BUILD_PROXY:+echo 'Acquire::http::Proxy \"$BUILD_PROXY\";' > /etc/apt/apt.conf.d/99proxy;} \
      ${pkg:+apt-get update >/dev/null && apt-get install -y --no-install-recommends $pkg >/dev/null;} \
      mkdir -p /out/$goos/$goarch; \
      go build -buildmode=c-shared -trimpath -ldflags='-s -w' -o /out/$goos/$goarch/$NAME.$ext .; \
      rm -f /out/$goos/$goarch/$NAME.h"
}

ensure_build_image

for target in $TARGETS; do
  run_target "$target"
done

echo
echo "artifacts:"
find "$OUT_DIR" -type f \( -name "*.so" -o -name "*.dll" -o -name "*.dylib" \) -printf '  %p (%s bytes)\n'
