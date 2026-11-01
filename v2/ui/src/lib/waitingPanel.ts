/**
 * Mission's "Waiting on people" panel (KB decisions/human-work-ux.md §3):
 * how each person's waiting items read and where their row leads.
 *
 * The items come from the server (GET /projects/:id/waiting-on-people,
 * Service.WaitingOn run per person), so there are no rules here about what
 * waits on whom: only wording, order and links.
 */
import type { PersonWaiting, WaitCounts, WaitKind } from '../types'

export const WAIT_KINDS: WaitKind[] = ['approve', 'answer', 'merge', 'start', 'do']
export const WAIT_KIND_LABEL: Record<WaitKind, string> = { approve: 'Approve', answer: 'Answer', merge: 'Merge', start: 'Start', do: 'Do' }

/** The ticket ids that wait on one person (every ticket of their items). */
export function ticketsWaitingOn(p: PersonWaiting | undefined): Set<number> {
  return new Set(p?.items.flatMap(it => it.task_ids) ?? [])
}

/** People in panel order: the viewer first, then most waiting, then most coming up, then by id. */
export function panelOrder(people: PersonWaiting[], meId?: number | null): PersonWaiting[] {
  return [...people].sort((a, b) =>
    Number(b.user_id === meId) - Number(a.user_id === meId)
    || b.counts.waiting - a.counts.waiting || b.counts.coming_up - a.counts.coming_up || a.user_id - b.user_id)
}

/** "#2471", "#2471, #2462", "#2471, #2462 +3": at most two ids, then how many more. */
export function idList(ids: number[]): string {
  const shown = ids.slice(0, 2).map(id => `#${id}`).join(', ')
  return ids.length > 2 ? `${shown} +${ids.length - 2}` : shown
}

/**
 * One person's counts in words: "Answer 1 (#2477) · Merge 1 (#2462)". An
 * approval counts epics and names its ticket count ("Approve 1 (9 tickets)").
 * With nothing waiting: "3 coming up", or "" when there's nothing at all.
 */
export function kindSummary(p: PersonWaiting): string {
  const parts = WAIT_KINDS.filter(k => p.counts[k] > 0).map(k => {
    const items = p.items.filter(it => it.kind === k)
    if (k === 'approve') {
      const n = items.reduce((s, it) => s + it.task_ids.length, 0)
      return `Approve ${items.length} (${n} ticket${n === 1 ? '' : 's'})`
    }
    return `${WAIT_KIND_LABEL[k]} ${items.length} (${idList(items.map(it => it.task_ids[0]))})`
  })
  if (p.counts.coming_up > 0) parts.push(`${p.counts.coming_up} coming up`)
  return parts.join(' · ')
}

/** The viewer's own counts in words for the strip: "Approve 1 · Answer 2", zeros left out. */
export function countsSummary(c: WaitCounts): string {
  return WAIT_KINDS.filter(k => c[k] > 0).map(k => `${WAIT_KIND_LABEL[k]} ${c[k]}`).join(' · ')
}

/**
 * Where a person's row leads: their My work in this project, with their first
 * waiting ticket focused. Someone else's list (?user=) is for leads and above
 * (read-only); for anyone else the row opens that ticket on the Board instead,
 * since the server refuses a member reading another person's My work.
 */
export function personLink(projectId: number, p: PersonWaiting, opts: { meId?: number | null; canReadOthers: boolean }): string {
  const focus = p.items[0]?.task_ids[0] ?? p.coming_up[0]
  const mine = p.user_id === opts.meId
  if (!mine && !opts.canReadOthers) {
    return focus != null ? `/project/${projectId}/board?focus=${focus}` : `/project/${projectId}/board`
  }
  const q = new URLSearchParams()
  if (!mine) q.set('user', String(p.user_id))
  if (focus != null) q.set('focus', String(focus))
  const s = q.toString()
  return `/project/${projectId}/my-work${s ? `?${s}` : ''}`
}
