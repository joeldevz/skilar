import { Plugin } from "@opencode-ai/plugin"
import { homedir } from "node:os"
import { join } from "node:path"
import { SkyAgents } from "./rpc"
import { explicitModels, readGlobalConfig, saveGlobalModel, type Assignment } from "./save-model"
import { applyProfile, checkProfileName, ensureDefaultProfile, listProfiles, MAX_PROFILES, ProfileExistsError, readProfile, saveProfile, validateCatalog, type Profile } from "./profiles"
import { deleteProfile, editProfile, profileSnapshot, ProfileMutationError, ProfileProtectedError, renameProfile } from "./profiles"

export default Plugin.define({
  id: "skynex-sky-agents.server",
  async setup(context) {
    const directory = process.env.OPENCODE_CONFIG_DIR ?? join(process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config"), "opencode")
    const previews = new Map<string, { profile: Profile; expires: number; consumed: boolean }>()
    const catalog = async () => {
      const [agents, models] = await Promise.all([context.agent.list(), context.catalog.model.list()])
      return { agents: agents.data, models: models.data }
    }
    const registration = await context.rpc.register(SkyAgents, {
      async inspectProfile(input, ctx) {
        try {
          const name = (input as { name: string }).name
          if (name === "default") await ensureDefaultProfile(await catalog())
          return await profileSnapshot(name)
        }
        catch (error) {
          if (error instanceof ProfileMutationError) return ctx.error(error.kind, "Profile snapshot unavailable", {})
          return ctx.error("invalid_profile", "Profile could not be read safely", {})
        }
      },
      async editProfile(input, ctx) {
        const { name, version, models } = input as { name: string; version: string; models: Record<string, string> }
        try { return await editProfile(name, version, models, await catalog()) }
        catch (error) {
          if (error instanceof ProfileMutationError) return ctx.error(error.kind, "Profile operation refused", {})
          return ctx.error("invalid_profile", "Check profile safety and complete available assignments", {})
        }
      },
      async renameProfile(input, ctx) {
        const { name, version, newName } = input as { name: string; version: string; newName: string }
        try { return await renameProfile(name, version, newName, await catalog()) }
        catch (error) {
          if (error instanceof ProfileProtectedError) return ctx.error("profile_protected", "The default profile is protected", {})
          if (error instanceof ProfileExistsError) return ctx.error("profile_exists", "Destination already exists", { name: error.profileName })
          if (error instanceof ProfileMutationError) return ctx.error(error.kind, "Profile operation refused", {})
          return ctx.error("invalid_profile", "Profile rename refused", {})
        }
      },
      async deleteProfile(input, ctx) {
        const { name, version } = input as { name: string; version: string }
        try { return await deleteProfile(name, version) }
        catch (error) {
          if (error instanceof ProfileProtectedError) return ctx.error("profile_protected", "The default profile is protected", {})
          if (error instanceof ProfileMutationError) return ctx.error(error.kind, "Profile operation refused", {})
          return ctx.error("invalid_profile", "Profile deletion refused", {})
        }
      },
      async checkProfileName(input) {
        const { name } = input as { name: string }
        return checkProfileName(name)
      },
      async saveProfile(input, ctx) {
        const { name, models } = input as { name: string; models: Record<string, string> }
        try {
          return await saveProfile(name, models, await catalog())
        } catch (error) {
          if (error instanceof ProfileProtectedError) return ctx.error("profile_protected", "The default profile is protected", {})
          if (error instanceof ProfileExistsError) {
            return ctx.error("profile_exists", "That profile name already exists; choose another name. It was not overwritten.", { name: error.profileName })
          }
          return ctx.error("invalid_profile", "Profile save refused", {})
        }
      },
      async listProfiles(_input, ctx) {
        try { return await ensureDefaultProfile(await catalog()).then(() => listProfiles()) }
        catch { return ctx.error("invalid_profile", "Profiles could not be listed safely", {}) }
      },
      async previewProfile(input) {
        const { name } = input as { name: string }
        if (name === "default") await ensureDefaultProfile(await catalog())
        const profile = await readProfile(name)
        validateCatalog(profile, await catalog())
        const current = explicitModels((await readGlobalConfig(directory)).agents)
        for (const [key, value] of previews) if (value.expires <= Date.now()) previews.delete(key)
        if (previews.size >= MAX_PROFILES && !previews.has(name)) throw new Error("Too many previews; try again later")
        const previous = previews.get(name)
        if (previous && JSON.stringify(previous.profile) !== JSON.stringify(profile)) {
          throw new Error("The profile changed while confirmation was pending; wait five minutes and select it again")
        }
        previews.set(name, { profile, expires: Date.now() + 5 * 60_000, consumed: false })
        return { profile, current }
      },
      async applyProfile(input) {
        const { name } = input as { name: string }
        const preview = previews.get(name)
        if (!preview || preview.consumed || preview.expires <= Date.now()) throw new Error("The preview has expired; select and confirm the profile again")
        // Retain the snapshot until expiry, so another client cannot replace an
        // outstanding name-only confirmation with changed profile contents.
        preview.consumed = true
        return applyProfile(name, directory, catalog, undefined, preview.profile)
      },
      async save(input) {
        const assignment = input as Assignment
        const agents = await context.agent.list()
        if (!agents.data.some((agent) => agent.id === assignment.agentID && !agent.hidden)) {
          throw new Error("The selected agent is no longer available")
        }
        const models = await context.catalog.model.list()
        if (!models.data.some((model) => model.providerID === assignment.providerID && model.id === assignment.modelID && model.enabled &&
            (assignment.variant === undefined || model.variants.some((variant) => variant.id === assignment.variant)))) {
          throw new Error("The selected model is no longer available")
        }
        return { path: await saveGlobalModel(directory, assignment) }
      },
    })
    return () => registration.dispose()
  },
})
