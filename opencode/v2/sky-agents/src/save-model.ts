import { rename, rm } from "node:fs/promises"
import { join } from "node:path"
import { applyEdits, getNodeValue, modify, parseTree, type ParseError } from "jsonc-parser"
import { entry, locked, readBounded, replaceFile, safeDirectory, temporaryFile } from "./storage"

export interface Assignment {
  agentID: string
  providerID: string
  modelID: string
  variant?: string
}

export const MAX_CONFIG_BYTES = 2 * 1024 * 1024
export const MAX_AGENTS = 256

export function object(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value)
}

export function validateAgentID(id: string) {
  if (!id || id.length > 128 || /[\s\x00-\x1f\x7f]/.test(id) || ["__proto__", "prototype", "constructor"].includes(id)) {
    throw new Error("Invalid agent ID")
  }
}

export function parseModel(value: string) {
  if (typeof value !== "string" || value.length > 512 || /[\s\x00-\x1f\x7f]/.test(value)) throw new Error("Invalid model")
  const match = /^([^/#]+)\/([^#]+)(?:#([^#]+))?$/.exec(value)
  if (!match) throw new Error(`Invalid model: ${value}`)
  return { providerID: match[1], id: match[2], variant: match[3] }
}

export function validateModels(models: Record<string, string>) {
  const assignments = Object.entries(models)
  if (assignments.length > MAX_AGENTS) throw new Error(`Maximum of ${MAX_AGENTS} agents per profile allowed`)
  for (const [id, model] of assignments) {
    validateAgentID(id)
    parseModel(model)
  }
}

export async function readGlobalConfig(directory: string) {
  const jsonPath = join(directory, "opencode.json")
  const jsoncPath = join(directory, "opencode.jsonc")
  const json = await readBounded(jsonPath, MAX_CONFIG_BYTES)
  const jsonc = await readBounded(jsoncPath, MAX_CONFIG_BYTES)
  if (json !== undefined && jsonc !== undefined) throw new Error("Both opencode.json and opencode.jsonc exist: consolidate the global configuration before saving")
  const path = jsonc !== undefined ? jsoncPath : jsonPath
  const original = jsonc ?? json
  const text = original ?? '{\n  "$schema": "https://opencode.ai/config.json"\n}\n'
  const errors: ParseError[] = []
  const tree = parseTree(text, errors, { allowTrailingComma: true })
  const config: unknown = tree ? getNodeValue(tree) : undefined
  if (errors.length || !tree || !object(config)) throw new Error("The global configuration contains invalid JSON")
  const pending = [tree]
  while (pending.length) {
    const node = pending.pop()!
    if (node.type === "object") {
      const keys = node.children?.map((property) => property.children?.[0].value) ?? []
      if (new Set(keys).size !== keys.length) throw new Error("The global configuration contains duplicate keys")
    }
    for (const child of node.children ?? []) pending.push(child)
  }
  if (Object.hasOwn(config, "agents") && Object.hasOwn(config, "agent")) throw new Error("Ambiguous configuration: contains both agent and agents; consolidate them before saving")
  const field = Object.hasOwn(config, "agents") ? "agents" : Object.hasOwn(config, "agent") ? "agent" : "agents"
  const agents = config[field]
  if (agents !== undefined && !object(agents)) throw new Error("The agent configuration is not an object")
  return { path, original, text, field, agents: (agents ?? {}) as Record<string, unknown> }
}

export function explicitModels(agents: Record<string, unknown>) {
  const models: Record<string, string> = Object.create(null)
  for (const [id, agent] of Object.entries(agents)) {
    if (!object(agent)) throw new Error(`The configuration for ${id} is not an object`)
    if (!Object.hasOwn(agent, "model")) continue
    let model = agent.model
    if (object(model) && typeof model.providerID === "string" && typeof model.model === "string") {
      if (model.variant !== undefined && typeof model.variant !== "string") throw new Error(`Invalid variant: ${id}`)
      if (!model.providerID || /[/#]/.test(model.providerID) || !model.model || model.model.includes("#") ||
          (typeof model.variant === "string" && (!model.variant || model.variant.includes("#")))) throw new Error(`Invalid global model: ${id}`)
      model = `${model.providerID}/${model.model}${model.variant !== undefined ? `#${model.variant}` : ""}`
    }
    if (typeof model !== "string") throw new Error(`Invalid global model: ${id}`)
    // Legacy separate variant is model-related; retain it in the portable reference.
    if (agent.variant !== undefined) {
      if (typeof agent.variant !== "string" || !agent.variant || model.includes("#")) throw new Error(`Ambiguous or invalid global variant: ${id}`)
      model += `#${agent.variant}`
    }
    models[id] = model
  }
  validateModels(models)
  return models
}

// Caller holds the shared directory lock; every edit is prepared before writing.
export async function writeGlobalModels(directory: string, models: Record<string, string>) {
  validateModels(models)
  if (!Object.keys(models).length) throw new Error("The profile contains no explicit assignments")
  const current = await readGlobalConfig(directory)
  let updated = current.text
  const options = { formattingOptions: { insertSpaces: !/^\t/m.test(updated), tabSize: 2, eol: updated.includes("\r\n") ? "\r\n" : "\n" } }
  for (const [id, model] of Object.entries(models)) {
    const agent = Object.hasOwn(current.agents, id) ? current.agents[id] : undefined
    if (agent !== undefined && !object(agent)) throw new Error(`The configuration for ${id} is not an object`)
    updated = applyEdits(updated, modify(updated, [current.field, id, "model"], model, options))
    if (object(agent) && Object.hasOwn(agent, "variant")) {
      updated = applyEdits(updated, modify(updated, [current.field, id, "variant"], undefined, options))
    }
  }
  if (Buffer.byteLength(updated) > MAX_CONFIG_BYTES) throw new Error("The resulting configuration is too large")
  const mode = ((await entry(current.path))?.mode ?? 0o600) & 0o777
  const temporary = await temporaryFile(current.path, updated, mode)
  try {
    const latest = await readGlobalConfig(directory)
    if (latest.path !== current.path || latest.original !== current.original) throw new Error("The configuration changed while saving; try again")
    if (current.original !== undefined) await replaceFile(`${current.path}.sky-agents.bak`, current.original)
    await safeDirectory(directory)
    const beforeReplace = await readGlobalConfig(directory)
    if (beforeReplace.path !== current.path || beforeReplace.original !== current.original) throw new Error("The configuration changed while saving; try again")
    await rename(temporary, current.path)
    return current.path
  } finally {
    await rm(temporary, { force: true })
  }
}

export async function saveGlobalModel(directory: string, assignment: Assignment): Promise<string> {
  validateAgentID(assignment.agentID)
  if (!assignment.providerID || !assignment.modelID || /[\s#]/.test(assignment.providerID + assignment.modelID) || assignment.providerID.includes("/")) throw new Error("Invalid model")
  if (assignment.variant !== undefined && (typeof assignment.variant !== "string" || !assignment.variant || assignment.variant.includes("#"))) throw new Error("Invalid variant")
  const models = { [assignment.agentID]: `${assignment.providerID}/${assignment.modelID}${assignment.variant !== undefined ? `#${assignment.variant}` : ""}` }
  validateModels(models)
  return locked(directory, () => writeGlobalModels(directory, models))
}
