import { displayState } from './ticketState'
import { lookupPerson, type Person } from './people'
import type { ActivityLog, Task } from '../types'

export interface PersonRow {
  id: number | null
  name: string
  done: Task[]
  flying: Task[]
  questions: Task[]
  ready: number
  /** open tickets waiting on dependencies or a scheduled start */
  waiting: number
  lastAt: number
}

/**
 * Each person's work on a project: what they (and their agents) finished
 * since `since`, what's running, what's waiting on them, and what's ready
 * next. Agents run under their person's identity, so a person's row is
 * the work their agents did too. This is the standup.
 */
export function peopleRows(tasks: Task[], activity: ActivityLog[], people: Map<number, Person>, since: string): PersonRow[] {
  const rows = new Map<number | null, PersonRow>()
  const row = (id: number | null) => {
    let r = rows.get(id)
    if (!r) {
      r = { id, name: id == null ? 'Unowned' : lookupPerson(people, id)!.name, done: [], flying: [], questions: [], ready: 0, waiting: 0, lastAt: 0 }
      rows.set(id, r)
    }
    return r
  }
  for (const t of tasks) {
    const st = displayState(t)
    const owner = t.owner_user_id ?? null
    if (t.state === 'completed' && t.completed_at && t.completed_at >= since) row(owner).done.push(t)
    else if (st === 'prog') row(owner).flying.push(t)
    else if (st === 'flag') row(owner).questions.push(t)
    else if (st === 'ready') row(owner).ready++
    else if (st === 'wait' || st === 'sched') row(owner).waiting++
  }
  const ownerOf = new Map(tasks.map(t => [t.id, t.owner_user_id ?? null]))
  for (const a of activity) {
    const id = a.actor_user_id ?? ownerOf.get(a.task_id) ?? null
    const r = rows.get(id)
    if (r) r.lastAt = Math.max(r.lastAt, Date.parse(a.created_at))
  }
  return [...rows.values()]
    .filter(r => r.done.length || r.flying.length || r.questions.length || r.ready || r.waiting)
    .sort((a, b) => b.questions.length - a.questions.length || b.flying.length - a.flying.length || b.done.length - a.done.length)
}
