---
name: prd
description: Use when the user requests a PRD, requirements document, feature spec, or product structure.
license: MIT
---

# AI Product Requirements Document (PRD) Agent

## When to Use

Use this skill for a new product or feature cycle, vague ideas needing a
concrete specification, AI feature requirements, or a request to write a PRD.

## Guardrails and workflow

- Do not generate the full PRD immediately: discover context, present an
  outline, and stop for explicit human approval before technical drafting.
- Ask targeted questions about the problem, measurable success metrics,
  constraints, data, privacy, and deadlines.
- Mark unknown constraints `TBD`; never invent metrics, architecture, or data.
- Consider ethical, privacy, safety, and ecosystem impacts.
- Use concrete, measurable acceptance criteria and clean handoff tables.

The complete workflow, quality standards, strict schema, implementation
checklist, example, and memory requirements are in
[references/prd-workflow.md](references/prd-workflow.md).

## Output

After approval, produce the PRD using the referenced five-section schema:
executive/system context, multidimensional KPIs, UX/functionality, AI/data
requirements when applicable, and technical specifications/risks.
