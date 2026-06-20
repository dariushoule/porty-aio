# CLAUDE.md

Guidance for working in the porty-aio repo.

## What this is

porty-aio is a compact, dependency-free, cross-platform network tool. The whole
identity is "one tiny static binary that just works anywhere", so every decision
defends that promise. Its core is a TCP connect scanner; it also has a single-box
TCP port forwarder (the "aio" part). Both are standard library only.

## Core architecture

- **Connect-scan only, on purpose.** TCP connect needs no raw sockets, no
  libpcap/Npcap, no root. That constraint is what lets the binary be fully
  static. Do not add scan modes that require raw sockets or native packet
  libraries; they would break the core promise.
- **Standard library only.** The scan engine (`internal/scan`) and forwarder
  (`internal/forward`) use only the Go stdlib (`net`, `context`, `sync`, `io`).
  Keep it that way. Adding a third-party dependency needs a strong, explicit
  justification because dependency-free is both a runtime and a supply-chain
  selling point.
- **Forwarding is a single-box relay, not a tunnel.** porty runs on one host,
  listens, and relays to a destination (loopback service or another reachable
  machine). No second instance, no multiplexing, no SSH-style local/reverse
  modes. The CLI is `forward --listen <addr> --to <host:port>`, repeatable.
- **Static by default.** All builds are `CGO_ENABLED=0`. The output must stay
  "statically linked" (verify with `file` / `ldd` showing "not a dynamic
  executable"). Never introduce cgo.
- **Go, chosen for cross-compilation.** `GOOS`/`GOARCH` with CGO off gives
  static binaries for every target from one machine. This is a load-bearing
  reason for the language choice, not incidental.

## Layout

```
cmd/porty-aio/      CLI entry point; "forward" subcommand, else scan (flags, output)
internal/scan/      stdlib-only connect-scan engine, target/port parsers, TopPorts
  scan_test.go      unit tests (parsing) + integration test (real loopback scan)
internal/forward/   stdlib-only TCP port forwarder (Listen + Serve relay)
Dockerfile          pinned Go toolchain; single source of truth for the Go version
scripts/
  build.sh          host wrapper (Linux/macOS), drives Docker
  build.ps1         host wrapper (Windows), drives Docker
  build-matrix.sh   in-container cross-compile loop; the real build logic lives here
  test.sh           host wrapper (Linux/macOS) to run the test suite in the container
  test.ps1          host wrapper (Windows) to run the test suite in the container
```

## Build

Builds run in Docker so no local Go toolchain is required. To upgrade Go, bump
the one `FROM` line in the `Dockerfile`. The host wrappers stay thin; shared
build logic lives only in `build-matrix.sh` (do not duplicate the matrix across
the `.sh` and `.ps1`).

```sh
./scripts/build.sh                       # full matrix
TARGETS="linux/amd64" ./scripts/build.sh # narrow for quick checks
```

## Tests

Run the suite with `./scripts/test.sh` (or `.\scripts\test.ps1`), which runs
`go test ./...` in the build container. The scan engine has table-driven unit
tests for parsing plus an integration test that opens real loopback listeners
and asserts the scanner reports exactly the expected open port set. Keep them
green, and prefer hermetic loopback-based tests over anything needing the
network.

## Conventions

- Binaries are UPX-compressed (`--best --lzma`) to minimize size. Packing is
  skipped for darwin (macOS codesigning rejects packed binaries) and
  windows/arm64 (not supported by UPX). Toggle with `UPX=0` for an uncompressed
  build when debugging.
- `-json` output is JSONL (one object per line) so it pipes cleanly into other
  tools.
- Prose and comments: plain punctuation. No em-dashes.

## Commits

- Do not add Claude/AI co-authorship trailers. Commit as the user only (no
  `Co-Authored-By` line).

## Branching and releases

- New features go through a branch and a pull request into `main`, not direct
  commits. This is what powers the auto-generated release notes: each merged PR
  becomes a changelog entry, grouped by label (see `.github/release.yml`). Label
  PRs with `feature`/`enhancement`, `bug`/`fix`, or `documentation` so they land
  in the right section.
- Foundational infra (CI config, build scripts) may go straight to `main`.
- CI (`.github/workflows/ci.yml`) runs gofmt, vet, and tests on every push and
  PR to `main`.
- To cut a release, push a `vMAJOR.MINOR.PATCH` tag. `release.yml` builds the
  full static + UPX matrix via `scripts/build.sh`, attaches the binaries and
  `SHA256SUMS.txt`, and creates the GitHub Release with generated notes. Stay on
  `v0.x` (breaking changes allowed in minor) until the API stabilizes.
