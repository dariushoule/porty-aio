# porty-aio

A compact, dependency-free, cross-platform port scanner that just works anywhere.

One static binary. No libpcap, no Npcap, no glibc pinning, no root. It does TCP
connect scanning, which needs no raw sockets or packet-capture libraries, so a
single `go build` produces a binary you can drop on any Linux, Windows, or macOS
host and run. Port-forwarding and pivot features are planned for later versions
(the "aio" part).

## Why connect-only

A TCP connect scan is the one scan mode that requires zero privileges and zero
native dependencies. Committing to it lets porty-aio be a fully static binary.

## Build (Dockerized, no local Go toolchain needed)

You only need Docker. The Go version is pinned in the `Dockerfile`. Bump that one
line to upgrade.

```sh
# Linux / macOS
./scripts/build.sh

# Windows
.\scripts\build.ps1
```

Binaries are written to `./dist`, one per target. For example
`porty-aio_linux_amd64` and `porty-aio_windows_amd64.exe`.

Binaries are UPX-compressed to minimize size (roughly a third of the unpacked
size). UPX is skipped for macOS (codesigning rejects packed binaries) and
windows/arm64 (not supported by UPX), so those targets ship uncompressed and are
larger. Pass `UPX=0` for an uncompressed build everywhere.

Stamp a version or narrow the target matrix:

```sh
VERSION=0.1.0 ./scripts/build.sh
TARGETS="linux/amd64 windows/amd64" ./scripts/build.sh
```

```powershell
$env:VERSION="0.1.0"; .\scripts\build.ps1
$env:TARGETS="linux/amd64 windows/amd64"; .\scripts\build.ps1
```

Verify a Linux artifact is truly static:

```sh
docker run --rm -v "$PWD/dist":/d alpine sh -c 'apk add -q file && file /d/porty-aio_linux_amd64'
# => ... statically linked ...
```

## Usage

```
porty-aio [flags] <target>

  <target>   host, IP, or CIDR (comma-separated): 10.0.0.0/24,host.lan

flags:
  -p   ports: 'top', 'all' (or '-'), '22,80,443', or '1-1024'   (default "top")
  -c   maximum concurrent connections                           (default 512)
  -t   per-connection timeout                                   (default 1.5s)
  -json   emit results as JSON lines
  -version
```

Flags must come before the target (standard Go flag parsing stops at the first
non-flag argument).

Examples:

```sh
porty-aio 10.0.0.0/24
porty-aio -p 1-65535 -c 1024 192.168.1.10
porty-aio -p 22,80,443 -json 10.0.0.0/24 > open.jsonl
```

## Layout

```
cmd/porty-aio/      CLI entry point
internal/scan/      stdlib-only connect-scan engine
Dockerfile          pinned Go toolchain (single source of truth for the version)
scripts/
  build.sh          host wrapper (Linux/macOS)
  build.ps1         host wrapper (Windows)
  build-matrix.sh   in-container cross-compile loop (the real build logic)
```
