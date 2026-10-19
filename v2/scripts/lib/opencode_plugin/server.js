/**
 * AMP for opencode — one plugin instead of copied files.
 *
 * Registers AMP's agent team, skills, MCP servers, OpenRouter model settings
 * (capabilities and thinking variants) and the amp_session_usage tool.
 * Everything comes from the files next to this one, generated from AMP's
 * repo (v2/.opencode, v2/opencode.json) by v2/scripts/build-opencode-plugin.sh.
 *
 * Options (opencode.json "plugins": [{ "package": "...", "options": {...} }]):
 *   url            AMP's MCP endpoint (default $AMP_MCP_URL, else the hosted AMP)
 *   token          an AMP API token; omit to sign in with OAuth ($AMP_TOKEN also works)
 *   models         "openrouter" (default): each agent on the model AMP picked for it;
 *                  "inherit": every agent uses your default model
 *   context7       true (default): add the context7 docs MCP server
 *   chromeDevtools true (default): add chrome-devtools MCP (headless) for visual checks
 *
 * opencode 2 plugin: default export { id, setup }. Tool schemas use zod.
 */
import { readFileSync } from "node:fs"
import { dirname, extname, isAbsolute, join, resolve } from "node:path"
import { fileURLToPath } from "node:url"
import { z } from "zod"

const here = dirname(fileURLToPath(import.meta.url))
const read = (f) => JSON.parse(readFileSync(join(here, f), "utf8"))
const DEFAULT_URL = "https://amp.simstech.cloud/mcp"
const KINDS = { ".html": "html", ".htm": "html", ".svg": "svg", ".png": "image", ".jpg": "image", ".jpeg": "image",
  ".webp": "image", ".gif": "image", ".md": "markdown", ".pdf": "pdf", ".txt": "text" }

// Calls one AMP MCP tool over HTTP (stateless 2026-07-28 protocol) with the token.
async function callAmp(url, token, name, args) {
  const meta = { "io.modelcontextprotocol/protocolVersion": "2026-07-28",
    "io.modelcontextprotocol/clientInfo": { name: "amp-opencode", version: "1" }, "io.modelcontextprotocol/clientCapabilities": {} }
  const res = await fetch(url, { method: "POST", signal: AbortSignal.timeout(120000), headers: {
    "Content-Type": "application/json", Accept: "application/json, text/event-stream", Authorization: `Bearer ${token}`,
    "MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": name },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name, arguments: args, _meta: meta } }) })
  let raw = await res.text()
  const data = raw.split("\n").find((l) => l.startsWith("data: "))
  if (data) raw = data.slice(6)
  const msg = JSON.parse(raw)
  if (msg.error) throw new Error(msg.error.message)
  const text = msg.result.content[0].text
  if (msg.result.isError) throw new Error(text)
  return JSON.parse(text)
}

export default {
  id: "amp",
  async setup(ctx) {
    const opts = ctx.options ?? {}
    const agents = read("agents.json")
    const skills = read("skills.json")
    const models = read("models.json")
    const url = opts.url || process.env.AMP_MCP_URL || DEFAULT_URL
    const token = opts.token || process.env.AMP_TOKEN || ""
    const inherit = opts.models === "inherit"

    // Agents: update() on a new id creates it.
    await ctx.agent.transform((editor) => {
      for (const a of agents) {
        editor.update(a.id, (agent) => {
          agent.name = a.name ?? a.id
          agent.description = a.description
          agent.mode = a.mode
          agent.hidden = !!a.hidden
          agent.system = a.system
          if (a.color) agent.color = a.color
          if (a.steps) agent.steps = a.steps
          agent.permissions = a.permissions
          if (!inherit && a.model) agent.model = a.model
        })
      }
      const manager = agents.find((a) => a.default)
      if (manager) editor.default(manager.id)
    })

    // Skills, read from the bundled skills/ directory.
    await ctx.skill.transform((editor) => {
      for (const s of skills) {
        if (editor.get(s.id)) continue // a local skill of the same name wins
        editor.add({ id: s.id, name: s.name, description: s.description, path: join(here, s.path),
          content: readFileSync(join(here, s.contentFile), "utf8") })
      }
    })

    // MCP servers. AMP signs in with OAuth unless a token is given.
    await ctx.mcp.transform((editor) => {
      // Direct tool calls (codemode off) and a cap per call: a call that never
      // returns must not stall an unattended agent forever.
      const timeout = { execution: 120000 }
      const amp = { type: "remote", url, codemode: false, timeout }
      if (token) amp.headers = { Authorization: `Bearer ${token}` }
      editor.set("amp", amp)
      if (opts.context7 !== false && !editor.get("context7")) {
        editor.set("context7", { type: "remote", url: "https://mcp.context7.com/mcp", codemode: false, timeout })
      }
      if (opts.chromeDevtools !== false && !editor.get("chrome-devtools")) {
        editor.set("chrome-devtools", { type: "local", command: ["npx", "-y", "chrome-devtools-mcp@latest", "--headless", "--isolated"], codemode: false, timeout })
      }
    })

    // OpenRouter models: what they accept and their thinking variants. Only
    // when you use OpenRouter and the model is in its catalog.
    if (!inherit) {
      await ctx.provider.transform((editor) => {
        if (!editor.get("openrouter")) return
        for (const [id, m] of Object.entries(models)) {
          try {
            editor.models.update("openrouter", id, (model) => {
              if (m.capabilities) model.capabilities = { ...model.capabilities, ...m.capabilities }
              if (m.variants) model.variants = m.variants
            })
          } catch {
            // not in this OpenRouter catalog: the agent falls back to its defaults
          }
        }
      })
    }

    // amp_publish_artifact_file: publish a file without the model re-typing it.
    await ctx.tool.transform((tools) => {
      tools.add({
        name: "amp_publish_artifact_file",
        options: { codemode: false },
        description:
          "Publish a file as an AMP artifact, or a new version of one, by path: the file is read and sent for you, so you " +
          "don't paste its contents. Same rules as amp_publish_artifact (task_id or project_id and title for a new one, " +
          "artifact_id for a new version). Kind comes from the extension. Needs an AMP token in the plugin; without one, " +
          "use amp_publish_artifact.",
        input: z.object({
          path: z.string().describe("REQUIRED. The file to publish (absolute, or relative to the working directory)."),
          task_id: z.number().optional().describe("The ticket it's for (new artifact)."),
          project_id: z.number().optional().describe("The project, for an artifact not tied to a ticket."),
          artifact_id: z.number().optional().describe("Publish a new version of this artifact."),
          title: z.string().optional().describe("Title (required for a new artifact)."),
          kind: z.string().optional().describe("html, svg, image, markdown, pdf or text; default from the extension."),
          note: z.string().optional().describe("What this version is or changed."),
        }),
        output: z.object({ result: z.string() }),
        execute: async (input) => {
          if (!token) return { output: { result: "No AMP token in the plugin's options or AMP_TOKEN: publish with amp_publish_artifact instead." } }
          const file = isAbsolute(input.path) ? input.path : resolve(ctx.location?.directory ?? process.cwd(), input.path)
          const kind = input.kind || KINDS[extname(file).toLowerCase()]
          if (!kind) return { output: { result: `Can't tell the kind of ${file}: pass kind.` } }
          const args = { note: input.note ?? "" }
          if (kind === "image" || kind === "pdf") args.content_base64 = readFileSync(file).toString("base64")
          else args.content = readFileSync(file, "utf8")
          if (input.artifact_id) { args.artifact_id = input.artifact_id; if (input.title) args.title = input.title }
          else Object.assign(args, { title: input.title, kind, task_id: input.task_id, project_id: input.project_id })
          try {
            const out = await callAmp(url, token, "amp_publish_artifact", args)
            const a = out.artifact
            return { output: { result: `Published artifact #${a.id} v${a.current_version} (${a.status}) ${a.url ?? ""}. ${out.next ?? ""}` } }
          } catch (e) {
            return { output: { result: `AMP refused: ${e.message}` } }
          }
        },
      })
    })

    // amp_session_usage: lets an agent report its own model and token usage
    // on its AMP ticket, so AMP can report spend per ticket.
    await ctx.tool.transform((tools) => {
      tools.add({
        name: "amp_session_usage",
        options: { codemode: false },
        description:
          "Your own model and token usage in this session so far: model, input/output/cache tokens, and cost when the provider " +
          "reports it. Call it right before amp_complete_task or amp_block_task and pass the numbers along, so AMP can report " +
          "token spend per ticket.",
        input: z.object({ ticket: z.number().optional().describe("Optional: the AMP ticket you're about to report on.") }),
        output: z.object({ usage: z.string() }),
        execute: async (_input, call) => {
          const res = await ctx.session.get({ sessionID: call.sessionID })
          const s = res?.session ?? res
          const t = s?.tokens
          const usage = {
            session_id: call.sessionID,
            model: s?.model ? `${s.model.providerID}/${s.model.id}` : "",
            input_tokens: t?.input ?? null,
            output_tokens: t ? (t.output ?? 0) + (t.reasoning ?? 0) : null,
            cache_read_tokens: t?.cache?.read ?? null,
            cache_write_tokens: t?.cache?.write ?? null,
            cost_usd: s?.cost != null ? Math.round(s.cost * 10000) / 10000 : null,
          }
          return { output: { usage: JSON.stringify(usage) } }
        },
      })
    })
  },
}
