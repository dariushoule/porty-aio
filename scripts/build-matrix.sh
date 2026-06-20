#!/bin/sh
# build-matrix.sh: runs INSIDE the build container.
#
# Cross-compiles porty-aio for every target in $TARGETS into ./dist as fully
# static, dependency-free binaries (CGO_ENABLED=0), then UPX-compresses them to
# minimize size. This is the single source of build logic; the host-side
# build.sh / build.ps1 wrappers only invoke it.
#
# Env:
#   VERSION   version string stamped into the binary (default: dev)
#   TARGETS   space-separated GOOS/GOARCH list (default: full matrix below)
#   UPX       1 to compress (default), 0 to skip
set -eu

PKG="./cmd/porty-aio"
OUT="dist"
VERSION="${VERSION:-dev}"
UPX="${UPX:-1}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64 linux/386 linux/arm windows/amd64 windows/386 windows/arm64 darwin/amd64 darwin/arm64}"

# upx_supported reports whether UPX can safely pack a given GOOS/GOARCH.
# darwin is excluded because macOS codesigning rejects packed binaries;
# windows/arm64 is not supported by UPX.
upx_supported() {
	case "$1/$2" in
	darwin/*) return 1 ;;
	windows/arm64) return 1 ;;
	*) return 0 ;;
	esac
}

# Start clean so reruns are deterministic (avoids re-packing stale binaries).
rm -rf "$OUT"
mkdir -p "$OUT"
echo "porty-aio build :: version=${VERSION} upx=${UPX}"
echo "targets: ${TARGETS}"
echo

for t in $TARGETS; do
	GOOS="${t%/*}"
	GOARCH="${t#*/}"
	name="porty-aio_${GOOS}_${GOARCH}"
	[ "$GOOS" = "windows" ] && name="${name}.exe"

	echo ">> ${GOOS}/${GOARCH} -> ${OUT}/${name}"
	# -buildvcs=false: the version is stamped via -ldflags below, and VCS
	# stamping fails when building in a container against a host-owned .git.
	CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" GOARM=7 \
		go build -trimpath -buildvcs=false \
		-ldflags "-s -w -X main.version=${VERSION}" \
		-o "${OUT}/${name}" "$PKG"

	if [ "$UPX" = "1" ]; then
		if upx_supported "$GOOS" "$GOARCH"; then
			upx -q --best --lzma "${OUT}/${name}" >/dev/null
		else
			echo "   (upx skipped: unsupported for ${GOOS}/${GOARCH})"
		fi
	fi
done

echo
echo "generating SHA256SUMS.txt..."
( cd "$OUT" && sha256sum porty-aio_* > SHA256SUMS.txt )

echo
echo "done. artifacts in ./${OUT}:"
ls -lh "$OUT"
