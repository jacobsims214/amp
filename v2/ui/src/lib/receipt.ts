import type { Comment, ReceiptCheck, ReceiptSummaryRow, Run, VerificationStatus } from '../types'

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

/** A verification status in words (#126). */
export const STATUS_LABEL: Record<VerificationStatus, string> = {
  unverified: 'Unverified',
  self_checked: 'Self-checked',
  independently_verified: 'Independently verified',
  human_accepted: 'Accepted',
}

/** A status's color: ink-3 for unverified, in-progress blue for self-checked, teal for verified, success for accepted. */
export const STATUS_TONE: Record<VerificationStatus, string> = {
  unverified: 'var(--ink-3)',
  self_checked: 'var(--st-prog)',
  independently_verified: 'var(--teal)',
  human_accepted: 'var(--success-ink)',
}

export interface CheckSummary {
  /** the worker's checks: passed, failed, couldn't tell, and in all */
  pass: number
  fail: number
  cantTell: number
  total: number
  /** the latest human verdict when it is a Reject */
  rejection?: ReceiptCheck
  /** checks by role, oldest first */
  byRole: { worker: ReceiptCheck[]; verifier: ReceiptCheck[]; human: ReceiptCheck[] }
}

const newer = (a: ReceiptCheck, b: ReceiptCheck) => {
  const ta = Date.parse(a.created_at) || 0
  const tb = Date.parse(b.created_at) || 0
  return ta > tb || (ta === tb && a.id > b.id)
}

/** Counts a receipt's checks for the receipt's "Self-checked 2/2" line. */
export function summarizeChecks(checks: ReceiptCheck[]): CheckSummary {
  const out: CheckSummary = { pass: 0, fail: 0, cantTell: 0, total: 0, byRole: { worker: [], verifier: [], human: [] } }
  let human: ReceiptCheck | undefined
  for (const c of checks) {
    out.byRole[c.role]?.push(c)
    if (c.role === 'human' && c.kind === 'verdict' && (!human || newer(c, human))) human = c
    if (c.role !== 'worker') continue
    out.total++
    if (c.verdict === 'pass') out.pass++
    else if (c.verdict === 'fail') out.fail++
    else out.cantTell++
  }
  if (human?.verdict === 'fail') out.rejection = human
  return out
}

/** The words a verification badge reads as, for a card's aria-label. */
export function verificationText(r: Pick<ReceiptSummaryRow, 'status' | 'held' | 'has_media'>): string {
  if (r.held) return "Couldn't verify, needs you"
  return STATUS_LABEL[r.status] + (r.has_media ? ', with media' : '')
}
