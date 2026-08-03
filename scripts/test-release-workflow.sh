#!/usr/bin/env bash
set -euo pipefail

workflow="$(dirname "$0")/../.github/workflows/release.yml"
grep -q '^  preflight:' "$workflow"
grep -q '^  release:' "$workflow"
grep -q 'needs: preflight' "$workflow"
grep -q 'environment: release' "$workflow"
grep -q 'git verify-tag' "$workflow"
grep -q 'git verify-commit' "$workflow"
grep -q 'git merge-base --is-ancestor' "$workflow"
grep -q 'commit_sha:' "$workflow"
grep -q 'printf '\''commit_sha=%s' "$workflow"
grep -q 'ref: \${{ needs.preflight.outputs.commit_sha }}' "$workflow"
grep -q "SKYNEX_RELEASE_SIGNING_KEY: \${{ secrets.SKYNEX_RELEASE_SIGNING_KEY }}" "$workflow"

preflight="$(awk '/^  preflight:/{seen=1} /^  release:/{seen=0} seen{print}' "$workflow")"
if grep -q 'secrets\.' <<<"$preflight"; then
  printf '%s\n' 'preflight must not reference secrets' >&2
  exit 1
fi

release="$(awk '/^  release:/{seen=1} seen{print}' "$workflow")"
if grep -q 'ref: \${{ github.ref' <<<"$release" || grep -q 'ref:.*github.ref_name' <<<"$release"; then
  printf '%s\n' 'release checkout must use the verified commit output, not the tag' >&2
  exit 1
fi

while IFS= read -r action; do
  [[ "$action" =~ uses: ]] || continue
  [[ "$action" =~ @[0-9a-f]{40} ]] || { printf 'unpinned action: %s\n' "$action" >&2; exit 1; }
done < <(grep 'uses:' "$workflow")
