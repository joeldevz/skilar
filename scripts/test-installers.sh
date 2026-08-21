#!/usr/bin/env bash
set -euo pipefail

# This is an acceptance harness. Every required Unix case invokes install.sh and
# asserts both its exit status and filesystem effect; trust pinning is checked
# against the repository's public trust fixture before those cases run.
root="$(cd "$(dirname "$0")/.." && pwd)"
bash -n "$root/scripts/setup.sh" "$root/scripts/install.sh"
trusted_pub_key="$(awk 'NF >= 2 { print $2; exit }' "$root/release/trust/skynex-release-signing-key.pub")"
for installer in "$root/scripts/install.sh" "$root/scripts/install.ps1"; do
  grep -Fq "$trusted_pub_key" "$installer" || {
    printf 'installer trust pin is not current: %s\n' "$installer" >&2
    exit 1
  }
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fixture="$tmp/fixture"; mkdir -p "$fixture/v1.2.3" "$tmp/bin"
executed=0

ssh-keygen -q -t ed25519 -N '' -f "$tmp/release-key" >/dev/null
ssh-keygen -y -f "$tmp/release-key" >"$tmp/release-key.pub"
pub="$(cat "$tmp/release-key.pub")"
pub_key="$(printf '%s\n' "$pub" | awk '{print $2}')"
printf 'skynex-release %s fixture\n' "$pub" >"$tmp/allowed_signers"

cat >"$tmp/fake-curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
out=''
url=''
while (($#)); do
  case "$1" in -o) out="$2"; shift 2 ;; -w) shift 2 ;; --*) shift ;; *) url="$1"; shift ;; esac
done
if [[ "$url" == */releases/latest ]]; then
  printf '{"tag_name":"v1.2.3"}\n200\n'
else
  name="${url##*/}"
  cp -- "$SKYNEX_FIXTURE/$name" "$out"
fi
EOF
chmod 755 "$tmp/fake-curl"
ln -s "$tmp/fake-curl" "$tmp/bin/curl"

make_binary() {
  local path="$1"
  { printf '#!/usr/bin/env bash\n'; printf 'if [ "$1" = internal-install-binary ]; then cp -- "$2" "$3"; chmod 755 "$3"; exit 0; fi\n'; printf 'printf help\\n'; head -c 1400 /dev/urandom; } >"$path"
  chmod 755 "$path"
}

make_case() {
  local archive="$1" extra="${2:-}"
  make_binary "$tmp/skynex"
  if [[ -n "$extra" ]]; then
    tar -czf "$archive" -C "$tmp" skynex "$extra"
  else
    tar -czf "$archive" -C "$tmp" skynex
  fi
  local name; name="$(basename "$archive")"
  cp -- "$archive" "$fixture/v1.2.3/$name"
  sha256sum "$archive" | sed "s#  .*#  $name#" >"$fixture/v1.2.3/checksums.txt"
  rm -f "$fixture/v1.2.3/checksums.txt.sig"
  (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
}

run_installer() {
  local case_name="$1" archive_name="$2"; shift 2
  executed=$((executed + 1))
  local home="$tmp/home-$case_name" dest="$tmp/dest-$case_name"; mkdir -p "$home"
  if [[ "$case_name" == symlink ]]; then
    mkdir -p "$tmp/outside"
    ln -s "$tmp/outside" "$dest"
  fi
  local script="$tmp/install-$case_name.sh"
  sed -e "s#$trusted_pub_key#$pub_key#" \
    -e 's#https://github.com/joeldevz/skynex/releases/download#http://fixture/releases/download#g' \
    "$root/scripts/install.sh" >"$script"
  chmod 755 "$script"
  SKYNEX_FIXTURE="$fixture/v1.2.3" \
    HOME="$home" PATH="$tmp/bin:/usr/bin:/bin" \
    bash "$script" --dir "$dest" "$@"
  test -x "$dest/skynex"
  test "$(head -n 1 "$dest/skynex")" = '#!/usr/bin/env bash'
}

archive_name='skynex_1.2.3_linux_amd64.tar.gz'
make_case "$tmp/good.tar.gz"
mv "$fixture/v1.2.3/$(basename "$tmp/good.tar.gz")" "$fixture/v1.2.3/$archive_name"
sed -i "s#$(basename "$tmp/good.tar.gz")#$archive_name#" "$fixture/v1.2.3/checksums.txt"
rm -f "$fixture/v1.2.3/checksums.txt.sig"; (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
ssh-keygen -Y verify -f "$tmp/allowed_signers" -I skynex-release -n file -s "$fixture/v1.2.3/checksums.txt.sig" <"$fixture/v1.2.3/checksums.txt" >/dev/null
run_installer success "$archive_name"

expect_failure() {
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then printf 'case unexpectedly passed: %s\n' "$label" >&2; exit 1; fi
}

expect_installer_failure_clean() {
  local label="$1" case_name="$2" archive_name="$3"; shift 3
  expect_failure "$label" run_installer "$case_name" "$archive_name" "$@"
  local dest="$tmp/dest-$case_name"
  if [[ "$case_name" == symlink ]]; then
    test -L "$dest"
    test ! -e "$tmp/outside/skynex"
  else
    test ! -e "$dest"
  fi
  if compgen -G "$dest/.skynex-*" >/dev/null 2>&1; then
    printf 'temporary installer residue in failed destination: %s\n' "$dest" >&2
    exit 1
  fi
}

# Production scripts must not accept the removed test-only signer overrides.
expect_failure production-env-bypass env SKYNEX_FIXTURE="$fixture/v1.2.3" SKYNEX_TEST_MODE=1 SKYNEX_TEST_ALLOWED_SIGNERS="$tmp/allowed_signers" \
  HOME="$tmp/home-production-env-bypass" PATH="$tmp/bin:/usr/bin:/bin" \
  bash "$root/scripts/install.sh" --dir "$tmp/dest-production-env-bypass" "$archive_name"
test ! -e "$tmp/dest-production-env-bypass"

# Invalid signature and checksum must fail before creating a destination.
cp "$fixture/v1.2.3/checksums.txt.sig" "$tmp/good.sig"
printf 'tampered\n' >"$fixture/v1.2.3/checksums.txt.sig"
expect_installer_failure_clean tampered-signature bad-signature "$archive_name"
cp "$tmp/good.sig" "$fixture/v1.2.3/checksums.txt.sig"
printf '%064d  %s\n' 0 "$archive_name" >"$fixture/v1.2.3/checksums.txt"
rm -f "$fixture/v1.2.3/checksums.txt.sig"; (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
expect_installer_failure_clean tampered-checksum bad-checksum "$archive_name"

# Rebuild the valid fixture, then exercise archive member and size guards.
make_case "$tmp/valid.tar.gz"; mv "$fixture/v1.2.3/valid.tar.gz" "$fixture/v1.2.3/$archive_name"
sed -i "s#valid.tar.gz#$archive_name#" "$fixture/v1.2.3/checksums.txt"
rm -f "$fixture/v1.2.3/checksums.txt.sig"; (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
printf x >"$tmp/extra"
make_case "$tmp/extra.tar.gz" extra
mv "$fixture/v1.2.3/extra.tar.gz" "$fixture/v1.2.3/$archive_name"
sed -i "s#extra.tar.gz#$archive_name#" "$fixture/v1.2.3/checksums.txt"
rm -f "$fixture/v1.2.3/checksums.txt.sig"; (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
expect_installer_failure_clean extra-member extra "$archive_name"
make_binary "$tmp/skynex"
tar -czf "$tmp/traversal.tar.gz" -C "$tmp" --transform='s#^skynex$#../escape#' skynex
cp "$tmp/traversal.tar.gz" "$fixture/v1.2.3/$archive_name"
sha256sum "$tmp/traversal.tar.gz" | sed "s#  .*#  $archive_name#" >"$fixture/v1.2.3/checksums.txt"
rm -f "$fixture/v1.2.3/checksums.txt.sig"; (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
expect_installer_failure_clean traversal-member traversal "$archive_name"
make_case "$tmp/size.tar.gz"
mv "$fixture/v1.2.3/size.tar.gz" "$fixture/v1.2.3/$archive_name"
sed -i "s#size.tar.gz#$archive_name#" "$fixture/v1.2.3/checksums.txt"
rm -f "$fixture/v1.2.3/checksums.txt.sig"; (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
export SKYNEX_MAX_COMPRESSED_BYTES=1000
expect_installer_failure_clean compressed-size size "$archive_name"
unset SKYNEX_MAX_COMPRESSED_BYTES
export SKYNEX_MAX_EXTRACTED_BYTES=100
expect_installer_failure_clean extracted-size extracted "$archive_name"
unset SKYNEX_MAX_EXTRACTED_BYTES

# A destination symlink is rejected and cannot redirect the verified binary.
make_case "$tmp/link.tar.gz"; mv "$fixture/v1.2.3/link.tar.gz" "$fixture/v1.2.3/$archive_name"
sed -i "s#link.tar.gz#$archive_name#" "$fixture/v1.2.3/checksums.txt"
rm -f "$fixture/v1.2.3/checksums.txt.sig"; (cd "$fixture/v1.2.3" && ssh-keygen -q -Y sign -n file -f "$tmp/release-key" checksums.txt >/dev/null)
expect_installer_failure_clean destination-symlink symlink "$archive_name"

# setup.sh must forward arguments to the installed binary.
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$@" >"%s/setup.args"\n' "$tmp" >"$tmp/bin/skynex"
chmod 755 "$tmp/bin/skynex"
SKYNEX_TEST_ARGS="$tmp/setup.args" PATH="$tmp/bin:/usr/bin:/bin" "$root/scripts/setup.sh" --package skills --target opencode
test "$(tr '\n' ' ' <"$tmp/setup.args")" = 'install --package skills --target opencode '
test "$executed" -eq 8 || { printf 'required Unix installer cases executed: %s/8\n' "$executed" >&2; exit 1; }

go test ./internal/safefs ./internal/binaryinstall ./internal/config ./internal/skillsync
if command -v pwsh >/dev/null 2>&1; then
  pwsh -NoProfile -NonInteractive -File "$root/scripts/test-installers.ps1"
else
  printf '%s\n' 'PowerShell acceptance skipped locally: pwsh is unavailable (not counted as passed).' >&2
fi
printf '%s\n' 'Unix installer runtime acceptance passed'
