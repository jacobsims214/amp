import type { Task } from '../types'

// AMP stores four ticket states, but "blocked" means three different things
// a person reads differently. The UI shows six:
//   ready      backlog with nothing left to wait on
//   prog       in progress
//   wait       blocked by unfinished dependencies (block_reason empty) — no one needs to act
//   flag       a question: blocked with a reason (an agent called amp_block_task) — needs a person
//   sched      blocked until start_at (block_reason "scheduled:…")
//   done       completed
export type DisplayState = 'ready' | 'prog' | 'wait' | 'flag' | 'sched' | 'done'

export const DISPLAY_STATES: DisplayState[] = ['done', 'prog', 'flag', 'sched', 'ready', 'wait']

export const STATE_LABEL: Record<DisplayState, string> = {
  ready: 'Ready', prog: 'In progress', wait: 'Waiting', flag: 'Question', sched: 'Scheduled', done: 'Done',
}

export const STATE_HINT: Record<DisplayState, string> = {
  ready: 'Nothing to wait on; can be dispatched now',
  prog: 'An agent or person is working it',
  wait: 'Waiting on unfinished dependencies; unblocks by itself',
  flag: 'An agent asked a question and stopped; answer it to put the ticket back to ready',
  sched: 'Waiting for its start time',
  done: 'Completed',
}

/** The color token for a display state. */
export const STATE_VAR: Record<DisplayState, string> = {
  ready: 'var(--st-ready)', prog: 'var(--st-prog)', wait: 'var(--st-wait)',
  flag: 'var(--st-flag)', sched: 'var(--st-sched)', done: 'var(--st-done)',
}

type TaskLike = Pick<Task, 'state'> & Partial<Pick<Task, 'block_reason' | 'blocked_by_ids'>>

export function displayState(t: TaskLike): DisplayState {
  switch (t.state) {
    case 'completed': return 'done'
    case 'in_progress': return 'prog'
    case 'scheduled': return 'sched'
    case 'blocked': {
      const reason = t.block_reason ?? ''
      if (reason.startsWith('scheduled:')) return 'sched'
      if (reason !== '') return 'flag'
      return 'wait'
    }
    default: return 'ready'
  }
}

/** Counts per display state. */
export function countStates(tasks: TaskLike[]): Record<DisplayState, number> {
  const c: Record<DisplayState, number> = { ready: 0, prog: 0, wait: 0, flag: 0, sched: 0, done: 0 }
  for (const t of tasks) c[displayState(t)]++
  return c
}
