#!/usr/bin/env bash
set -euo pipefail

# Compatibility entry point. The Go installer is the sole implementation.
if command -v skynex >/dev/null 2>&1; then
  exec skynex install "$@"
fi

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
if [ -f "$repo_root/go.mod" ] && [ -d "$repo_root/cmd/skynex" ] && command -v go >/dev/null 2>&1; then
  exec go run ./cmd/skynex install "$@"
fi

printf '%s\n' 'skynex is not installed and this is not a Go repository checkout.' >&2
printf '%s\n' 'Install skynex first, then rerun scripts/setup.sh.' >&2
exit 1
