// The Approve view (artifact #24 v2, KB decisions/human-work-ux.md §2).
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { ApproveView } from './ApproveView'
import { ApproveCard } from './ApproveCard'
import { AS_OF, busyPlan } from '../../test/planFixture'
import type { ApprovalPlan, ProjectWaitItem } from '../../types'
import type { ApproveSet } from '../../hooks/useApproveParam'

let plan: ApprovalPlan = busyPlan()
let approveReply: Record<string, unknown> = {}
const approveBodies: unknown[] = []
const planQueries: string[] = []
const server = setupServer(
  http.get('*/api/projects/2/my-work', ({ request }) => {
    planQueries.push(new URL(request.url).searchParams.get('approve') ?? '')
    return HttpResponse.json({ plan })
  }),
  http.post('*/api/projects/2/approve-plan', async ({ request }) => {
    approveBodies.push(await request.json())
    return HttpResponse.json(approveReply)
  }),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => { server.resetHandlers(); approveBodies.length = 0; planQueries.length = 0; plan = busyPlan() })
afterAll(() => server.close())

function View({ set = [172] as ApproveSet, onApproved = vi.fn(), focus = null as number | null }) {
  const [f, setF] = useState<number | null>(focus)
  return (
    <MemoryRouter>
      <ApproveView projectId={2} projectName="AMP v2" set={set} focusId={f} onFocus={id => setF(id)} onBack={() => {}} onApproved={onApproved} />
    </MemoryRouter>
  )
}

const bar = () => screen.getByRole('region', { name: 'Approval' })

describe('ApproveView', () => {
  it('loads the epics asked for and ticks your own awaiting tickets', async () => {
    render(<View set={[172, 2]} />)
    expect(await screen.findByRole('heading', { level: 1 })).toHaveTextContent('Human work UX')
    expect(planQueries).toEqual(['172,2'])
    expect(within(bar()).getByText('Approve 5 of 5 tickets')).toBeInTheDocument()
    expect(within(bar()).getByText('2 start right away · 1 step for you (#5) · 3 wait on earlier tickets')).toBeInTheDocument()
    expect(within(bar()).getByText(/planned up to/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Approve 5 tickets' })).toBeEnabled()
  })

  it('shows what leaving a ticket out holds, and can leave those out too', async () => {
    render(<View />)
    await userEvent.click(await screen.findByRole('checkbox', { name: 'Approve #2' }))
    expect(within(bar()).getByText(/Leaving out 1 \(#2\)\. 2 tickets you approve will wait on them: #4, #5\./)).toBeInTheDocument()
    expect(screen.getByText('Held: waits on #2', { selector: '[data-ticket="4"] .nt' })).toBeInTheDocument()
    await userEvent.click(within(bar()).getByRole('button', { name: 'Leave those 2 out too' }))
    expect(within(bar()).getByText('Approve 2 of 5 tickets')).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'Approve #4' })).not.toBeChecked()
  })

  it('story checkboxes tick the whole story and go mixed', async () => {
    render(<View />)
    const story = await screen.findByRole('checkbox', { name: 'Approve all of story 413' })
    await userEvent.click(screen.getByRole('checkbox', { name: 'Approve #1' }))
    expect(story).toHaveAttribute('aria-checked', 'mixed')
    await userEvent.click(story)
    expect(screen.getByRole('checkbox', { name: 'Approve #1' })).toBeChecked()
    await userEvent.click(story)
    expect(within(bar()).getByText('Approve 2 of 5 tickets')).toBeInTheDocument()
  })

  it('approves the ticked tickets with as_of verbatim and reports what starts', async () => {
    approveReply = { project: { id: 2 }, approved: [1, 2, 3, 4, 5], awaiting_yours: [], skipped: {} }
    const onApproved = vi.fn()
    render(<View onApproved={onApproved} />)
    await userEvent.click(await screen.findByRole('button', { name: 'Approve 5 tickets' }))
    expect(await screen.findByText('You approved 5 tickets in Human work UX')).toBeInTheDocument()
    expect(approveBodies).toEqual([{ task_ids: [1, 3, 2, 4, 5], as_of: AS_OF }])
    expect(onApproved).toHaveBeenCalled()
    expect(screen.getByText(/3 follow over 3 more steps/)).toBeInTheDocument()
    expect(screen.getByText(/#5 Ticket 5\. It's under Coming up and moves to Do when #4 is done\./)).toBeInTheDocument()
  })

  it('says a ready step of yours is under Do, not Coming up', async () => {
    plan = busyPlan()
    plan.tickets = plan.tickets.map(t => t.id === 5 ? { ...t, dependency_ids: [], waits_on: [] } : t)
    approveReply = { project: { id: 2 }, approved: [1, 2, 3, 4, 5], awaiting_yours: [], skipped: {} }
    render(<View />)
    await userEvent.click(await screen.findByRole('button', { name: 'Approve 5 tickets' }))
    expect(await screen.findByText(/#5 Ticket 5\. It's ready: it's under Do in My work\./)).toBeInTheDocument()
    expect(screen.queryByText(/#5 Ticket 5\. It's under Coming up/)).toBeNull()
  })

  it('says which tickets changed while the person was reading', async () => {
    approveReply = { project: { id: 2 }, approved: [1, 3, 2, 4], awaiting_yours: [5], skipped: { 5: 'it was planned after what was approved (as_of)' } }
    render(<View />)
    await userEvent.click(await screen.findByRole('button', { name: 'Approve 5 tickets' }))
    expect(await screen.findByText(/4 approved\. 1 changed while you were reading, so it still waits for you\./)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Review the 1 change' })).toBeInTheDocument()
  })

  it('sends on_behalf only when a lead ticks someone else\'s ticket', async () => {
    plan = busyPlan({ can_approve_on_behalf: true })
    plan.tickets[1] = { ...plan.tickets[1], yours: false, owner_name: 'Bob' }
    approveReply = { project: { id: 2 }, approved: [2], skipped: {} }
    render(<View />)
    const bobs = await screen.findByRole('checkbox', { name: 'Approve #2' })
    expect(bobs).not.toBeChecked()
    await userEvent.click(screen.getByRole('button', { name: 'None' }))
    await userEvent.click(bobs)
    await userEvent.click(screen.getByRole('button', { name: 'Approve 1 ticket (1 on behalf)' }))
    await waitFor(() => expect(approveBodies).toEqual([{ task_ids: [2], as_of: AS_OF, on_behalf: true }]))
  })

  it('disables approve at zero', async () => {
    render(<View />)
    await userEvent.click(await screen.findByRole('button', { name: 'None' }))
    expect(screen.getByRole('button', { name: 'Select tickets to approve' })).toBeDisabled()
  })

  it('reads a ticket in full, with what changed since it was approved', async () => {
    plan = busyPlan({ changed_ids: [2], new_ids: [1, 3, 4, 5], caller_last_approved_at: '2026-10-27T12:41:00Z' })
    plan.tickets[1] = {
      ...plan.tickets[1], change: 'changed', approved_at: '2026-10-27T12:41:00Z', name: 'Ticket 2, renamed',
      description: 'Line one.\nLine two, edited.', dependency_ids: [1, 3],
      changed_since: { at: '2026-10-27T12:58:00Z', last_approved_at: '2026-10-27T12:41:00Z',
        previous: { name: 'Ticket 2', description: 'Line one.\nLine two.', acceptance_criteria: '2 works.', dependency_ids: [1] } },
    }
    render(<View focus={2} />)
    const reader = await screen.findByRole('region', { name: 'Ticket 2, renamed' })
    expect(within(reader).getByText(/What changed since you approved it/)).toBeInTheDocument()
    expect(within(reader).getByText('Ticket 2', { selector: '.del' })).toBeInTheDocument()
    expect(within(reader).getByRole('button', { name: '+ #3' })).toBeInTheDocument()
    expect(within(reader).getByText('Line two.', { selector: '.del' })).toBeInTheDocument()
    expect(within(reader).getByText('Line two, edited.', { selector: '.add' })).toBeInTheDocument()
    expect(within(reader).getByText('Acceptance criteria unchanged.', { exact: false })).toBeInTheDocument()
    // Runs after lists every dependency, clickable.
    await userEvent.click(within(reader).getByRole('button', { name: 'Ticket 1' }))
    expect(await screen.findByRole('region', { name: 'Ticket 1' })).toBeInTheDocument()
  })

  it('J and K step through tickets in outline order, X ticks', async () => {
    render(<View focus={1} />)
    await screen.findByRole('region', { name: 'Ticket 1' })
    await userEvent.keyboard('j')
    expect(await screen.findByRole('region', { name: 'Ticket 3' })).toBeInTheDocument()
    await userEvent.keyboard('x')
    expect(screen.getByRole('checkbox', { name: 'Approve #3' })).not.toBeChecked()
    await userEvent.keyboard('k')
    expect(await screen.findByRole('region', { name: 'Ticket 1' })).toBeInTheDocument()
  })

  it('collapses a story whose tickets are all approved', async () => {
    plan = busyPlan({ awaiting_ids: [4, 5], yours_ids: [4, 5], for_user_ids: [4, 5], caller_last_approved_at: '2026-10-27T12:41:00Z' })
    plan.tickets = plan.tickets.map(t => t.story_id === 413 ? { ...t, awaiting: false, change: null, approved_at: '2026-10-27T12:41:00Z' } : t)
    render(<View />)
    expect(await screen.findByText(/all 3 approved/)).toBeInTheDocument()
    expect(document.querySelector('[data-ticket="1"]')).toBeNull()
    expect(screen.getByText('2 of 5', { exact: false })).toBeInTheDocument()
  })

  it('says when nothing awaits the person', async () => {
    plan = busyPlan({ awaiting_ids: [], yours_ids: [], for_user_ids: [] })
    plan.tickets = plan.tickets.map(t => ({ ...t, awaiting: false }))
    render(<View />)
    expect(await screen.findByText('Nothing awaits your approval in AMP v2')).toBeInTheDocument()
  })
})

describe('ApproveCard', () => {
  const item = (epic: number, count: number): ProjectWaitItem => ({
    kind: 'approve', key: `approve:${epic}`, project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2', task_ids: [1, 2, 3, 4, 5],
    name: 'Human work UX', epic_id: epic, epic_name: epic === 172 ? 'Human work UX' : 'Dex', count, since: new Date().toISOString(), plan: busyPlan(),
  })
  it('summarizes the plan and opens the Approve view', async () => {
    const onReview = vi.fn()
    const items = [item(172, 5), item(2, 3)]
    render(<ApproveCard item={items[0]} allItems={items} onReview={onReview} />)
    expect(screen.getByText('Build: backend')).toBeInTheDocument()
    expect(screen.getByText('steps 3–4')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Review and approve 5' }))
    expect(onReview).toHaveBeenCalledWith([172], 1)
    await userEvent.click(screen.getByRole('button', { name: 'Review both plans together (8)' }))
    expect(onReview).toHaveBeenLastCalledWith('all', 1)
  })
})

