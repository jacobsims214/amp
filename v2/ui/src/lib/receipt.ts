import type { Comment, Run } from '../types'

/** One criterion from a worker's "Work complete." comment. */
export interface SelfCheckItem { criterion: string; ok: boolean; why: string }

export interface SelfCheck {
  items: SelfCheckItem[]
  verified: number
  /** criteria the worker could not verify */
  unverified: SelfCheckItem[]
  total: number
}

const LINE = /^\s*[-*]\s+(.+?):\s*(VERIFIED|COULD NOT VERIFY)\b[\s:—–-]*(.*)$/

/**
 * The self-check in the latest comment that starts "Work complete." (the
 * worker's closing comment): `- <criterion>: VERIFIED` and
 * `- <criterion>: COULD NOT VERIFY <why>` lines, where <why> may follow a
 * dash, an em dash or a colon. Null when no such comment has any such line.
 */
export function parseSelfCheck(comments: Comment[]): SelfCheck | null {
  let latest: Comment | null = null
  let latestAt = 0
  for (const c of comments) {
    if (!c.body.trimStart().startsWith('Work complete.')) continue
    // Parsed, not compared as strings: fractional-second precision can differ.
    const at = Date.parse(c.created_at) || 0
    if (!latest || at > latestAt || (at === latestAt && c.id > latest.id)) { latest = c; latestAt = at }
  }
  if (!latest) return null
  const items: SelfCheckItem[] = []
  for (const line of latest.body.split(/\r?\n/)) {
    const m = LINE.exec(line)
    if (!m) continue
    items.push({ criterion: m[1].trim(), ok: m[2] === 'VERIFIED', why: m[3].trim() })
  }
  if (items.length === 0) return null
  const verified = items.filter(i => i.ok).length
  return { items, verified, unverified: items.filter(i => !i.ok), total: items.length }
}

/** The newest run (runs are oldest first) that satisfies `has`; by default one that names a PR or a branch. */
export function latestRunWith(runs: Run[], has: (r: Run) => boolean = r => !!(r.pr_url || r.branch)): Run | undefined {
  for (let i = runs.length - 1; i >= 0; i--) if (has(runs[i])) return runs[i]
  return undefined
}

/** A run note that starts "closeout:" marks a completion that wasn't the dispatched worker's. */
export const isCloseout = (r?: Run) => !!r?.note?.trimStart().startsWith('closeout:')

/** "2h ago" style age. */
export function agoText(iso: string, now = Date.now()): string {
  const mins = Math.floor(Math.max(0, now - new Date(iso).getTime()) / 60000)
  const hrs = Math.floor(mins / 60)
  const days = Math.floor(hrs / 24)
  if (days > 0) return `${days}d ago`
  if (hrs > 0) return `${hrs}h ago`
  if (mins > 0) return `${mins}m ago`
  return 'just now'
}
