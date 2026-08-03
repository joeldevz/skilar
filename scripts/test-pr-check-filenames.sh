#!/usr/bin/env bash
set -euo pipefail

workflow="$(dirname "$0")/../.github/workflows/pr-check.yml"
grep -q -- "git diff --name-only -z" "$workflow"
grep -q -- "read -r -d '' file" "$workflow"
! grep -q -- 'steps.changed.outputs.files' "$workflow"

repo="$(mktemp -d)"
trap 'rm -rf "$repo"' EXIT
git -C "$repo" init -q
git -C "$repo" config user.email test@example.invalid
git -C "$repo" config user.name test
git -C "$repo" commit --allow-empty -qm base
name=$'SKILL-$(touch SHOULD_NOT_EXIST) `printf nope`\nfile/SKILL.md'
mkdir -p "$repo/$(dirname "$name")"
printf '%s\n' safe > "$repo/$name"
git -C "$repo" add -- "$name"
git -C "$repo" commit -qm change
git -C "$repo" diff --name-only -z HEAD^ HEAD |
  while IFS= read -r -d '' file; do
    [ "$file" = "$name" ] || { printf 'unsafe filename boundary: %q\n' "$file" >&2; exit 1; }
  done
[ ! -e "$repo/SHOULD_NOT_EXIST" ]
