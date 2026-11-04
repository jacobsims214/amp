/**
 * Reading a ticket's own text for the person who has to act on it: the
 * acceptance criteria as a checklist, and comments with their line breaks.
 */

export interface ChecklistItem { text: string; done: boolean }

const ITEM = /^\s*(?:[-*+]|\d+[.)])\s+(?:\[([ xX])\]\s+)?(.*\S)\s*$/

/**
 * Acceptance criteria written as a markdown list ("- a", "1. b", "- [ ] c"),
 * one item per line, ticked where the planner wrote "[x]". Anything else
 * (prose, headings, code, a wrapped item) returns null and is shown as
 * markdown instead, so nothing the planner wrote is dropped.
 */
export function checklistItems(md?: string | null): ChecklistItem[] | null {
  const lines = (md ?? '').split(/\r?\n/).filter(l => l.trim() !== '')
  if (lines.length === 0) return null
  const items: ChecklistItem[] = []
  for (const line of lines) {
    const m = line.match(ITEM)
    if (!m) return null
    items.push({ text: m[2], done: m[1] === 'x' || m[1] === 'X' })
  }
  return items
}

const FENCE = /^\s{0,3}(```|~~~)/

/**
 * Keeps single newlines as line breaks when text is shown as markdown, the
 * way a comment read in a pre-wrapped box did: each line followed by another
 * line gets markdown's trailing-two-spaces hard break. Fenced code is left
 * as it is, and a break at the end of a block (before a blank line or the
 * end) is never added.
 */
export function softBreaks(text: string): string {
  const lines = text.split(/\r?\n/)
  let fenced = false
  return lines.map((line, i) => {
    if (FENCE.test(line)) { fenced = !fenced; return line }
    const next = lines[i + 1]
    if (fenced || next == null || line.trim() === '' || next.trim() === '' || FENCE.test(next)) return line
    return line.endsWith('  ') ? line : `${line.replace(/\s+$/, '')}  `
  }).join('\n')
}
