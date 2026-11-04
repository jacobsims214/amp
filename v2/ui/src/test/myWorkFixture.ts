/** ProjectMyWork fixtures (GET /api/projects/2/my-work) for the My work tab's tests. */
import type { DoneItem, ProjectMyWork, ProjectWaitItem, Task, WaitCounts } from '../types'
import { busyPlan } from './planFixture'

const H = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString()

export function task(id: number, over: Partial<Task> = {}): Task {
  return {
    id, project_id: 2, epic_id: 172, story_id: 414, name: `Ticket ${id}`, description: 'd', acceptance_criteria: 'a', state: 'backlog',
    priority: '2', dependency_ids: [], created_at: H(5), updated_at: H(1), planned_at: H(5), approved_at: H(4), approved_by: 9,
    owner_user_id: 9, created_by: 9, ...over,
  }
}

const base = { project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2' }

export const approveItem = (): ProjectWaitItem => ({
  ...base, kind: 'approve', key: 'approve:172', task_ids: [1, 2, 3, 4, 5], name: 'Human work UX', epic_id: 172,
  epic_name: 'Human work UX', count: 5, since: H(3), plan: busyPlan(),
})
export const answerItem = (): ProjectWaitItem => ({
  ...base, kind: 'answer', key: 'task:2473', task_id: 2473, task_ids: [2473], name: 'Design: one per-project My work view', since: H(0.2),
  state: 'blocked', question: 'Approved as is, or request changes?',
  task: task(2473, { state: 'blocked', block_reason: 'Fall back to a 0600 file, or fail with a clear error?', agent_id: 'amp-designer' }),
})
export const mergeItem = (over: Partial<ProjectWaitItem> = {}): ProjectWaitItem => ({
  ...base, kind: 'merge', key: 'task:2471', task_id: 2471, task_ids: [2471], name: 'Board: the Mine filter uses the owner only', since: H(2),
  completed_at: H(2), merge_state: 'needed', pr_url: 'https://github.com/o/r/pull/88', branch: 'fix/board-mine',
  // The #3011 contract: a recorded target, real waiters only, no folded step.
  base_branch: 'main', target_source: 'run', holding_up: [2490, 2491],
  task: task(2471, { state: 'completed', completed_at: H(2), merge_state: 'needed', agent_id: 'amp-worker-frontend' }),
  ...over,
})
/**
 * A Merge card carrying the merge-alert classifier's verdict (AMP #3158): the
 * arm decided server-side, plus the facts behind it. `mergeItem` without these
 * fields is a pre-#3158 server, which the card must keep rendering the old way.
 */
export const alertMergeItem = (over: Partial<ProjectWaitItem> = {}): ProjectWaitItem => mergeItem({
  alert: 'default_branch',
  alert_reason: "the PR targets main, the repository's default branch",
  default_branch: 'main',
  target_source: 'detected',
  base_branch: 'main',
  ...over,
})
/**
 * A linked Merge card: the plan's human merge step folds into it (#3009/#3011,
 * artifact #27 states 4–5). Not ready by default, so a test that wants the
 * ready state passes `tracked_by: {... step, ready: true}`.
 */
export const linkedMergeItem = (over: Partial<ProjectWaitItem> = {}): ProjectWaitItem => mergeItem({
  key: 'task:2302', task_id: 2302, task_ids: [2302], name: 'Open PR: harness on the public home page',
  pr_url: 'https://github.com/o/r/pull/88', branch: 'feat/harness-home-page', base_branch: 'main', target_source: 'run',
  holding_up: [2901],
  tracked_by: { task_id: 2304, name: 'Merge PR: harness public page', state: 'backlog', ready: false, waiting_on: [2303, 2131],
    reason: 'its merge step still waits on #2303, #2131' },
  task: task(2302, { state: 'completed', completed_at: H(2), merge_state: 'needed', agent_id: 'amp-worker-docs' }),
  ...over,
})

/**
 * A legacy Merge card from a server without the #3011 contract: a PR and a head,
 * no target provenance and no waiter set. It must keep the ordinary action — an
 * unreported waiter set is not evidence that nothing waits.
 */
export const legacyMergeItem = (over: Partial<ProjectWaitItem> = {}): ProjectWaitItem => ({
  ...base, kind: 'merge', key: 'task:2600', task_id: 2600, task_ids: [2600], name: 'Older PR with no contract', since: H(2),
  completed_at: H(2), merge_state: 'needed', pr_url: 'https://github.com/o/r/pull/77', branch: 'fix/older',
  task: task(2600, { state: 'completed', completed_at: H(2), merge_state: 'needed', agent_id: 'amp-worker-backend' }),
  ...over,
})

export const startItem = (): ProjectWaitItem => ({
  ...base, kind: 'start', key: 'task:2803', task_id: 2803, task_ids: [2803], name: 'UI: not started states', since: H(0.5),
  state: 'not_started', agent_id: 'amp:amp-worker-frontend', not_started_minutes: 32,
  task: task(2803, { state: 'in_progress', phase: 'not_started', started_at: null, agent_id: 'amp:amp-worker-frontend', dispatched_at: H(0.5) }),
})
export const doItem = (): ProjectWaitItem => ({
  ...base, kind: 'do', key: 'task:2481', task_id: 2481, task_ids: [2481], name: 'Rotate the Dex client secret in prod', since: H(1),
  state: 'backlog', performer: 'human', human_reason: 'access',
  task: task(2481, { performer: 'human', human_reason: 'access' }),
})
export const comingItem = (): ProjectWaitItem => ({
  ...base, kind: 'coming_up', key: 'task:2131', task_id: 2131, task_ids: [2131], name: 'M5-TAG: tag harness v0.5.0', since: H(9),
  state: 'blocked', performer: 'human',
  waits_on: [{ id: 2130, project_id: 2, epic_id: 1, story_id: 1, name: 'M5-MERGE', state: 'in_progress', performer: 'human', merge_state: 'none', satisfied: false }],
})

export const doneItems = (): DoneItem[] => [
  { kind: 'approve', task_ids: [1, 2, 3], epic_id: 172, name: 'Human work UX', at: H(1), count: 9 },
  { kind: 'merge', task_id: 2470, task_ids: [2470], name: 'My Work: questions counted once', at: H(0.5), count: 1, merge_source: 'github', merge_state: 'merged' },
]

function counts(items: ProjectWaitItem[], coming: number): WaitCounts {
  const n = (k: string) => items.filter(i => i.kind === k).length
  return { waiting: items.length, approve: n('approve'), answer: n('answer'), merge: n('merge'), start: n('start'), do: n('do'), coming_up: coming }
}

export function myWork(state: 'busy' | 'empty' | 'done', over: Partial<ProjectMyWork> = {}): ProjectMyWork {
  const items = state === 'busy' ? [approveItem(), answerItem(), mergeItem(), startItem(), doItem()] : []
  const coming = state === 'empty' ? [] : [comingItem()]
  return {
    project_id: 2, user_id: 9, read_only: false, kinds: ['approve', 'answer', 'merge', 'start', 'do'],
    counts: counts(items, coming.length),
    items: {
      approve: items.filter(i => i.kind === 'approve'), answer: items.filter(i => i.kind === 'answer'),
      merge: items.filter(i => i.kind === 'merge'), start: items.filter(i => i.kind === 'start'),
      do: items.filter(i => i.kind === 'do'),
    },
    coming_up: coming,
    done_today: state === 'empty' ? [] : doneItems(),
    agents: { project_id: 2, in_progress: 4, ready: 11, waiting: 19, done_today: 3 },
    day: '2026-10-27',
    ...over,
  }
}
