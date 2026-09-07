import type { ModelInfo } from "@opencode-ai/client"

const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0

export function profileAgents<T extends { id: string; hidden?: boolean; mode?: string }>(agents: readonly T[]): T[] {
  return agents.filter((agent) => !agent.hidden && !agent.id.startsWith("_") &&
    (agent.id === "skynex-orchestrator" || agent.mode === "subagent" || agent.mode === "all"))
    .sort((a, b) => a.id === b.id ? 0 : a.id === "skynex-orchestrator" ? -1 : b.id === "skynex-orchestrator" ? 1 : compare(a.id, b.id))
}

export function modelOptions(models: readonly ModelInfo[]) {
  return models.filter((model) => model.enabled)
    .sort((a, b) => compare(a.providerID, b.providerID) || compare(a.name, b.name) || compare(a.id, b.id))
    .flatMap((model) => [undefined, ...model.variants.map((variant) => variant.id).sort(compare)].map((variant) => {
      const reference = `${model.providerID}/${model.id}${variant === undefined ? "" : `#${variant}`}`
      return {
        // beta-19234 has no search/keywords field: keep all search terms in title.
        title: `${model.name || model.id} · ${reference}`,
        value: reference,
        description: variant === undefined ? "Base model" : `Variant: ${variant}`,
        category: model.providerID,
      }
    }))
}
