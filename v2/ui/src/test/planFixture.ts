/** Small ApprovalPlan fixtures for the Approve view's tests. */
import type { ApprovalPlan, PlanTicket } from '../types'

export const AS_OF = '2026-10-27T13:00:27.767763Z'

export function ticket(id: number, over: Partial<PlanTicket> = {}): PlanTicket {
  return {
    id, epic_id: 172, story_id: 413, name: `Ticket ${id}`, description: `Do ${id}.`, acceptance_criteria: `${id} works.`,
    priority: '2', state: 'backlog', performer: 'agent', assigned_to: 'amp:amp-worker-backend', owner_user_id: 9, owner_name: 'Jacob',
    planned_at: '2026-10-27T12:53:35.243576Z', merge_state: 'none', awaiting: true, yours: true, change: 'new',
    dependency_ids: [], dependent_ids: [], waits_on: [], starts_on_approval: false, ...over,
  }
}

/**
 * Epic 172 in miniature:
 *   story 413: 1 (starts), 2 (after 1), 3 (starts)
 *   story 414: 4 (after 2), 5 human, yours (after 4)
 */
export function busyPlan(over: Partial<ApprovalPlan> = {}): ApprovalPlan {
  const tickets = [
    ticket(1, { starts_on_approval: true }),
    ticket(2, { dependency_ids: [1], waits_on: [1] }),
    ticket(3, { starts_on_approval: true }),
    ticket(4, { story_id: 414, dependency_ids: [2], waits_on: [2], assigned_to: 'amp:amp-worker-frontend' }),
    ticket(5, { story_id: 414, dependency_ids: [4], waits_on: [4], performer: 'human', assigned_to: '' }),
  ]
  return {
    epic: { id: 172, name: 'Human work UX', description: '' },
    epics: [{ id: 172, name: 'Human work UX', description: '', ticket_count: 5, awaiting_ids: [1, 2, 3, 4, 5] }],
    stories: [
      { id: 413, epic_id: 172, name: 'Build: backend', description: '', acceptance_criteria: '', task_ids: [1, 2, 3], awaiting_ids: [1, 2, 3] },
      { id: 414, epic_id: 172, name: 'Build: frontend', description: '', acceptance_criteria: '', task_ids: [4, 5], awaiting_ids: [4, 5] },
    ],
    done_stories: [],
    as_of: AS_OF, require_plan_approval: true, can_approve_on_behalf: false,
    tickets, tickets_total: tickets.length, offset: 0, limit: 200, more: false,
    awaiting_ids: [1, 2, 3, 4, 5], yours_ids: [1, 2, 3, 4, 5], for_user_ids: [1, 2, 3, 4, 5],
    new_ids: [1, 2, 3, 4, 5], changed_ids: [], starts_on_approval: [1, 3], human_gates: [], issues: [], outside: [],
    ...over,
  }
}
