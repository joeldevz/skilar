---
name: visual-recap
description: Use when a reviewer needs a self-contained HTML summary of a large, multi-file PR, branch, commit, or diff.
metadata:
  visibility: exported
---

# Visual Recap

Create one standalone local HTML file summarizing a PR, branch, commit, or raw
diff. Skip tiny single-file changes where plain diff review is faster.

## Trigger and deliverable

- Default input is the working-tree diff; also support PRs, branches, commits,
  and patch files as described in the reference.
- Always write the HTML file to disk (default `visual-recap.html` or the chosen
  path), never return the recap as inline prose, and report its absolute path.
- No server, SaaS account, MCP connector, or database is required.

## Workflow and guards

Gather and analyze the requested diff, redact secrets, and ground every claim in
visible diff facts. Build a responsive, self-contained HTML document with only
applicable schema/API, UI, key-change, summary, and review-note sections.

Full source selection, analysis checklist, HTML structure, CSS requirements,
content rules, Mermaid fallback, and handoff requirements are in
[references/recap-workflow.md](references/recap-workflow.md).

## Output

Report the absolute HTML path, recapped scope, changed-file count and net
line count, plus a concise browser-open instruction. Modify the same file for
later edits unless a new path is requested.
