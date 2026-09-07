/** @jsxImportSource @opentui/solid */
import type { Context } from "@opencode-ai/plugin/tui/context"
import { TextAttributes, type ScrollBoxRenderable } from "@opentui/core"
import { useTerminalDimensions } from "@opentui/solid"
import { createSignal, For } from "solid-js"
import type { Profile } from "./profiles"

export function confirmProfile(context: Context, name: string, preview: { profile: Profile; current: Record<string, string> }): Promise<boolean | undefined> {
  // The host confirm message is plain text. Use native text attributes instead.
  return new Promise((resolve) => {
    let settled = false
    const settle = (value: boolean | undefined) => {
      if (settled) return
      settled = true
      resolve(value)
    }
    const finish = (value: boolean | undefined) => {
      settle(value)
      context.ui.dialog.clear()
    }
    context.ui.dialog.show(() => {
      context.ui.dialog.set({ size: "large", centered: true })
      const dimensions = useTerminalDimensions()
      const [active, setActive] = createSignal<"confirm" | "cancel">("confirm")
      let scroll: ScrollBoxRenderable | undefined
      const toggle = () => { setActive((value) => value === "confirm" ? "cancel" : "confirm") }
      // Register inside the mounted dialog owner, like the host confirmation.
      context.keymap.layer(() => ({
        mode: "modal",
        commands: [
          { bind: "return", run: () => finish(active() === "confirm") },
          { bind: "left", run: toggle },
          { bind: "right", run: toggle },
          { bind: "tab", run: toggle },
          { bind: "up", run: () => { scroll?.scrollBy(-1) } },
          { bind: "down", run: () => { scroll?.scrollBy(1) } },
          { bind: "pageup", run: () => { scroll?.scrollBy(-1, "viewport") } },
          { bind: "pagedown", run: () => { scroll?.scrollBy(1, "viewport") } },
        ],
      }))
      return (
        <box paddingLeft={2} paddingRight={2} paddingBottom={1} gap={1}>
          <box flexDirection="row" justifyContent="space-between" flexShrink={0}>
            <text fg={context.theme.text.default} attributes={TextAttributes.BOLD} flexShrink={1} minWidth={0} maxHeight={2}>{`Apply profile '${name}' globally`}</text>
            <text fg={context.theme.text.subdued} flexShrink={0} onMouseUp={() => finish(undefined)}>esc</text>
          </box>
          <scrollbox ref={(value) => { scroll = value }} height={Math.max(1, Math.min(Math.floor(dimensions().height / 2), dimensions().height - 10))} scrollX={false}>
            <box gap={1}>
              <For each={Object.entries(preview.profile.models)}>{([id, model]) => (
                <box flexShrink={0}>
                  <text fg={context.theme.text.default} attributes={TextAttributes.BOLD}>{id}</text>
                  <text fg={context.theme.text.subdued} wrapMode="word">{`Current: ${preview.current[id] ?? "No global assignment (inherited)"}`}</text>
                  <text fg={context.theme.text.default} attributes={TextAttributes.BOLD} wrapMode="word">{`→ New: ${model}`}</text>
                </box>
              )}</For>
              <text fg={context.theme.text.subdued} flexShrink={0} wrapMode="word">Only these agents will be updated. Open sessions keep their selection. A project can override the global model.</text>
              <text fg={context.theme.text.default} attributes={TextAttributes.BOLD} flexShrink={0}>Save these assignments?</text>
            </box>
          </scrollbox>
          <text fg={context.theme.text.subdued} flexShrink={0} wrapMode="none" truncate>↑↓ / PgUp / PgDn: Review · ←→ / Tab: Choose · Enter: Confirm</text>
          <box flexDirection="row" justifyContent="flex-end" flexShrink={0}>
            <For each={["cancel", "confirm"] as const}>{(action) => (
              <box paddingLeft={1} paddingRight={1}
                backgroundColor={active() === action ? context.theme.background.action.primary.focused : undefined}
                onMouseUp={() => finish(action === "confirm")}>
                <text attributes={TextAttributes.BOLD} fg={active() === action ? context.theme.text.action.primary.focused : context.theme.text.subdued}>
                  {action === "confirm" ? "Apply profile" : "Cancel"}
                </text>
              </box>
            )}</For>
          </box>
        </box>
      )
    }, () => settle(undefined))
  })
}
