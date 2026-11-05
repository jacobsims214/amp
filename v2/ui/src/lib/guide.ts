/**
 * Links into the AMP guide (the global KB at /guide). A doc is named by its
 * path under v2/kb, e.g. setup/connect-your-agent.md, and opened with
 * ?doc=<path>.
 */
export function guideDoc(path: string): string {
  return `/guide?doc=${path}`
}

/** How to connect an agent harness, and what the manager agent is. */
export const CONNECT_AGENT_DOC = guideDoc('setup/connect-your-agent.md')

/** AMP's MCP endpoint, the address an agent harness connects to. */
export const mcpURL = () => `${window.location.origin}/mcp`
