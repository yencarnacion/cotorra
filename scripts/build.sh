#!/usr/bin/env bash
# Cross-build without CGo or audio SDKs. Run from any directory.
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  GOOS="${target%/*}"; GOARCH="${target#*/}"; suffix=''
  [[ "$GOOS" != windows ]] || suffix='.exe'
  printf 'Building %s\n' "$target"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -o "dist/cotorra-${GOOS}-${GOARCH}${suffix}" .
done
