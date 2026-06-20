# test.ps1: host wrapper (Windows). Runs the test suite in the build container,
# so no local Go toolchain is required. Extra args are passed through to
# `go test` (e.g. .\scripts\test.ps1 -run Scan -v).
#requires -Version 5
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

$Image = 'porty-aio-builder'

Write-Host "[*] Building builder image ($Image)..."
docker build -t $Image .
if ($LASTEXITCODE -ne 0) { throw "docker build failed" }

Write-Host "[*] Running tests..."
docker run --rm `
	-v "${PWD}:/src" `
	-v porty-aio-gocache:/root/.cache/go-build `
	-v porty-aio-gomod:/go/pkg/mod `
	-w /src `
	$Image go test ./... -count=1 @args
if ($LASTEXITCODE -ne 0) { throw "tests failed" }
