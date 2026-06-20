#!/usr/bin/env bash
# test.sh: host wrapper (Linux/macOS). Runs the test suite in the build
# container, so no local Go toolchain is required. Extra args are passed through
# to `go test` (e.g. ./scripts/test.sh -run Scan -v).
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="porty-aio-builder"

echo "[*] Building builder image ($IMAGE)..."
docker build -t "$IMAGE" .

echo "[*] Running tests..."
docker run --rm \
	-v "$PWD":/src \
	-v porty-aio-gocache:/root/.cache/go-build \
	-v porty-aio-gomod:/go/pkg/mod \
	-w /src \
	"$IMAGE" go test ./... -count=1 "$@"
