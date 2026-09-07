$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot
$Binary = Join-Path $Root 'bin/cotorra.exe'
$BinDir = Split-Path $Binary
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Install a supported Go toolchain first.'
}
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
$OldCgo = $env:CGO_ENABLED
Push-Location $Root
try {
    $env:CGO_ENABLED = '0'
    & go build -trimpath -o $Binary .
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
} finally {
    $env:CGO_ENABLED = $OldCgo
    Pop-Location
}
# Preserve the caller's directory for .env and relative paths.
& $Binary @args
exit $LASTEXITCODE
