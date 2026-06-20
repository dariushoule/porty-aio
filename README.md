# porty-aio

A compact, dependency-free, cross-platform network tool that just works anywhere.

One static binary. No libpcap, no Npcap, no glibc pinning, no root. Its core is a
TCP connect scanner, which needs no raw sockets or packet-capture libraries, plus
a simple single-box TCP port forwarder. A single `go build` produces a binary you
can drop on any Linux, Windows, or macOS host and run.

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

## Test

Tests run in the same build container, so no local Go toolchain is needed:

```sh
# Linux / macOS
./scripts/test.sh

# Windows
.\scripts\test.ps1
```

This covers the scan engine (port/target/CIDR parsing, plus an integration test
that stands up real loopback listeners and checks the exact open set), the port
forwarder (end-to-end relay through a loopback backend), and the CLI (subcommand
dispatch and text/JSON output modes).

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

## Forward

Run porty on a box and relay a local listener to a destination, which may be a
loopback-only service on the same host or another machine the box can reach.
There is no tunnel and no second instance.

```
porty-aio forward --listen <addr> --to <host:port> [--listen ... --to ...]
```

```sh
# expose another machine's service on this box
porty-aio forward --listen :8080 --to 10.0.0.5:80

# expose a loopback-only service to the network
porty-aio forward --listen :3306 --to 127.0.0.1:3306

# multiple forwards at once
porty-aio forward --listen :8080 --to 10.0.0.5:80 --listen :2222 --to 10.0.0.9:22
```

`--listen` binds all interfaces when the host is omitted (`:8080`); give a host
to bind one interface (`127.0.0.1:8080`). TCP only.

## Layout

```
cmd/porty-aio/      CLI entry point (scan + forward subcommands)
internal/scan/      stdlib-only connect-scan engine
internal/forward/   stdlib-only TCP port forwarder
Dockerfile          pinned Go toolchain (single source of truth for the version)
scripts/
  build.sh            host wrapper (Linux/macOS)
  build.ps1           host wrapper (Windows)
  build-matrix.sh     in-container cross-compile loop (the real build logic)
  test.sh / test.ps1  run the test suite in the build container
```
