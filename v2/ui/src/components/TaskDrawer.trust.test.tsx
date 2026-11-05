// The drawer says one thing about a ticket (#129): the header's state and the
// Details tab agree, an unapproved ticket says so where its state is shown,
// and the creator is named (or, in local dev, why nobody is).
import { cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { TaskDrawer } from './TaskDrawer'
import type { ProjectAccess, ProjectMember, Task } from '../types'

export const trustTask = (over: Partial<Task> = {}): Task => ({
  id: 1, project_id: 2, epic_id: 1, story_id: 3, name: 'Import leads', description: 'Write the import.', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_at: '2026-11-05T10:00:00Z', updated_at: '2026-11-05T10:00:00Z',
  planned_at: '2026-11-05T10:00:00Z', owner_user_id: 9, created_by: 9, ...over,
})
const members: ProjectMember[] = [{ user_id: 9, email: 'jo@example.com', display_name: 'Jo Owner', access: 'member' as ProjectAccess }]

let storyDesc = ''
let epicDesc = ''
const server = setupServer(
  http.get('*/api/projects/2/tasks', () => HttpResponse.json({ tasks: [] })),
  http.get('*/api/projects/2/epics', () => HttpResponse.json({ epics: [{ id: 1, project_id: 2, name: 'Leads', description: epicDesc }] })),
  http.get('*/api/epics/1/stories', () => HttpResponse.json({ stories: [{ id: 3, epic_id: 1, name: 'Import', description: storyDesc, acceptance_criteria: '' }] })),
  http.get('*/api/tasks/:id/history', () => HttpResponse.json({ history: [], count: 0 })),
  http.get('*/api/tasks/:id/comments', () => HttpResponse.json({ comments: [] })),
  http.get('*/api/tasks/:id/runs', () => HttpResponse.json({ runs: [], usage: { runs: 0 } })),
  http.get('*/api/*', () => HttpResponse.json({})),
)
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen() })
afterEach(() => { cleanup(); server.resetHandlers(); storyDesc = ''; epicDesc = '' })
afterAll(() => { server.close(); vi.unstubAllGlobals() })

function open(t: Task, meId: number | 'local' = 9, opts: { requireApproval?: boolean; members?: ProjectMember[] } = {}) {
  const router = createMemoryRouter([{ path: '*', element: <TaskDrawer task={t} onClose={() => {}} members={opts.members ?? members} meId={meId === 'local' ? undefined : meId}
    myAccess={'member' as ProjectAccess} onTaskUpdated={() => {}} requireApproval={opts.requireApproval ?? true} /> }],
    { initialEntries: ['/project/2/board?focus=1&tab=details'] })
  render(<RouterProvider router={router} />)
}

describe('TaskDrawer speaks one status vocabulary (#129)', () => {
  it('a ready ticket nobody approved: the header and Details say Ready, and both say it needs approval', async () => {
    open(trustTask())
    expect(await screen.findByTestId('needs-approval')).toHaveTextContent('Needs approval')
    expect(screen.getByText('Plan not approved yet.')).toBeInTheDocument()
    const state = screen.getByText('State').parentElement!
    expect(within(state).getByText('Ready')).toBeInTheDocument()
    expect(screen.queryByText('backlog')).toBeNull()
  })

  it('an approved ticket has no Needs approval tag', async () => {
    open(trustTask({ approved_at: '2026-11-05T10:05:00Z', approved_by: 9 }))
    expect(await screen.findByText(/Approved by/)).toBeInTheDocument()
    expect(screen.queryByTestId('needs-approval')).toBeNull()
  })

  it('names the creator and the chosen owner', async () => {
    open(trustTask())
    expect(await screen.findByText('Jo Owner (you)')).toBeInTheDocument()
    expect(screen.getByText('Jo Owner')).toBeInTheDocument()
  })

  it('with no creator or approver on record, says so, not "unknown", "local dev" or "the AMP upgrade"', async () => {
    open(trustTask({ created_by: null, owner_user_id: null, approved_at: '2026-11-05T10:05:00Z', approved_by: null }), 'local')
    expect(await screen.findByText('not recorded')).toBeInTheDocument()
    expect(screen.getByText('(approver not recorded)')).toBeInTheDocument()
    expect(screen.queryByText('unknown')).toBeNull()
    expect(screen.queryByText(/AMP upgrade|local dev/)).toBeNull()
  })

  it('names a recorded creator and approver even when they are not in members and no viewer is known (the Graph case)', async () => {
    open(trustTask({ created_by: 5, owner_user_id: 5, approved_at: '2026-11-05T10:05:00Z', approved_by: 5 }), 'local', { members: [] })
    const created = await screen.findByText(/^Created by/)
    expect(created).toHaveTextContent('Created by User #5')
    expect(screen.getByText(/^Approved by/).parentElement).toHaveTextContent('Approved by User #5')
    expect(screen.queryByText(/not recorded|local dev/)).toBeNull()
  })

  it('no Needs approval tag when the project does not require approval', async () => {
    open(trustTask(), 9, { requireApproval: false })
    await screen.findByText('Plan not approved yet.')
    expect(screen.queryByTestId('needs-approval')).toBeNull()
  })
})

describe('TaskDrawer shows an agent question\'s options (#130)', () => {
  it('as choices, the recommended one marked', async () => {
    open(trustTask({ state: 'blocked', agent_id: 'amp-worker', block_reason: 'Daily or hourly?', approved_at: '2026-11-05T10:05:00Z', approved_by: 9,
      question: { options: [{ label: 'Daily', consequence: 'Cheaper' }, { label: 'Hourly', consequence: '' }], recommended: 0 } }))
    const group = await screen.findByRole('radiogroup', { name: 'Choose an answer' })
    expect(within(group).getByRole('radio', { name: /Daily/ }).closest('label')).toHaveTextContent('Recommended')
  })
})

describe('TaskDrawer story context (#133)', () => {
  it("shows the story's description as markdown after expanding Story context", async () => {
    storyDesc = '**Goal**: import every lead'
    epicDesc = 'Leads reach sales the same day.'
    open(trustTask())
    const toggle = await screen.findByRole('button', { name: 'Story context' })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    await userEvent.click(toggle)
    const ctx = screen.getByTestId('story-context')
    expect(within(ctx).getByText('Goal', { selector: 'strong' })).toBeInTheDocument()
    expect(within(ctx).getByText('Leads reach sales the same day.')).toBeInTheDocument()
  })

  it('has no Story context toggle when the story and epic have no description', async () => {
    open(trustTask())
    await screen.findByText('Import')
    expect(screen.queryByRole('button', { name: 'Story context' })).toBeNull()
  })
})
