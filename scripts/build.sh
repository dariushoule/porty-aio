#!/usr/bin/env bash
# build.sh: host wrapper (Linux/macOS). Drives the Dockerized cross-compile.
#
# No local Go toolchain required; only Docker. Artifacts land in ./dist.
#
# Env:
#   VERSION   version string to stamp (default: dev)
#   TARGETS   override the GOOS/GOARCH matrix (space-separated "os/arch")
#
# Examples:
#   ./scripts/build.sh
#   VERSION=0.1.0 ./scripts/build.sh
#   TARGETS="linux/amd64 windows/amd64" ./scripts/build.sh
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="porty-aio-builder"
VERSION="${VERSION:-dev}"
TARGETS="${TARGETS:-}"

echo "[*] Building builder image ($IMAGE)..."
docker build -t "$IMAGE" .

echo "[*] Cross-compiling (VERSION=$VERSION)..."
docker run --rm \
	-e VERSION="$VERSION" \
	-e TARGETS="$TARGETS" \
	-v "$PWD":/src \
	-v porty-aio-gocache:/root/.cache/go-build \
	-v porty-aio-gomod:/go/pkg/mod \
	-w /src \
	"$IMAGE" sh scripts/build-matrix.sh

echo "[*] Done. Binaries in ./dist"
