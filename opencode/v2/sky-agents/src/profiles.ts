import { link, opendir, rename, rm, unlink } from "node:fs/promises"
import { createHash } from "node:crypto"
import { homedir } from "node:os"
import { join } from "node:path"
import { object, parseModel, validateModels, writeGlobalModels } from "./save-model"
import { entry, locked, readBounded, safeDirectory, temporaryFile } from "./storage"
import { profileAgents } from "./catalog"

export const MAX_PROFILE_BYTES = 64 * 1024
export const MAX_PROFILES = 100
export const profileDirectory = () => join(homedir(), ".config", "skynex", "profiles")

export interface Profile {
  name: string
  created_at: string
  updated_at: string
  models: Record<string, string>
}

export interface Catalog {
  agents: readonly { id: string; hidden?: boolean; mode?: string }[]
  models: readonly { providerID: string; id: string; enabled: boolean; variants: readonly { id: string }[] }[]
}

export function validateSafeName(name: string) {
  if (typeof name !== "string" || !/^[a-z0-9-]{1,32}$/.test(name)) {
    throw new Error("Invalid name: use 1–32 lowercase letters, digits or hyphens")
  }
}

export function validateName(name: string) {
  validateSafeName(name)
  if (name === "default") {
    throw new Error("Invalid name: use 1–32 lowercase letters, digits or hyphens; 'default' is reserved")
  }
}

export class ProfileExistsError extends Error {
  constructor(readonly profileName: string) {
    super(`Profile '${profileName}' already exists; it was not overwritten`)
  }
}

export class ProfileMutationError extends Error {
  constructor(readonly kind: "profile_changed" | "not_found" | "rename_partial") { super(kind) }
}

export class ProfileProtectedError extends Error {
  constructor() { super("profile_protected") }
}

export interface ProfileSnapshot { profile: Profile; version: string }

// Tokens include content and file identity: replacing a file with identical JSON
// must also invalidate a pending destructive confirmation.
export async function profileSnapshot(name: string, directory = profileDirectory()): Promise<ProfileSnapshot> {
  validateSafeName(name)
  const path = join(directory, `${name}.json`)
  const before = await entry(path)
  const text = await readBounded(path, MAX_PROFILE_BYTES)
  if (!before || text === undefined) throw new ProfileMutationError("not_found")
  const after = await entry(path)
  if (!after || before.dev !== after.dev || before.ino !== after.ino || before.mtimeMs !== after.mtimeMs || before.ctimeMs !== after.ctimeMs) throw new ProfileMutationError("profile_changed")
  const version = createHash("sha256").update(text).update(JSON.stringify([after.dev, after.ino, after.mtimeMs, after.ctimeMs])).digest("hex")
  return { profile: decode(text, name), version }
}

async function unchanged(name: string, version: string, directory: string) {
  if (typeof version !== "string" || !/^[a-f0-9]{64}$/.test(version)) throw new Error("Invalid profile version")
  const snapshot = await profileSnapshot(name, directory)
  if (snapshot.version !== version) throw new ProfileMutationError("profile_changed")
  return snapshot.profile
}

function encoded(profile: Profile) {
  const text = JSON.stringify(profile, null, 2) + "\n"
  if (Buffer.byteLength(text) > MAX_PROFILE_BYTES) throw new Error("The profile is too large")
  return text
}

export async function editProfile(name: string, version: string, models: Record<string, string>, catalog: Catalog, directory = profileDirectory()) {
  validateSafeName(name)
  if (!object(models) || ![Object.prototype, null].includes(Object.getPrototypeOf(models))) throw new Error("Invalid profile assignments")
  validateModels(models)
  return locked(directory, async () => {
    const original = await unchanged(name, version, directory)
    // Never discard saved entries which disappeared from the visible catalog.
    validateCatalog(original, catalog)
    const required = profileAgents(catalog.agents)
    if (!Object.hasOwn(models, "skynex-orchestrator") || required.some((agent) => !Object.hasOwn(models, agent.id)) ||
        Object.keys(original.models).some((id) => !Object.hasOwn(models, id))) throw new Error("Incomplete profile assignments")
    const profile = { ...original, models: { ...models }, updated_at: new Date().toISOString() }
    validateCatalog(profile, catalog)
    const path = join(directory, `${name}.json`)
    const temporary = await temporaryFile(path, encoded(profile))
    try {
      await unchanged(name, version, directory)
      await rename(temporary, path)
    } finally { await rm(temporary, { force: true }) }
    return profile
  })
}

export async function renameProfile(name: string, version: string, newName: string, catalog: Catalog, directory = profileDirectory()) {
  validateSafeName(name)
  if (name === "default" || newName === "default") throw new ProfileProtectedError()
  validateName(newName)
  return locked(directory, async () => {
    const original = await unchanged(name, version, directory)
    validateCatalog(original, catalog)
    const destination = join(directory, `${newName}.json`)
    if (await entry(destination)) throw new ProfileExistsError(newName)
    const profile = { ...original, name: newName, updated_at: new Date().toISOString() }
    const temporary = await temporaryFile(destination, encoded(profile))
    let published = false
    try {
      try {
        await unchanged(name, version, directory)
        await safeDirectory(directory)
        await link(temporary, destination).catch((error: NodeJS.ErrnoException) => {
          if (error.code === "EEXIST") throw new ProfileExistsError(newName)
          throw error
        })
        published = true
      } finally { await rm(temporary, { force: true }) }
    } catch (error) {
      if (published) throw new ProfileMutationError("rename_partial")
      throw error
    }
    // Cross-file rename is deliberately not advertised as a transaction. Never
    // roll back a published destination that another actor could have changed.
    try {
      await unchanged(name, version, directory)
      await unlink(join(directory, `${name}.json`))
    } catch { throw new ProfileMutationError("rename_partial") }
    return profile
  })
}

export async function deleteProfile(name: string, version: string, directory = profileDirectory()) {
  validateSafeName(name)
  if (name === "default") throw new ProfileProtectedError()
  return locked(directory, async () => {
    await unchanged(name, version, directory)
    await unlink(join(directory, `${name}.json`))
    return { name }
  })
}

// Read-only advisory check: any occupied name is unavailable, even a corrupt
// file or a symlink. Publication still checks under lock and uses atomic link.
export async function checkProfileName(name: string, directory = profileDirectory()): Promise<{ available: boolean }> {
  validateName(name)
  if (!await safeDirectory(directory)) return { available: true }
  return { available: !await entry(join(directory, `${name}.json`)) }
}

function decode(text: string, name: string): Profile {
  const profile: unknown = JSON.parse(text)
  if (!object(profile) || profile.name !== name || !object(profile.models) ||
      typeof profile.created_at !== "string" || !Number.isFinite(Date.parse(profile.created_at)) ||
      typeof profile.updated_at !== "string" || !Number.isFinite(Date.parse(profile.updated_at))) {
    throw new Error(`Invalid profile format: ${name}`)
  }
  if (!Object.values(profile.models).every((model) => typeof model === "string")) throw new Error(`Invalid models: ${name}`)
  validateModels(profile.models as Record<string, string>)
  return { name, created_at: profile.created_at, updated_at: profile.updated_at, models: profile.models as Record<string, string> }
}

export async function readProfile(name: string, directory = profileDirectory()): Promise<Profile> {
  validateSafeName(name)
  const text = await readBounded(join(directory, `${name}.json`), MAX_PROFILE_BYTES)
  if (text === undefined) throw new Error(`Profile '${name}' does not exist`)
  return decode(text, name)
}

async function names(directory: string) {
  if (!await safeDirectory(directory)) return []
  const result: string[] = []
  let entries = 0
  const stream = await opendir(directory)
  for await (const item of stream) {
    if (++entries > 1024) throw new Error("The profiles directory contains too many entries")
    if (!item.name.endsWith(".json")) continue
    const name = item.name.slice(0, -5)
    validateSafeName(name)
    if (!item.isFile() || item.isSymbolicLink()) throw new Error(`Unsafe profile file: ${item.name}`)
    result.push(name)
    if (result.length > MAX_PROFILES) throw new Error(`Maximum of ${MAX_PROFILES} profiles allowed`)
  }
  return result.sort()
}

export async function listProfiles(directory = profileDirectory()): Promise<Profile[]> {
  const result: Profile[] = []
  for (const name of await names(directory)) result.push(await readProfile(name, directory))
  return result.sort((a, b) => a.name === "default" ? -1 : b.name === "default" ? 1 : a.name.localeCompare(b.name))
}

const DEFAULT_ORCHESTRATOR_MODEL = "openai/gpt-5.6-sol-fast#high"
const DEFAULT_SUBAGENT_MODEL = "openai/gpt-5.6-luna-fast#high"

// Seed only when absent. The lock plus exclusive link makes concurrent list
// calls converge without overwriting an existing or invalid default file.
export async function ensureDefaultProfile(catalog: Catalog, directory = profileDirectory()): Promise<Profile> {
  return locked(directory, async () => {
    const path = join(directory, "default.json")
    if (await entry(path)) return readProfile("default", directory)
    if ((await names(directory)).length >= MAX_PROFILES) throw new Error(`Maximum of ${MAX_PROFILES} profiles allowed`)
    const now = new Date().toISOString()
    const models: Record<string, string> = {}
    for (const agent of profileAgents(catalog.agents)) {
      models[agent.id] = agent.id === "skynex-orchestrator" ? DEFAULT_ORCHESTRATOR_MODEL : DEFAULT_SUBAGENT_MODEL
    }
    const profile: Profile = { name: "default", created_at: now, updated_at: now, models }
    const temporary = await temporaryFile(path, encoded(profile))
    try {
      await safeDirectory(directory)
      await link(temporary, path).catch((error: NodeJS.ErrnoException) => {
        if (error.code !== "EEXIST") throw error
      })
    } finally { await rm(temporary, { force: true }) }
    return readProfile("default", directory)
  })
}

// New profiles contain only the wizard's explicit choices, never active assignments.
export async function saveProfile(name: string, models: Record<string, string>, catalog: Catalog, directory = profileDirectory()): Promise<Profile> {
  if (name === "default") throw new ProfileProtectedError()
  validateName(name)
  if (!object(models) || ![Object.prototype, null].includes(Object.getPrototypeOf(models))) throw new Error("Invalid profile assignments")
  validateModels(models)
  const now = new Date().toISOString()
  const profile: Profile = { name, created_at: now, updated_at: now, models: { ...models } }
  validateCatalog(profile, catalog)
  if (!Object.hasOwn(profile.models, "skynex-orchestrator")) throw new Error("Select the model for skynex-orchestrator first")
  const required = profileAgents(catalog.agents)
  if (required.length !== Object.keys(profile.models).length || required.some((agent) => !Object.hasOwn(profile.models, agent.id))) {
    throw new Error("The agent list has changed or is incomplete; create the profile again")
  }
  const text = JSON.stringify(profile, null, 2) + "\n"
  if (Buffer.byteLength(text) > MAX_PROFILE_BYTES) throw new Error("The profile is too large")
  return locked(directory, async () => {
    const path = join(directory, `${name}.json`)
    if (await entry(path)) throw new ProfileExistsError(name)
    if ((await names(directory)).length >= MAX_PROFILES) throw new Error(`Maximum of ${MAX_PROFILES} profiles allowed`)
    const temporary = await temporaryFile(path, text)
    try {
      await safeDirectory(directory)
      // Atomic publication without overwriting even a concurrently created name.
      await link(temporary, path).catch((error: NodeJS.ErrnoException) => {
        if (error.code === "EEXIST") throw new ProfileExistsError(name)
        throw error
      })
    } finally {
      await rm(temporary, { force: true })
    }
    return profile
  })
}

export function validateCatalog(profile: Profile, catalog: Catalog) {
  validateModels(profile.models)
  if (!Object.keys(profile.models).length) throw new Error("The profile contains no explicit assignments")
  for (const [id, reference] of Object.entries(profile.models)) {
    if (!catalog.agents.some((agent) => agent.id === id && !agent.hidden && !agent.id.startsWith("_"))) {
      throw new Error(`Agent '${id}' is no longer available`)
    }
    const selected = parseModel(reference)
    // id is the configured alias; modelID is the upstream identifier.
    const model = catalog.models.find((item) => item.providerID === selected.providerID && item.id === selected.id && item.enabled)
    if (!model) throw new Error(`Model '${reference}' for '${id}' is no longer available`)
    if (selected.variant && !model.variants.some((variant) => variant.id === selected.variant)) {
      throw new Error(`Variant '${selected.variant}' for '${id}' is no longer available`)
    }
  }
}

export async function applyProfile(
  name: string,
  configDirectory: string,
  catalog: () => Promise<Catalog>,
  directory = profileDirectory(),
  expected?: Profile,
): Promise<{ path: string; profile: Profile }> {
  validateSafeName(name)
  return locked(configDirectory, async () => {
    const profile = await readProfile(name, directory)
    if (expected && JSON.stringify(profile) !== JSON.stringify(expected)) throw new Error("The profile has changed since the preview; select it again")
    await validateCatalog(profile, await catalog())
    return { path: await writeGlobalModels(configDirectory, profile.models), profile }
  })
}
