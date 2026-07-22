/** @jsxImportSource @opentui/solid */
import type { TuiPlugin, TuiPluginModule, TuiSlotPlugin } from "@opencode-ai/plugin/tui"

// Skynex wordmark rendered on the TUI home screen, replacing the default
// OpenCode logo. ANSI Shadow font; each entry is one row of the banner.
const WORDMARK = [
  "███████╗██╗  ██╗██╗   ██╗███╗   ██╗███████╗██╗  ██╗",
  "██╔════╝██║ ██╔╝╚██╗ ██╔╝████╗  ██║██╔════╝╚██╗██╔╝",
  "███████╗█████╔╝  ╚████╔╝ ██╔██╗ ██║█████╗   ╚███╔╝ ",
  "╚════██║██╔═██╗   ╚██╔╝  ██║╚██╗██║██╔══╝   ██╔██╗ ",
  "███████║██║  ██╗   ██║   ██║ ╚████║███████╗██╔╝ ██╗",
  "╚══════╝╚═╝  ╚═╝   ╚═╝   ╚═╝  ╚═══╝╚══════╝╚═╝  ╚═╝",
]

const branding: TuiSlotPlugin = {
  slots: {
    home_logo(ctx) {
      const accent = ctx.theme.current.primary
      return (
        <box flexDirection="column">
          {WORDMARK.map((line) => (
            <text fg={accent}>{line}</text>
          ))}
        </box>
      )
    },
  },
}

const tui: TuiPlugin = async (api) => {
  api.slots.register(branding)
}

const plugin: TuiPluginModule & { id: string } = {
  id: "skynex-branding",
  tui,
}

export default plugin
