/**
 * A remark plugin that turns ticket refs like "#34" in prose into links that
 * open the ticket on the project's Board (`/project/:id/board?focus=34`).
 * Markdown's internal-link handling then routes them in-app.
 *
 * Only `text` nodes change. Text inside links, inline code, fenced code and
 * raw HTML is left alone, and a ref must not follow a word character, '&' or
 * '/', so "C#", "a&#34;" and "https://x/#34" never match. Cross-project refs
 * ("BA-34") are not handled yet: they need a project code -> id lookup.
 */
import { projectLink } from '../../lib/projectLink'

interface MdNode {
  type: string
  value?: string
  url?: string
  children?: MdNode[]
  data?: { hProperties?: Record<string, string> }
}

const REF = /(?<![\w&/])#(\d{1,6})\b/g
const SKIP = new Set(['link', 'linkReference', 'definition', 'inlineCode', 'code', 'html'])

function splitText(value: string, projectId: number | string): MdNode[] | null {
  const out: MdNode[] = []
  let last = 0
  for (const m of value.matchAll(REF)) {
    const id = Number(m[1])
    if (!(id > 0)) continue
    const at = m.index
    if (at > last) out.push({ type: 'text', value: value.slice(last, at) })
    out.push({
      type: 'link',
      url: projectLink(projectId, 'board', id),
      children: [{ type: 'text', value: m[0] }],
      data: { hProperties: { 'data-ticket-ref': String(id) } },
    })
    last = at + m[0].length
  }
  if (!out.length) return null
  if (last < value.length) out.push({ type: 'text', value: value.slice(last) })
  return out
}

function walk(node: MdNode, projectId: number | string) {
  if (!node.children || SKIP.has(node.type)) return
  const next: MdNode[] = []
  for (const child of node.children) {
    const parts = child.type === 'text' && child.value?.includes('#') ? splitText(child.value, projectId) : null
    if (parts) next.push(...parts)
    else { walk(child, projectId); next.push(child) }
  }
  node.children = next
}

export default function remarkTicketRefs(options: { projectId: number | string }) {
  return (tree: MdNode) => { walk(tree, options.projectId) }
}
