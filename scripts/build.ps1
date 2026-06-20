# build.ps1: host wrapper (Windows). Drives the Dockerized cross-compile.
#
# No local Go toolchain required; only Docker Desktop. Artifacts land in .\dist.
#
# Env:
#   VERSION   version string to stamp (default: dev)
#   TARGETS   override the GOOS/GOARCH matrix (space-separated "os/arch")
#
# Examples:
#   .\scripts\build.ps1
#   $env:VERSION="0.1.0"; .\scripts\build.ps1
#   $env:TARGETS="linux/amd64 windows/amd64"; .\scripts\build.ps1
#requires -Version 5
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

$Image   = 'porty-aio-builder'
$Version = if ($env:VERSION) { $env:VERSION } else { 'dev' }
$Targets = if ($env:TARGETS) { $env:TARGETS } else { '' }

Write-Host "[*] Building builder image ($Image)..."
docker build -t $Image .
if ($LASTEXITCODE -ne 0) { throw "docker build failed" }

Write-Host "[*] Cross-compiling (VERSION=$Version)..."
docker run --rm `
	-e VERSION=$Version `
	-e TARGETS=$Targets `
	-v "${PWD}:/src" `
	-v porty-aio-gocache:/root/.cache/go-build `
	-v porty-aio-gomod:/go/pkg/mod `
	-w /src `
	$Image sh scripts/build-matrix.sh
if ($LASTEXITCODE -ne 0) { throw "build failed" }

Write-Host "[*] Done. Binaries in .\dist"
