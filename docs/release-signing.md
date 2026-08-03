# Release signing

Release checksums and the installer scripts are signed with the pinned Ed25519
SSH key at
`release/trust/skynex-release-signing-key.pub`. The release workflow requires
the protected GitHub Environment `release`, containing the complete private key
as the environment secret `SKYNEX_RELEASE_SIGNING_KEY`. The key is never a
repository or organization secret.

Repository configuration is a security prerequisite: protect `v*` tags so only
authorized maintainers can create them, and configure the `release` environment
with required reviewers. The preflight job fails closed unless the tag and its
commit are verified signed commits and the tag commit is an ancestor of `main`.
GitHub branch/tag and environment protection are external controls and cannot
be represented or enforced by this repository alone.

Maintainer procedure:

1. Copy `/tmp/opencode/skynex-release-signing/release-signing-key` into the
   `SKYNEX_RELEASE_SIGNING_KEY` Actions secret without adding it to the
   repository, logs, or shell history.
2. Confirm a release produces `checksums.txt.sig`, `install.sh.sig`, and
   `install.ps1.sig` alongside their signed assets.
3. Securely delete the local private key after the secret has been verified.

Bootstrap is intentionally not claimed to be trustless: a script fetched and
executed in one pipeline can be replaced before signature verification. Users
must download the tagged script and its signature, verify with the public key,
and only then execute it. Package managers remain delegated trust paths.

Verification uses OpenSSH's detached-signature format:

```bash
ssh-keygen -Y verify -f release/trust/skynex-release-signing-key.pub \
  -I skynex-release -n file -s install.sh.sig < install.sh
```

Homebrew remains delegated to Homebrew's tap, bottle, and transport trust
model; it is a separate trust boundary from release checksum signatures.
