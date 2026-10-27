/**
 * The Approve view's model (design #2494, artifact #24 v2; KB
 * decisions/human-work-ux.md §2 "Approve view"). Pure functions over the
 * ApprovalPlan the server returns, so the outline, the readouts and the
 * approval bar all agree, and so they can be tested without React.
 *
 * - Step: when a ticket can start. Step 1 has no open dependency inside the
 *   set; otherwise 1 + the latest dependency's step. Same step = parallel.
 * - Critical path: the longest chain of open tickets (ties: lowest ids).
 * - Partial approval: an awaiting ticket that isn't ticked is left out (it
 *   stays awaiting). Every ticked ticket downstream of a left-out one is
 *   held: approved, but it can't start until that one is approved.
 */
import { compareInstants } from './approval'
import type { ApprovalPlan, OutsideTicket, PlanEpic, PlanStory, PlanTicket } from '../types'

/** Story colours, in story order (cycled). Tokens from index.css. */
export const STORY_COLORS = ['var(--link)', 'var(--teal)', 'var(--band-2)', 'var(--pink)', 'var(--violet)', 'var(--band-3)']

export interface StorySection {
  story: PlanStory
  /** ticket ids in run order: by step, then id */
  ids: number[]
  firstStep: number
  lastStep: number
  color: string
  /** nothing in it awaits approval */
  allApproved: boolean
}

export interface EpicSection {
  epic: PlanEpic
  stories: StorySection[]
  firstStep: number
  ticketCount: number
}

export interface PlanModel {
  byId: Map<number, PlanTicket>
  /** every ticket in outline order: epic > story > ticket */
  order: number[]
  epics: EpicSection[]
  step: Map<number, number>
  steps: number
  critical: Set<number>
  storyOf: Map<number, StorySection>
  outside: Map<number, OutsideTicket>
  /** open dependents inside the set, for every ticket */
  dependents: Map<number, number[]>
}

const byStepThenId = (step: Map<number, number>) => (a: number, b: number) => (step.get(a)! - step.get(b)!) || a - b

/** Steps for every ticket, cycle-safe (a cycle is cut where it closes). */
export function computeSteps(tickets: PlanTicket[]): Map<number, number> {
  const byId = new Map(tickets.map(t => [t.id, t]))
  const step = new Map<number, number>()
  const visiting = new Set<number>()
  const visit = (id: number): number => {
    const known = step.get(id)
    if (known !== undefined) return known
    if (visiting.has(id)) return 0
    visiting.add(id)
    let s = 1
    for (const d of byId.get(id)?.dependency_ids ?? []) {
      if (byId.has(d)) s = Math.max(s, visit(d) + 1)
    }
    visiting.delete(id)
    step.set(id, s)
    return s
  }
  tickets.forEach(t => visit(t.id))
  return step
}

/** The longest chain: from its last ticket (highest step, lowest id) back through the latest dependency (lowest id). */
export function criticalPath(tickets: PlanTicket[], step: Map<number, number>): Set<number> {
  const byId = new Map(tickets.map(t => [t.id, t]))
  let end: number | null = null
  for (const t of tickets) {
    if (end === null || step.get(t.id)! > step.get(end)! || (step.get(t.id) === step.get(end) && t.id < end)) end = t.id
  }
  const out = new Set<number>()
  let cur = end
  while (cur !== null && !out.has(cur)) {
    out.add(cur)
    const s = step.get(cur)!
    const prev = (byId.get(cur)?.dependency_ids ?? []).filter(d => byId.has(d) && step.get(d) === s - 1).sort((a, b) => a - b)
    cur = prev.length ? prev[0] : null
  }
  return out
}

export function buildModel(plan: ApprovalPlan): PlanModel {
  const tickets = plan.tickets
  const byId = new Map(tickets.map(t => [t.id, t]))
  const step = computeSteps(tickets)
  const steps = Math.max(0, ...step.values())
  const critical = criticalPath(tickets, step)
  const dependents = new Map<number, number[]>()
  for (const t of tickets) {
    for (const d of t.dependency_ids) {
      if (byId.has(d)) dependents.set(d, [...(dependents.get(d) ?? []), t.id])
    }
  }
  const epicList: PlanEpic[] = plan.epics?.length ? plan.epics : plan.epic ? [plan.epic] : []
  const epicOf = (t: PlanTicket) => t.epic_id ?? epicList[0]?.id
  const storyById = new Map(plan.stories.map(s => [s.id, s]))

  // Stories with their tickets; a ticket whose story isn't listed gets a stand-in.
  const storyIds = new Map<number, number[]>()
  for (const t of tickets) storyIds.set(t.story_id, [...(storyIds.get(t.story_id) ?? []), t.id])
  const sections: StorySection[] = []
  for (const [sid, ids] of storyIds) {
    const story = storyById.get(sid) ?? { id: sid, epic_id: epicOf(byId.get(ids[0])!), name: `Story ${sid}`, description: '', acceptance_criteria: '', task_ids: ids, awaiting_ids: [] }
    ids.sort(byStepThenId(step))
    const ss = ids.map(id => step.get(id)!)
    sections.push({ story, ids, firstStep: Math.min(...ss), lastStep: Math.max(...ss), color: '', allApproved: ids.every(id => !byId.get(id)!.awaiting) })
  }
  sections.sort((a, b) => (a.firstStep - b.firstStep) || (a.story.id - b.story.id))

  const epics: EpicSection[] = epicList.map(epic => {
    const stories = sections.filter(s => (s.story.epic_id ?? epicOf(byId.get(s.ids[0])!)) === epic.id)
    return { epic, stories, firstStep: stories.length ? Math.min(...stories.map(s => s.firstStep)) : Infinity, ticketCount: stories.reduce((n, s) => n + s.ids.length, 0) }
  }).filter(e => e.stories.length > 0)
  epics.sort((a, b) => a.firstStep - b.firstStep || epicList.indexOf(a.epic) - epicList.indexOf(b.epic))
  // Colours follow the outline: epic by epic, story by story.
  epics.flatMap(e => e.stories).forEach((s, i) => { s.color = STORY_COLORS[i % STORY_COLORS.length] })

  const storyOf = new Map<number, StorySection>()
  sections.forEach(s => s.ids.forEach(id => storyOf.set(id, s)))
  const order = epics.flatMap(e => e.stories.flatMap(s => s.ids))
  return { byId, order, epics, step, steps, critical, storyOf, outside: new Map(plan.outside.map(o => [o.id, o])), dependents }
}

// ---- Selection ----

/** Tickets the person may tick: awaiting, and theirs (or anyone's with on-behalf rights). */
export function canTick(t: PlanTicket, canOnBehalf: boolean): boolean {
  return t.awaiting && (t.yours || canOnBehalf)
}

/** Default ticks: the person's own awaiting tickets. Others' (leads) start unticked. */
export function defaultTicks(plan: ApprovalPlan): Set<number> {
  return new Set(plan.tickets.filter(t => t.awaiting && t.yours).map(t => t.id))
}

export interface SelectionEffect {
  /** awaiting tickets the person could approve but didn't tick */
  leftOut: Set<number>
  /** ticked tickets downstream of a left-out (or anyone's unticked awaiting) ticket → those tickets */
  held: Map<number, number[]>
  /** ticked, not held, and every dependency satisfied: dispatch picks them up */
  startsNow: number[]
  /** ticked human tickets that are the person's own */
  yourSteps: number[]
  /** ticked, not held, waiting on earlier tickets (your steps included) */
  waitEarlier: number[]
}

export function selectionEffect(model: PlanModel, ticks: Set<number>, canOnBehalf: boolean): SelectionEffect {
  const leftOut = new Set<number>()
  const blocking = new Set<number>() // awaiting and not ticked: anything after it can't start
  for (const t of model.byId.values()) {
    if (t.awaiting && !ticks.has(t.id)) {
      blocking.add(t.id)
      if (canTick(t, canOnBehalf)) leftOut.add(t.id)
    }
  }
  // Every unticked awaiting ancestor of each ticket, through the set.
  const memo = new Map<number, Set<number>>()
  const ancestors = (id: number, seen = new Set<number>()): Set<number> => {
    const m = memo.get(id)
    if (m) return m
    if (seen.has(id)) return new Set()
    seen.add(id)
    const out = new Set<number>()
    for (const d of model.byId.get(id)?.dependency_ids ?? []) {
      if (!model.byId.has(d)) continue
      if (blocking.has(d)) out.add(d)
      ancestors(d, seen).forEach(x => out.add(x))
    }
    memo.set(id, out)
    return out
  }
  const held = new Map<number, number[]>()
  const startsNow: number[] = [], yourSteps: number[] = [], waitEarlier: number[] = []
  for (const id of model.order) {
    if (!ticks.has(id)) continue
    const t = model.byId.get(id)!
    const a = [...ancestors(id)].sort((x, y) => x - y)
    if (a.length) held.set(id, a)
    if (t.performer === 'human' && t.yours) yourSteps.push(id)
    if (a.length) continue
    if (t.starts_on_approval) startsNow.push(id)
    else if (t.waits_on.length > 0) waitEarlier.push(id)
  }
  return { leftOut, held, startsNow, yourSteps, waitEarlier }
}

/** The ticked tickets that are held only because of `leftOut` ids: what "Leave those N out too" unticks. */
export function heldIds(effect: SelectionEffect): number[] {
  return [...effect.held.keys()]
}

// ---- Rows ----

export interface AfterChip {
  /** the other story (null for an outside dependency) */
  story: StorySection | null
  ids: number[]
  outside?: OutsideTicket
  /** it points at a left-out ticket */
  held: boolean
}

/** "after #x" chips: one per other story the ticket depends on, plus unsatisfied outside dependencies. */
export function afterChips(model: PlanModel, id: number, leftOut: Set<number>): AfterChip[] {
  const t = model.byId.get(id)!
  const own = model.storyOf.get(id)
  const groups = new Map<StorySection, number[]>()
  const out: AfterChip[] = []
  for (const d of [...t.dependency_ids].sort((a, b) => a - b)) {
    if (model.byId.has(d)) {
      const s = model.storyOf.get(d)!
      if (s !== own) groups.set(s, [...(groups.get(s) ?? []), d])
    } else {
      const o = model.outside.get(d)
      if (!o || !o.satisfied) out.push({ story: null, ids: [d], outside: o, held: false })
    }
  }
  const chips = [...groups].map(([story, ids]) => ({ story, ids, held: ids.some(x => leftOut.has(x)) }))
  return [...chips, ...out]
}

/** "#a, #b +N": at most two ids, then a count. */
export function chipIds(ids: number[]): string {
  const shown = ids.slice(0, 2).map(i => `#${i}`).join(', ')
  return ids.length > 2 ? `${shown} +${ids.length - 2}` : shown
}

/** Words for an outside ticket's state. */
export function outsideState(o: OutsideTicket | undefined): string {
  if (!o) return 'outside'
  if (o.state === 'completed') return o.merge_state === 'needed' ? 'done, merge needed' : o.merge_state === 'abandoned' ? 'PR closed' : 'done'
  if (o.state === 'in_progress') return 'in progress'
  if (o.state === 'blocked') return o.block_reason && !o.block_reason.startsWith('scheduled:') ? 'question' : 'waiting'
  return 'ready'
}

/** What including this ticket does, in words (the reader's "Include in this approval"). */
export function includeEffect(t: PlanTicket, ticks: Set<number>, effect: SelectionEffect): string {
  if (!t.awaiting) return 'Already approved.'
  if (!ticks.has(t.id)) {
    const after = [...effect.held].filter(([, by]) => by.includes(t.id)).length
    return `Left out: it stays awaiting your approval.${after ? ` ${after} approved ${after === 1 ? 'ticket' : 'tickets'} after it would wait.` : ''}`
  }
  const held = effect.held.get(t.id)
  if (held) return `Approved, but it can't start until you approve ${held.map(i => `#${i}`).join(', ')}.`
  if (t.performer === 'human') return t.waits_on.length ? `Your step, once ${t.waits_on.map(i => `#${i}`).join(', ')} ${t.waits_on.length === 1 ? 'is' : 'are'} done.` : 'Your step: you can start it right away.'
  if (t.starts_on_approval) return 'Starts right away when you approve.'
  if (t.waits_on.length) return `Starts when ${t.waits_on.map(i => `#${i}`).join(', ')} ${t.waits_on.length === 1 ? 'is' : 'are'} done.`
  return t.state === 'in_progress' ? 'In progress.' : 'Starts when dispatched.'
}

/** The newest of two RFC 3339 times, verbatim. */
export function newer(a: string | undefined, b: string | undefined): string | undefined {
  if (!a) return b
  if (!b) return a
  return compareInstants(a, b) >= 0 ? a : b
}

/** Every ticket that awaits the person whose list this is (their Approve item). */
export function awaitingForUser(plan: ApprovalPlan): number[] {
  return plan.for_user_ids?.length ? plan.for_user_ids : plan.yours_ids
}
