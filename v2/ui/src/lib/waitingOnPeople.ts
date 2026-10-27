/**
 * "Waiting on people" for Mission (KB decisions/human-work-ux.md §3): what
 * each person in the project must do next, by kind, from the Board's tickets.
 *
 * There's no REST endpoint for every person's waiting items in one project
 * (GET /projects/:id/my-work is one person's), so this mirrors the server's
 * classifier, Service.waitingOn (amp-api internal/service/waiting.go), rule
 * for rule:
 * - approve: not done, not approved as planned now, the project requires plan
 *   approval, and the person is its approval owner (owner, else its creator).
 *   One item per epic.
 * - answer: the person owns it and it's blocked with a question (a reason that
 *   isn't "scheduled:…").
 * - merge: the person owns it and it's done with merge_state needed or abandoned.
 * - do: a human ticket the person owns, in progress, or ready and not awaiting
 *   a required approval.
 * - coming up: the person's other open human tickets. Shown, never counted.
 * The viewer's own badge and strip use the server's counts (lib/waitCounts.ts).
 */
import { approvalOwner, ticketApproved } from './approval'
import { isHuman } from './human'
import type { Task, WaitCounts } from '../types'

export type WaitKind = 'approve' | 'answer' | 'merge' | 'do'
export const WAIT_KINDS: WaitKind[] = ['approve', 'answer', 'merge', 'do']
export const WAIT_KIND_LABEL: Record<WaitKind, string> = { approve: 'Approve', answer: 'Answer', merge: 'Merge', do: 'Do' }

/** One item: a ticket, or for approve, the epic's tickets awaiting the person. */
export interface PersonItem { kind: WaitKind; taskIds: number[]; epicId?: number }

export interface PersonWaiting {
  userId: number
  counts: WaitCounts
  items: PersonItem[]
  /** ticket ids of the person's Coming up */
  comingUp: number[]
}

/** True when the ticket is a question (blocked with a reason that isn't a schedule). */
export function isQuestion(t: Pick<Task, 'state' | 'block_reason'>): boolean {
  const r = t.block_reason ?? ''
  return t.state === 'blocked' && r !== '' && !r.startsWith('scheduled:')
}

/** Done with a PR nobody merged yet (or one closed without merging): it holds its dependents. */
export function needsMerge(t: Pick<Task, 'state' | 'merge_state'>): boolean {
  return t.state === 'completed' && (t.merge_state === 'needed' || t.merge_state === 'abandoned')
}

const time = (iso?: string | null) => (iso ? Date.parse(iso) || 0 : 0)

/** Every person with something waiting on them or coming up, in the server's item order. */
export function waitingOnPeople(tasks: Task[], opts: { requireApproval: boolean }): Map<number, PersonWaiting> {
  const out = new Map<number, PersonWaiting>()
  const person = (id: number) => {
    let p = out.get(id)
    if (!p) {
      p = { userId: id, counts: { waiting: 0, approve: 0, answer: 0, merge: 0, do: 0, coming_up: 0 }, items: [], comingUp: [] }
      out.set(id, p)
    }
    return p
  }
  const since = new Map<PersonItem, number>()
  const add = (uid: number, it: PersonItem, at: number) => { person(uid).items.push(it); since.set(it, at) }
  const approve = new Map<string, PersonItem>()

  for (const t of tasks) {
    const awaits = t.state !== 'completed' && !ticketApproved(t)
    if (opts.requireApproval && awaits) {
      const who = approvalOwner(t)
      if (who != null) {
        const key = `${who}:${t.epic_id}`
        let it = approve.get(key)
        const at = time(t.planned_at || t.created_at)
        if (!it) {
          it = { kind: 'approve', taskIds: [], epicId: t.epic_id }
          approve.set(key, it)
          add(who, it, at)
        }
        it.taskIds.push(t.id)
        if (at < (since.get(it) ?? at)) since.set(it, at)
      }
    }
    const owner = t.owner_user_id
    if (owner == null) continue
    if (isQuestion(t)) { add(owner, { kind: 'answer', taskIds: [t.id] }, time(t.updated_at)); continue }
    if (needsMerge(t)) { add(owner, { kind: 'merge', taskIds: [t.id] }, time(t.completed_at || t.updated_at)); continue }
    if (!isHuman(t) || t.state === 'completed') continue
    if (t.state === 'in_progress') add(owner, { kind: 'do', taskIds: [t.id] }, time(t.dispatched_at || t.ready_at || t.created_at))
    else if (t.state === 'backlog' && !(opts.requireApproval && awaits)) add(owner, { kind: 'do', taskIds: [t.id] }, time(t.ready_at || t.created_at))
    else person(owner).comingUp.push(t.id)
  }

  for (const p of out.values()) {
    p.items.sort((a, b) => WAIT_KINDS.indexOf(a.kind) - WAIT_KINDS.indexOf(b.kind)
      || (since.get(a) ?? 0) - (since.get(b) ?? 0) || a.taskIds[0] - b.taskIds[0])
    for (const it of p.items) p.counts[it.kind]++
    p.counts.waiting = p.items.length
    p.counts.coming_up = p.comingUp.length
  }
  return out
}

/** The ticket ids that wait on one person (every ticket of their items). */
export function ticketsWaitingOn(p: PersonWaiting | undefined): Set<number> {
  return new Set(p?.items.flatMap(it => it.taskIds) ?? [])
}

/** People in panel order: the viewer first, then most waiting, then most coming up, then by id. */
export function panelOrder(all: Map<number, PersonWaiting>, meId?: number | null): PersonWaiting[] {
  return [...all.values()].sort((a, b) =>
    Number(b.userId === meId) - Number(a.userId === meId)
    || b.counts.waiting - a.counts.waiting || b.counts.coming_up - a.counts.coming_up || a.userId - b.userId)
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
      const n = items.reduce((s, it) => s + it.taskIds.length, 0)
      return `Approve ${items.length} (${n} ticket${n === 1 ? '' : 's'})`
    }
    return `${WAIT_KIND_LABEL[k]} ${items.length} (${idList(items.map(it => it.taskIds[0]))})`
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
  const focus = p.items[0]?.taskIds[0] ?? p.comingUp[0]
  const mine = p.userId === opts.meId
  if (!mine && !opts.canReadOthers) {
    return focus != null ? `/project/${projectId}/board?focus=${focus}` : `/project/${projectId}/board`
  }
  const q = new URLSearchParams()
  if (!mine) q.set('user', String(p.userId))
  if (focus != null) q.set('focus', String(focus))
  const s = q.toString()
  return `/project/${projectId}/my-work${s ? `?${s}` : ''}`
}
