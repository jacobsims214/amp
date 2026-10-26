/**
 * A remark plugin that keeps single newlines in prose as line breaks, the way
 * the terminal shows the same text (what remark-breaks does, without another
 * dependency). Markdown would otherwise join "a\nb\nc" into one line.
 *
 * Only `text` nodes change, so fenced and indented code (`code`), inline code
 * (`inlineCode`) and raw HTML (`html`, shown as text by SafeMarkdown) keep
 * their newlines as they were, and table cells never hold a newline.
 */
interface MdNode { type: string; value?: string; children?: MdNode[] }

function splitText(node: MdNode): MdNode[] {
  const parts = (node.value ?? '').split(/[ \t]*\r?\n[ \t]*/)
  const out: MdNode[] = []
  parts.forEach((value, i) => {
    if (i > 0) out.push({ type: 'break' })
    if (value) out.push({ type: 'text', value })
  })
  return out
}

function walk(node: MdNode) {
  if (!node.children) return
  const next: MdNode[] = []
  for (const child of node.children) {
    if (child.type === 'text' && child.value?.includes('\n')) next.push(...splitText(child))
    else { walk(child); next.push(child) }
  }
  node.children = next
}

export default function remarkSoftBreaks() {
  return (tree: MdNode) => { walk(tree) }
}
