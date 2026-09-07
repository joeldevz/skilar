import { Rpc } from "@opencode-ai/plugin/rpc"

const nameInput = {
  type: "object",
  // beta-19234 rejects JSON Schema pattern during RPC conversion.
  // profiles.validateName enforces the safe-name rule before every filesystem access.
  properties: { name: { type: "string", minLength: 1, maxLength: 32 } },
  required: ["name"],
  additionalProperties: false,
} as const

const profileOutput = {
  type: "object",
  properties: {
    name: { type: "string" },
    created_at: { type: "string" },
    updated_at: { type: "string" },
    models: { type: "object", additionalProperties: { type: "string" } },
  },
  required: ["name", "created_at", "updated_at", "models"],
  additionalProperties: false,
} as const

const empty = { type: "object", properties: {}, additionalProperties: false } as const
const managementErrors = {
  profile_exists: nameInput,
  profile_changed: empty,
  not_found: empty,
  invalid_profile: empty,
  rename_partial: empty,
  profile_protected: empty,
} as const
const version = { type: "string", minLength: 64, maxLength: 64 } as const
const versionInput = {
  type: "object", properties: { name: nameInput.properties.name, version },
  required: ["name", "version"], additionalProperties: false,
} as const

export const SkyAgents = Rpc.define({
  id: "skynex.sky-agents",
  events: {},
  methods: {
    inspectProfile: {
      input: nameInput,
      output: { type: "object", properties: { profile: profileOutput, version }, required: ["profile", "version"], additionalProperties: false },
      errors: managementErrors,
    },
    editProfile: {
      input: { ...versionInput, properties: { ...versionInput.properties, models: profileOutput.properties.models }, required: ["name", "version", "models"] },
      output: profileOutput, errors: managementErrors,
    },
    renameProfile: {
      input: { ...versionInput, properties: { ...versionInput.properties, newName: nameInput.properties.name }, required: ["name", "version", "newName"] },
      output: profileOutput, errors: managementErrors,
    },
    deleteProfile: { input: versionInput, output: nameInput, errors: managementErrors },
    checkProfileName: {
      input: nameInput,
      output: {
        type: "object",
        properties: { available: { type: "boolean" } },
        required: ["available"],
        additionalProperties: false,
      },
    },
    saveProfile: {
      input: {
        type: "object",
        properties: {
          name: nameInput.properties.name,
          models: { type: "object", additionalProperties: { type: "string" } },
        },
        required: ["name", "models"],
        additionalProperties: false,
      },
      output: profileOutput,
      errors: managementErrors,
    },
    listProfiles: {
      input: { type: "object", properties: {}, additionalProperties: false },
      output: { type: "array", items: profileOutput },
      errors: managementErrors,
    },
    previewProfile: {
      input: nameInput,
      output: {
        type: "object",
        properties: { profile: profileOutput, current: { type: "object", additionalProperties: { type: "string" } } },
        required: ["profile", "current"],
        additionalProperties: false,
      },
    },
    applyProfile: {
      input: nameInput,
      output: {
        type: "object",
        properties: { path: { type: "string" }, profile: profileOutput },
        required: ["path", "profile"],
        additionalProperties: false,
      },
    },
    save: {
      input: {
        type: "object",
        properties: {
          agentID: { type: "string", minLength: 1 },
          providerID: { type: "string", minLength: 1 },
          modelID: { type: "string", minLength: 1 },
          variant: { type: "string", minLength: 1 },
        },
        required: ["agentID", "providerID", "modelID"],
        additionalProperties: false,
      },
      output: {
        type: "object",
        properties: { path: { type: "string" } },
        required: ["path"],
        additionalProperties: false,
      },
    },
  },
})

export function managementErrorMessage(error: unknown): string | undefined {
  if (!error || typeof error !== "object" || !("type" in error) || typeof error.type !== "string") return
  const messages: Record<string, string> = {
    profile_exists: "That profile name already exists; choose another name. Nothing was overwritten.",
    profile_changed: "The profile changed since it was opened. Reopen it and review again; your operation was refused.",
    not_found: "The profile no longer exists. Refresh the profile list.",
    invalid_profile: "The profile operation was refused. Check the name, file safety, and complete assignments with available agents, model aliases and variants; another operation may hold the directory lock.",
    rename_partial: "The new profile was published, but the old profile could not be removed safely. Refresh the list and inspect both names before any further action. No rollback was attempted.",
    profile_protected: "The default profile is protected and cannot be renamed or deleted.",
  }
  return Object.hasOwn(messages, error.type) ? messages[error.type] : undefined
}

// Promise RPC errors are plain objects. Validate the declared payload and use
// local copy for UI text rather than displaying arbitrary transport messages.
export function isProfileExists(error: unknown, name: string): boolean {
  if (error === null || typeof error !== "object" || !("type" in error) ||
      error.type !== "profile_exists" || !("data" in error)) return false
  const data = error.data
  return data !== null && typeof data === "object" && "name" in data &&
    data.name === name && /^[a-z0-9-]{1,32}$/.test(name) && name !== "default"
}
