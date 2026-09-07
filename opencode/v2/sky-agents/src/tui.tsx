import { Plugin } from "@opencode-ai/plugin/tui"
import type { AgentInfo } from "@opencode-ai/client"
import { isProfileExists, managementErrorMessage, SkyAgents } from "./rpc"
import { manageProfile } from "./manage-profile"
import type { Profile } from "./profiles"
import { modelOptions, profileAgents } from "./catalog"
import { confirmProfile } from "./confirm-profile"

export default Plugin.define({
  id: "skynex-sky-agents",
  setup(context) {
    const open = async () => {
      const location = context.location ?? context.data.location.default()
      try {
        const action = await context.ui.dialog.select({
          title: "Sky Agents · Global models",
          options: [
            { title: "Assign a model to an agent", value: "agent" },
            { title: "New profile", value: "save", description: "Choose models for the orchestrator and subagents, then review before saving" },
            { title: "List profiles", value: "list" },
          ],
        })
        if (!action) return
         const rpc = context.client.rpc(SkyAgents)
        if (action === "save") {
          const chooseName = async (description = "New name: 1–32 lowercase letters, digits or hyphens. 'default' is reserved. Active models will not change."): Promise<string | undefined> => {
            while (true) {
              const candidate = await context.ui.dialog.prompt({ title: "New profile", description, placeholder: "my-profile" })
              if (candidate === undefined) return undefined
              if (!/^[a-z0-9-]{1,32}$/.test(candidate) || candidate === "default") {
                description = "Invalid name: use 1–32 lowercase letters, digits or hyphens; 'default' is reserved. Choose another name."
                continue
              }
              const result = await rpc.checkProfileName({ name: candidate }, { location }) as { available: boolean }
              if (result.available === true) return candidate
              description = `Profile '${candidate}' already exists. Choose another name; it will not be overwritten.`
            }
          }
          let name = await chooseName()
          if (name === undefined) return
          await Promise.all([
            context.data.location.agent.sync(location),
            context.data.location.model.sync(location),
          ])
          const agents = profileAgents(context.data.location.agent.list(location) ?? [])
          if (!agents.some((agent) => agent.id === "skynex-orchestrator")) throw new Error("skynex-orchestrator is not available")
          const options = modelOptions(context.data.location.model.list(location) ?? [])
          if (!options.length) throw new Error("No enabled models are available")
          const orchestrator = await context.ui.dialog.select<string>({
            title: "New profile · 1 · skynex-orchestrator",
            placeholder: "Search by name, provider or configured alias",
            options,
          })
          if (orchestrator === undefined) return
          const models: Record<string, string> = Object.create(null)
          models["skynex-orchestrator"] = orchestrator
          const inherited = new Set<string>()
          for (const [index, agent] of agents.filter((agent) => agent.id !== "skynex-orchestrator").entries()) {
            const current = agent.model ? `${agent.model.providerID}/${agent.model.id}${agent.model.variant ? `#${agent.model.variant}` : ""}` : undefined
            const currentOption = current && options.some((option) => option.value === current)
              ? [{ title: `Current / recent model · ${current}`, value: current, description: "This agent's effective model", category: "Inheritance" }]
              : []
            const selected = await context.ui.dialog.select<string>({
              title: `New profile · ${index + 2}/${agents.length} · ${agent.name || agent.id} (${agent.id})`,
              placeholder: "Search by name, provider or configured alias",
              options: [
                { title: "Inherit from orchestrator", value: "inherit", description: orchestrator, category: "Inheritance" },
                ...currentOption,
                ...options,
              ],
            })
            if (selected === undefined) return
            if (selected === "inherit") inherited.add(agent.id)
            models[agent.id] = selected === "inherit" ? orchestrator : selected
          }
          // A navigable review keeps every assignment accessible on short terminals.
          while (true) {
            const review = await context.ui.dialog.select<string>({
              title: `Review new profile '${name}' · ${agents.length} assignments`,
              placeholder: "Search assignments · Esc cancels without saving",
              options: [
                ...Object.entries(models).map(([id, model]) => ({
                  title: `${id} → ${model}${inherited.has(id) ? " (inherited from orchestrator)" : ""}`,
                  value: id,
                  description: "View full reference",
                  category: "Assignments",
                })),
                { title: "Save profile", value: "", description: "Inherited models are saved as explicit assignments. Active models will not change.", category: "Actions" },
              ],
            })
            if (review === undefined) return
            if (review === "") {
              try {
                const profile = await rpc.saveProfile({ name, models }, { location }) as Profile
                context.ui.toast.show({ variant: "success", title: "Sky Agents", message: `Profile '${profile.name}' saved: ${Object.keys(profile.models).length} assignments. Active models have not changed.` })
                return
              } catch (error) {
                if (!isProfileExists(error, name)) throw error
                name = await chooseName(`Profile '${name}' already exists; it was not overwritten. Choose another name. Your selections have been kept.`)
                if (name === undefined) return
                continue
              }
            }
            await context.ui.dialog.alert({
              title: review,
              message: `${models[review]}${inherited.has(review) ? "\nInherited from orchestrator; will be saved as an explicit reference." : ""}`,
            })
          }
        }
        if (action === "list") {
          const profiles = await rpc.listProfiles({}, { location }) as Profile[]
          if (!profiles.length) {
            context.ui.toast.show({ variant: "info", title: "Sky Agents", message: "No saved profiles. Use New profile to create one." })
            return
          }
          const selected = await context.ui.dialog.select<Profile>({
            title: "Saved profiles",
             options: profiles.map((profile) => ({ title: profile.name === "default" ? "default (protected)" : profile.name, value: profile, description: `${Object.keys(profile.models).length} agents · ${profile.updated_at}` })),
          })
          if (!selected) return
          let profileAction: string | undefined
          while (true) {
            profileAction = await context.ui.dialog.select<string>({
               title: `Profile: ${selected.name}${selected.name === "default" ? " (protected)" : ""}`,
              options: [
                { title: "View configuration", value: "view", description: "Show saved agent assignments" },
                { title: "Edit", value: "edit", description: "Review saved assignments and explicitly save changes" },
                ...(selected.name === "default" ? [] : [
                  { title: "Rename", value: "rename", description: "Confirm the old and new names" },
                  { title: "Delete", value: "delete", description: "Review and confirm deletion; active models will not change" },
                ]),
                { title: "Apply", value: "apply", description: "Preview and confirm before saving" },
              ],
            })
            if (profileAction !== "view") break
            await context.ui.dialog.alert({ title: `Profile: ${selected.name}`, message: Object.entries(selected.models).map(([id, model]) => `${id} → ${model}`).join("\n") || "No assignments" })
          }
          if (profileAction === undefined) return
          if (profileAction === "edit" || profileAction === "rename" || profileAction === "delete") {
            return await manageProfile(context, profileAction, selected.name)
          }
          if (profileAction !== "apply") return
          const previewStarted = Date.now()
          // The list entry is only a choice; preview fetches a fresh server snapshot.
          const preview = await rpc.previewProfile({ name: selected.name }, { location }) as { profile: Profile; current: Record<string, string> }
          const confirmed = await confirmProfile(context, selected.name, preview)
          if (confirmed !== true) return
          if (Date.now() - previewStarted >= 5 * 60_000) throw new Error("The preview has expired; select and confirm the profile again")
          const saved = await rpc.applyProfile({ name: selected.name }, { location }) as { path: string; profile: Profile }
          const message = `Profile '${saved.profile.name}' saved globally: ${Object.keys(saved.profile.models).length} agents.\n${saved.path}`
          try {
            let different: string[] = []
            for (let attempt = 0; attempt < 20; attempt++) {
              await context.data.location.agent.sync(location)
              const effective = context.data.location.agent.list(location) ?? []
              different = Object.entries(saved.profile.models).filter(([id, reference]) => {
                const model = effective.find((agent) => agent.id === id)?.model
                return !model || `${model.providerID}/${model.id}${model.variant ? `#${model.variant}` : ""}` !== reference
              }).map(([id]) => id)
              if (!different.length) {
                context.ui.toast.show({ variant: "success", title: "Sky Agents", message })
                return
              }
              await new Promise((resolve) => setTimeout(resolve, 250))
            }
            context.ui.toast.show({ variant: "warning", title: "Sky Agents", message: `${message}\nEffective model differs for: ${different.join(", ")}. A project override or pending reload may be the cause.` })
          } catch {
            context.ui.toast.show({ variant: "warning", title: "Sky Agents", message: `${message}\nCould not verify that the agents reloaded.` })
          }
          return
        }
        await Promise.all([
          context.data.location.agent.sync(location),
          context.data.location.model.sync(location),
        ])
        const agents = (context.data.location.agent.list(location) ?? []).filter(
          (agent) => !agent.hidden && !agent.id.startsWith("_"),
        )
        const agent = await context.ui.dialog.select<AgentInfo>({
          title: "Sky Agents · Select an agent",
          placeholder: "Search agents or subagents",
          options: agents.map((item) => ({
            title: item.name || item.id,
            value: item,
            description: item.model
              ? `${item.model.providerID}/${item.model.id}${item.model.variant ? `#${item.model.variant}` : ""}`
              : "Inherits the default model",
            category: item.mode === "subagent" ? "Subagents" : "Agents",
          })),
        })
        if (!agent) return

        const options = modelOptions(context.data.location.model.list(location) ?? [])
        if (!options.length) throw new Error("No enabled models are available")
        const reference = await context.ui.dialog.select<string>({
          title: `Model for ${agent.id}`,
          placeholder: "Search by name, provider or configured alias",
          options,
          current: agent.model ? `${agent.model.providerID}/${agent.model.id}${agent.model.variant ? `#${agent.model.variant}` : ""}` : undefined,
        })
        if (reference === undefined) return
        const slash = reference.indexOf("/")
        const [modelID, variant] = reference.slice(slash + 1).split("#")
        const providerID = reference.slice(0, slash)

        const saved = await context.client.rpc(SkyAgents).save({
          agentID: agent.id,
          providerID,
          modelID,
          ...(variant === undefined ? {} : { variant }),
        }, { location }) as { path: string }
        const message = `Saved globally: ${agent.id} → ${reference}\n${saved.path}`
        try {
          // The server watches config files; wait for its authoritative agent state.
          for (let attempt = 0; attempt < 20; attempt++) {
            await context.data.location.agent.sync(location)
            const active = context.data.location.agent.list(location)?.find((item) => item.id === agent.id)?.model
            if (active?.providerID === providerID && active.id === modelID && (active.variant || undefined) === variant) {
              context.ui.toast.show({ variant: "success", title: "Sky Agents", message })
              return
            }
            await new Promise((resolve) => setTimeout(resolve, 250))
          }
          context.ui.toast.show({
            variant: "warning",
            title: "Sky Agents",
            message: `${message}\nThe effective model still differs: a project override or pending reload may be the cause.`,
          })
        } catch {
          context.ui.toast.show({ variant: "warning", title: "Sky Agents", message: `${message}\nCould not verify that the agent reloaded.` })
        }
      } catch (error) {
        context.ui.toast.show({
          variant: "error",
          title: "Sky Agents",
          message: managementErrorMessage(error) ?? "Could not complete the Sky Agents operation. Check that the catalog and profiles are available, then try again.",
        })
      }
    }

    return context.ui.slot({
      append: "app",
      render: () => {
        // Keymap requires the Solid owner provided when the app slot mounts.
        context.keymap.layer(() => ({
          mode: "global",
          priority: 10,
          commands: [{
            id: "skynex.sky-agents",
            title: "Configure agent models",
            description: "Assign models and save or apply global profiles",
            group: "Skynex",
            palette: true,
            slash: { name: "sky-agents" },
            suggested: true,
            run: open,
          }],
        }))
        return null
      },
    })
  },
})
