import type { Context } from "@opencode-ai/plugin/tui/context"
import { modelOptions, profileAgents } from "./catalog"
import type { Profile, ProfileSnapshot } from "./profiles"
import { SkyAgents } from "./rpc"

export async function manageProfile(context: Context, action: "edit" | "rename" | "delete", selectedName?: string) {
  const location = context.location ?? context.data.location.default()
  const rpc = context.client.rpc(SkyAgents)
  const profiles = await rpc.listProfiles({}, { location }) as Profile[]
  if (!profiles.length) {
    await context.ui.dialog.alert({ title: "Saved profiles", message: "No saved profiles. Use New profile to create one." })
    return
  }
  const name = selectedName ?? await context.ui.dialog.select<string>({
    title: `${action === "edit" ? "Edit" : action === "rename" ? "Rename" : "Delete"} profile`,
    options: profiles.map((profile) => ({ title: profile.name, value: profile.name, description: `${Object.keys(profile.models).length} assignments · ${profile.updated_at}` })),
  })
  if (name === undefined) return
  const { profile, version } = await rpc.inspectProfile({ name }, { location }) as ProfileSnapshot
  let newName = name
  const models = { ...profile.models }
  const inherited = new Set<string>()
  if (action === "rename") {
    let description = "Use 1–32 lowercase letters, digits or hyphens; 'default' is reserved."
    while (true) {
      const candidate = await context.ui.dialog.prompt({ title: `Rename profile '${name}'`, description, placeholder: "new-profile-name" })
      if (candidate === undefined) return
      if (!/^[a-z0-9-]{1,32}$/.test(candidate) || candidate === "default") {
        description = "Invalid name. Use 1–32 lowercase letters, digits or hyphens; 'default' is reserved."
        continue
      }
      newName = candidate
      break
    }
  }
  if (action === "edit") {
    await Promise.all([context.data.location.agent.sync(location), context.data.location.model.sync(location)])
    const visible = (context.data.location.agent.list(location) ?? []).filter((agent) => !agent.hidden && !agent.id.startsWith("_"))
    const options = modelOptions(context.data.location.model.list(location) ?? [])
    if (Object.entries(models).some(([id, model]) => !visible.some((agent) => agent.id === id) || !options.some((option) => option.value === model))) {
      await context.ui.dialog.alert({ title: "Cannot edit this profile", message: "A saved agent, model alias or variant is no longer available. Editing is blocked so saved assignments are not silently dropped. Restore catalog availability before editing." })
      return
    }
    const required = profileAgents(visible)
    const orchestratorAgent = visible.find((agent) => agent.id === "skynex-orchestrator")
    if (!orchestratorAgent || !options.length) {
      await context.ui.dialog.alert({ title: "Cannot edit this profile", message: "The orchestrator and enabled models must be available." })
      return
    }
    const agents = [orchestratorAgent, ...visible.filter((agent) => agent.id !== "skynex-orchestrator" && (Object.hasOwn(models, agent.id) || required.some((item) => item.id === agent.id)))]
    for (const agent of agents) {
      const saved = models[agent.id]
      const orchestrator = models["skynex-orchestrator"]
      const current = agent.model ? `${agent.model.providerID}/${agent.model.id}${agent.model.variant ? `#${agent.model.variant}` : ""}` : undefined
      const canInherit = agent.id !== "skynex-orchestrator"
      const selected = await context.ui.dialog.select<string>({
        title: `Edit '${name}' · ${agent.id}${saved ? " · Saved selection" : " · New assignment: review required"}`,
        placeholder: "Saved choices are retained unless explicitly changed · Esc cancels",
        current: saved ?? (canInherit ? "inherit" : undefined),
        options: [
          ...(canInherit ? [{ title: "Inherit from selected orchestrator", value: "inherit", description: orchestrator, category: "Inheritance" }] : []),
          ...(current && options.some((option) => option.value === current) ? [{ title: `Current / recent model · ${current}`, value: current, description: "This agent's effective model; select explicitly to copy", category: "Current" }] : []),
          ...options,
        ],
      })
      if (selected === undefined) return
      models[agent.id] = selected === "inherit" ? orchestrator : selected
      if (selected === "inherit") inherited.add(agent.id)
    }
  }
  const title = action === "rename" ? `Rename '${name}' → '${newName}'` : `${action === "delete" ? "Delete" : "Review changes to"} profile '${name}'`
  // Searchable summary stays usable on small terminals. Cancel is the initial
  // selection; assignment rows only open details, never trigger a mutation.
  while (true) {
    const selected = await context.ui.dialog.select<string>({
      title,
      current: "cancel",
      placeholder: "Review exact assignments · Esc cancels",
      options: [
        { title: "Cancel", value: "cancel", category: "Actions" },
        ...Object.entries(models).map(([id, model]) => ({ title: `${id} → ${model}${inherited.has(id) ? " (inherited explicitly)" : ""}`, value: `agent:${id}`, description: action === "edit" ? `Saved: ${profile.models[id] ?? "Absent (new assignment)"}` : "Saved assignment", category: "Assignments" })),
        { title: action === "edit" ? "Save changes" : action === "rename" ? "Confirm rename" : `Delete profile '${name}'`, value: "confirm", description: "Active models and sessions will not change. Deletion does not undo previously applied models.", category: "Actions" },
      ],
    })
    if (selected === undefined || selected === "cancel") return
    if (selected !== "confirm") {
      const id = selected.slice(6)
      await context.ui.dialog.alert({ title: id, message: `Saved: ${profile.models[id] ?? "Absent"}\n${action === "edit" ? "Proposed" : "Model"}: ${models[id]}` })
      continue
    }
    if (action === "edit") await rpc.editProfile({ name, version, models }, { location })
    else if (action === "rename") await rpc.renameProfile({ name, version, newName }, { location })
    else await rpc.deleteProfile({ name, version }, { location })
    context.ui.toast.show({ variant: "success", title: "Sky Agents", message: action === "rename" ? `Profile '${name}' renamed to '${newName}'. Active models have not changed.` : `Profile '${name}' ${action === "edit" ? "updated" : "deleted"}. Active models have not changed.` })
    return
  }
}
