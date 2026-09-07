# Sky Agents (OpenCode V2 beta-19234)

`/sky-agents` opens a menu to assign an agent model, create a profile with **New profile**, or
list profiles. Selecting a saved profile opens **View configuration**, **Edit**, **Rename**,
**Delete**, and **Apply** actions. Agent assignment and apply actions store preferences in the connected
server's global OpenCode configuration. Profile management only changes saved
profile files. Existing sessions keep their selected
model. Project configuration can override the global preference.

## Global profiles

Profiles use the canonical Skynex directory on the **server**:
`~/.config/skynex/profiles/<name>.json`, independent of OpenCode's config-directory
overrides. The Go-compatible document contains `name`, `created_at`, `updated_at`
(RFC3339 timestamps), and `models` (agent → `provider/configured-alias#variant`).
The optional `#variant` is retained; upstream model IDs do not replace aliases.

- **New profile** checks name availability on the server before model selection,
  prompting again for an invalid or existing name, then asks for the configured model for
  `skynex-orchestrator` first. It refreshes the available, nonhidden agent catalog
  and asks for each agent with mode `subagent` or `all`, in stable ID order.
  Each subagent can explicitly choose **Inherit from orchestrator** or its own model.
  Directly below inheritance, **Current / recent model** offers that agent's
  effective `provider/configured-alias#variant`, only if still enabled in the catalog.
  This is not global selection history; it is copied only when explicitly chosen.
  Inheritance resolves to the chosen orchestrator reference (including its variant)
  and is stored as an explicit assignment, not a dynamic link.
  A final review shows every assignment and inherited indicators; **Save profile**
  is required. Canceling any step creates no profile. No active assignments are
  copied automatically and creation does not change global configuration. Existing names are
  refused without overwrite. The server validates the entire mapping against its
  current catalog, including orchestrator presence, complete subagent coverage,
  enabled configured model IDs and variants; catalog changes can require starting again.
  A name taken during the wizard returns a declared duplicate-name RPC error;
  the wizard keeps all selections and asks for another name before reviewing and retrying.
- Both model selectors group enabled models by provider in stable order and show
  readable names plus full `provider/configured-alias#variant` references. Search
  matches names, providers and configured aliases through the option title: the
  installed beta-19234 dialog option type has no separate search/keywords field.
  Base models and available variants are individually selectable.
- **List profiles** displays saved names and timestamps; selecting one opens its profile action
  submenu. **View configuration** displays saved assignments and returns to that submenu.
  Malformed or unsafe profiles cause an error rather than silently disappearing.
- **Edit profile** loads the saved mapping, including variants, rather than active
  configuration. Each selector starts on its saved choice. Current/recent models
  are explicit shortcuts for that particular agent. New subagents start on an
  explicit inheritance choice and must be selected and reviewed. Unavailable saved
  agents, aliases or variants block editing rather than dropping assignments.
  **Save changes** in the navigable review is required; replacement is atomic,
  retaining the name and `created_at` while updating `updated_at`.
- **Rename profile** validates the new name and reviews the exact old/new names
  and saved assignments before **Confirm rename**. Assignments and `created_at`
  are retained; embedded name and `updated_at` are updated. Existing destinations
  are never overwritten, including concurrent collisions. Rename publishes the
  destination before removing the validated unchanged source; this is **not a
  full filesystem transaction**. A partial failure explicitly reports that the
  destination was published and the source could not safely be removed. Inspect
  both names; no destructive rollback is attempted.
- **Delete profile** reviews the exact name and saved assignments. **Cancel** is
  the initial selection; only the explicit delete action removes the file.
  Deletion does not undo previously applied models.
- Edit, rename and delete use server-returned content/file-identity digest tokens
  and recheck them under the shared exclusive profile-directory lock. Concurrent
  changes require reopening and reviewing again. Locks coordinate participating
  writers; they cannot make a check followed by unlink/rename transactional against
  an external same-user writer that ignores the lock. These actions never write
  active OpenCode configuration or modify sessions. Expected RPC errors use fixed,
  bounded messages rather than exposing filesystem errors or arbitrary details.
- **Apply profile** previews current global → stored assignments and requires an explicit
  confirmation. The server rechecks the previewed profile, all agent IDs, enabled
  model aliases and variant IDs before a single batch write. Only profile agents'
  model fields (including legacy model-related variant fields) are changed.
  Other agents, comments and unrelated settings are preserved. A preview expires
  after five minutes; another client consuming the same preview requires reopening
  it. Creation RPC inputs contain a name and explicit model mapping; management
  writes also carry a version token, and edit/rename carry mappings/a new name.
  Clients never supply filesystem paths.

Names match Go's rules: 1–32 lowercase letters, digits or hyphens, except
`default`. Limits: 100 profiles, 64 KiB per profile, 256 assignments, 128 characters
per agent ID, 512 per model reference, 1,024 directory entries, and 2 MiB for the
global config. Empty mappings cannot be saved or applied. Symlinks, non-regular
or hard-linked files, and unsafe directory ancestors are rejected. Configuration
containing both `agent` and `agents`, or both global JSON and JSONC files, is
refused as ambiguous. Locks are exclusive and fail honestly when busy; a stale
lock after a crashed process requires manual inspection/removal.

Undo is deferred. The backup described below is the previous
global configuration, not an automatic undo workflow.

## Shipped installation (OpenCode V2 beta-19234)

Skynex includes the production package at `v2/sky-agents/` under the installed
OpenCode configuration directory. Two discovered directories load it:

```text
~/.config/opencode/plugins/sky-agents/tui.ts
~/.config/opencode/plugins/sky-agents-config/index.ts
```

The first file re-exports `../../v2/sky-agents/src/tui`; the second re-exports
`../../v2/sky-agents/src/index`. Both paths are relative to their wrapper, so the
installation needs neither a Skynex checkout nor its temporary extraction directory.
Keep the
server directory free of a TUI entrypoint: beta-19234 otherwise discovers the same
TUI both locally and from the server's plugin list. Do not also list the package
in `cli.json` or server configuration.

The root OpenCode manifest and npm/Bun/pnpm lockfiles contain the runtime graph:
plugin/client `0.0.0-beta-19234`, OpenTUI core/Solid `0.5.10`, Solid `1.9.12`
(the renderer's exact peer), and JSONC parser `3.3.1`. Standard managed dependency
setup uses frozen locks and disables lifecycle scripts by default. These are V2
dependencies, not a claim of V1 plugin API compatibility. Other legacy Skynex
plugins are unchanged by this integration.

`shipping.json` lists production files for both checkout installs and embedded
assets. Tests, development locks, tsconfig and nested node_modules are not shipped.
For a bounded asset refresh, build/run `cmd/tools/sync-assets` with `--sky-agents`;
the ordinary full sync also honors the manifest. Rebuild and release Skynex after
refreshing assets: an older binary cannot include new source automatically.

No profile JSON is shipped or seeded by the installer. On profile operations the
server lazily initializes the protected `default` profile when absent, preserving
existing files. Its orchestrator reference is `openai/gpt-5.6-sol-fast#high`; its
subagent reference is `openai/gpt-5.6-luna-fast#high`. Installation does not apply
these assignments or configure providers. Applying a profile remains explicit.

For package development only, run `npm ci --ignore-scripts` in this package and
`npm run typecheck`. Do not put absolute checkout imports in installed wrappers.

The TUI calls the `skynex.sky-agents` RPC on the connected server. The server
validates the selected agent and model, then edits only that agent's model field.
It uses `OPENCODE_CONFIG_DIR`, or `$XDG_CONFIG_HOME/opencode`, or
`~/.config/opencode`. Legacy `agent` and native V2 `agents` documents are supported.
Comments and unrelated settings are preserved. Saves use an exclusive lock and
atomic replacement; the previous contents are backed up beside the config as
`opencode.json(c).sky-agents.bak`.

A green notification requires both a successful write and a matching model in
the server's refreshed agent state. A warning after saving distinguishes pending
reload or project overrides from a write failure. Cancellation never writes.

## Mechanical verification

```sh
npm run typecheck
```

No automated tests or runtime mutations were performed for this management change.
Read-only runtime review can inspect `/plugins`, open selectors and cancel previews.
Saving, renaming, deleting or applying real profiles requires the user's explicit
runtime confirmation. A healthy server or successful typecheck alone does not
prove TUI activation or persistence.
