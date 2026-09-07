#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
command -v go >/dev/null 2>&1 || { printf 'Install a supported Go toolchain first.\n' >&2; exit 1; }
mkdir -p "$ROOT/bin"
(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$ROOT/bin/cotorra" .)
# Preserve the caller's directory: .env and relative file paths resolve there.
exec "$ROOT/bin/cotorra" "$@"
