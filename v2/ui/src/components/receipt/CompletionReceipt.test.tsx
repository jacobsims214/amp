import { render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { CompletionReceipt } from './CompletionReceipt'
import type { Comment, Run, Task } from '../../types'

let artifacts = 0
let fail = false
const server = setupServer(
  http.get('*/api/projects/2/artifacts', () => fail ? HttpResponse.json({}, { status: 500 }) : HttpResponse.json({ artifacts: Array.from({ length: artifacts }, (_, i) => ({ id: i + 1 })) })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => { server.resetHandlers(); artifacts = 0; fail = false })
afterAll(() => server.close())

const task = (over: Partial<Task> = {}): Task => ({
  id: 5, project_id: 2, epic_id: 1, story_id: 1, name: 't', description: 'd', acceptance_criteria: 'a', state: 'completed',
  priority: '1', dependency_ids: [], created_at: '2026-10-27T10:00:00Z', updated_at: '2026-10-27T10:00:00Z',
  completed_at: new Date(Date.now() - 2 * 3600e3).toISOString(), ...over,
})
const run = (over: Partial<Run> = {}): Run => ({
  id: 1, task_id: 5, project_id: 2, outcome: 'completed', agent_id: 'amp-worker-frontend', model: 'claude-opus-5-5',
  created_at: new Date(Date.now() - 2 * 3600e3).toISOString(), ...over,
})
const comment = (body: string): Comment => ({ id: 1, task_id: 5, body, author: 'a', created_at: '2026-11-01T10:00:00Z' })
const CHECK = 'Work complete.\n- A: VERIFIED\n- B: VERIFIED\n- C: VERIFIED\n- D: COULD NOT VERIFY — no browser'

describe('CompletionReceipt', () => {
  it('shows the self-check from the comment as Unverified', () => {
    render(<CompletionReceipt task={task()} runs={[run()]} comments={[comment(CHECK)]} />)
    expect(screen.getByTestId('receipt-check')).toHaveTextContent('Self-checked (from comment) 3/4')
    expect(screen.getByTestId('receipt-status')).toHaveTextContent('Unverified')
    expect(screen.getByTestId('receipt-unverified')).toHaveTextContent('D: no browser')
    expect(screen.getByTestId('receipt-done')).toHaveTextContent('Done by amp-worker-frontend · claude-opus-5-5 · 2h ago')
  })

  it('says so when no self-check was posted', () => {
    render(<CompletionReceipt task={task()} runs={[run()]} comments={[comment('Work complete. Shipped it.')]} />)
    expect(screen.getByTestId('receipt-check')).toHaveTextContent('No self-check posted')
  })

  it('shows the branch, short sha, base and PR from the latest run that has them', () => {
    const runs = [
      run({ id: 1, branch: 'old', commit: 'aaaaaaaaaa' }),
      run({ id: 2, branch: 'ux/receipts', commit: '3f2a9c1deadbeef', base_branch: 'main', pr_url: 'https://github.com/o/r/pull/9' }),
      run({ id: 3, outcome: 'progress' }),
    ]
    render(<CompletionReceipt task={task({ merge_state: 'needed' })} runs={runs} comments={[]} />)
    const code = screen.getByTestId('receipt-code')
    expect(code).toHaveTextContent('ux/receipts @ 3f2a9c1 → main')
    expect(code).not.toHaveTextContent('3f2a9c1d')
    expect(screen.getByRole('link', { name: 'PR' })).toHaveAttribute('href', 'https://github.com/o/r/pull/9')
  })

  it('shows the merge status once', () => {
    render(<CompletionReceipt task={task({ merge_state: 'needed' })} runs={[run({ branch: 'b' })]} comments={[]} />)
    expect(screen.getAllByTestId('merge-status')).toHaveLength(1)
    expect(screen.getByTestId('merge-status')).toHaveTextContent('Done · Merge needed')
  })

  it('tags a closeout completion', () => {
    render(<CompletionReceipt task={task()} runs={[run({ note: 'closeout: finished by hand' })]} comments={[]} />)
    expect(screen.getByText('Closeout')).toBeInTheDocument()
  })

  it('has no Closeout tag for a normal completion', () => {
    render(<CompletionReceipt task={task()} runs={[run({ note: 'did it' })]} comments={[]} />)
    expect(screen.queryByText('Closeout')).toBeNull()
  })

  it('counts artifacts, and hides the line at zero', async () => {
    artifacts = 2
    const { unmount } = render(<CompletionReceipt task={task()} runs={[run()]} comments={[]} />)
    expect(await screen.findByText('2 artifacts')).toBeInTheDocument()
    unmount()
    artifacts = 0
    render(<CompletionReceipt task={task()} runs={[run()]} comments={[]} />)
    await waitFor(() => expect(screen.queryByTestId('receipt-artifacts')).toBeNull())
  })

  it('does not carry one ticket\'s artifact count to the next, even if the next fetch fails', async () => {
    artifacts = 2
    const { rerender } = render(<CompletionReceipt key={5} task={task()} runs={[run()]} comments={[]} />)
    expect(await screen.findByText('2 artifacts')).toBeInTheDocument()
    fail = true
    rerender(<CompletionReceipt key={6} task={task({ id: 6 })} runs={[run()]} comments={[]} />)
    await waitFor(() => expect(screen.queryByTestId('receipt-artifacts')).toBeNull())
  })

  it('is hidden on human tickets and tickets that are not done', () => {
    const { container, rerender } = render(<CompletionReceipt task={task({ performer: 'human', human_reason: 'judgment' } as Partial<Task>)} runs={[run()]} comments={[]} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CompletionReceipt task={task({ state: 'in_progress' })} runs={[run()]} comments={[]} />)
    expect(container).toBeEmptyDOMElement()
  })
})
