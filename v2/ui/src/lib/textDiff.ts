/**
 * A line diff for "What changed since you approved it" (Approve view): the
 * ticket's text as approved (the snapshot) against its text now. Plain LCS
 * over lines; ticket text is short, so O(n·m) is fine. Long inputs fall back
 * to "all removed, all added" rather than a slow table.
 */
export type DiffLine = { kind: 'same' | 'add' | 'del'; text: string }

const MAX_CELLS = 250_000

export function lineDiff(before: string, after: string): DiffLine[] {
  const a = before.replace(/\r\n/g, '\n').split('\n')
  const b = after.replace(/\r\n/g, '\n').split('\n')
  if (before === after) return a.map(text => ({ kind: 'same', text }))
  if (a.length * b.length > MAX_CELLS) {
    return [...a.map(text => ({ kind: 'del' as const, text })), ...b.map(text => ({ kind: 'add' as const, text }))]
  }
  const n = a.length, m = b.length
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0))
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1])
    }
  }
  const out: DiffLine[] = []
  let i = 0, j = 0
  while (i < n && j < m) {
    if (a[i] === b[j]) { out.push({ kind: 'same', text: a[i] }); i++; j++ }
    else if (lcs[i + 1][j] >= lcs[i][j + 1]) out.push({ kind: 'del', text: a[i++] })
    else out.push({ kind: 'add', text: b[j++] })
  }
  while (i < n) out.push({ kind: 'del', text: a[i++] })
  while (j < m) out.push({ kind: 'add', text: b[j++] })
  return out
}

/** Only the changed lines, with blank lines dropped (what the reader shows). */
export function changedLines(before: string, after: string): DiffLine[] {
  return lineDiff(before, after).filter(l => l.kind !== 'same' && l.text.trim() !== '')
}
