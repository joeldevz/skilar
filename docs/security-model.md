# Installer security model

Skynex treats embedded assets as trusted because they are compiled into the
released binary. Non-embedded repository installs are different: the
requested ref is resolved by Git, then the GitHub API must independently report
that the same repository ref resolves to the exact selected commit and that
commit has a verified cryptographic signature. The checkout is fetched and
detached at that bound SHA; a changed ref, different repository, unsigned
commit, API error, or unavailable network fails closed. This gate does not
invent or locally validate a signature, and it does not replace review of the
trusted signing identity configured for the repository.

Release archives must use the same repository/tag-to-verified-commit binding
before extraction. There is no offline bypass for non-embedded sources:
offline installation requires the embedded binary (or a separately verified
local workspace selected explicitly as `workspace`).

Filesystem writes use retained rooted descriptors, atomic replacement, and
descriptor-based permission changes where supported. Source/config reads reject
symlinks and hard links and verify file identity across the read.
