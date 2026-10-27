# Build image with every cross toolchain preinstalled, so ./build.sh never has
# to apt-get at build time (that step is the flaky one on slow networks).
#
#   docker build -f build.Dockerfile -t raccoon2api-build:latest .
#
# build.sh builds this image automatically when it is missing.
FROM golang:1.26-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        gcc \
        gcc-aarch64-linux-gnu \
        libc6-dev-arm64-cross \
        gcc-mingw-w64-x86-64 \
    && rm -rf /var/lib/apt/lists/*

ENV CGO_ENABLED=1
