# Design Tree - Reliable Windows Installation

## Resolved decisions
- D1: Windows installation must work as a single supported flow. It must install the Skynex OpenCode configuration without requiring users to manually extract releases, clear caches, or repair dependencies.
- D2: Backup snapshots exclude only an explicit allowlist of regenerable OpenCode caches, beginning with `EBWebView/Default/Code Cache`. User configuration, skills, credentials, and other unrecognised files remain protected.
- D3: The installer detects and uses an already-installed package manager in this order: Bun, pnpm, then npm. It runs dependency installation with lifecycle scripts disabled. If no manager is available or dependency installation fails, the installed Skynex configuration remains in place and the CLI reports the required next action.
- D4: Missing or failed plugin dependency installation is a successful partial installation: Skynex keeps the installed orchestrators and reports `skynex deps` as the focused retry command.
- D5: The Windows release installer accepts a root `README.md` plus the root `skynex.exe`, rejects all other archive shapes, and extracts only the executable.
- D6: Windows dependency installation validates every destination component, rejects reparse points, and retains a directory handle without delete sharing while Bun, pnpm, or npm runs. Unsupported platforms remain fail-closed, and native Windows verification is required before release.

## Open assumptions (validate before PRD)
- None.

## Out of scope (explicit)
- Changing users' existing OpenCode configuration beyond the managed Skynex files.

## Ready for PRD
✅ yes
