# Neurox Memory Retention — Design Tree

Status: **READY**

## Goal

Retain concise, durable operational knowledge that improves future orchestration
without turning Neurox into an event log.

## Resolved decisions

- Save error solutions with root cause, durable solution, regression proof, and
  limits or non-applicable conditions.
- Save scope decisions and explicit non-goals with their rationale and boundary.
- Save dependency compatibility and deprecation findings with affected versions,
  evidence, and upgrade or avoidance guidance.
- Save explicitly accepted risks with acceptance rationale and mitigation or
  follow-up. Never invent an approver.
- Save recurrent blockers by root-cause class with recurrence evidence and
  documented recovery steps.
- The read-only orchestrator routes these records to `infrastructure-engineer`,
  the authorized Neurox writer.
- Use the project namespace by default. Promote only sanitized, concise,
  reusable patterns that are genuinely cross-project to global memory.
- Preserve the safety boundary: no secrets, tokens, private data, raw prompts,
  large logs/dumps, trivial changes, or routine validation/delivery events.

## Observation contract

Each saved record uses a stable topic key and concise `What / Why / Where /
Evidence / Learned / Limits or follow-up` fields. Existing architecture,
preference, discovery, configuration, pattern, and gotcha triggers remain valid
when they contain durable evidence; retention does not expand to every workflow
event.

## Explicitly out of scope

- Changing Neurox storage, ranking, namespaces, or tool APIs.
- Giving the orchestrator direct Neurox write access.
- Changing `opencode/commands/pr.md` or any other command.
- Saving complete transcripts, prompts, logs, dumps, credentials, or personal
  information.
- Promoting repository-specific implementation details to global memory.

## Affected documents

- `opencode/agents/orchestrator.md` — mandatory orchestration save triggers.
- `opencode/skills/_shared/neurox-protocol.md` — shared triggers and safety rules.
