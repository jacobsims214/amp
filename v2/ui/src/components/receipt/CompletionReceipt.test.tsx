import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { CompletionReceipt } from './CompletionReceipt'
import type { Comment, ReceiptCheck, Run, Task, TaskReceipt } from '../../types'

let artifacts = 0
let fail = false
const verdicts: { kind: string; note: string }[] = []
const server = setupServer(
  http.post('*/api/tasks/5/receipt/:kind', async ({ params, request }) => {
    const body = await request.json() as { note: string }
    verdicts.push({ kind: String(params.kind), note: body.note })
    return HttpResponse.json({})
  }),
  http.get('*/api/projects/2/artifacts', () => fail ? HttpResponse.json({}, { status: 500 }) : HttpResponse.json({ artifacts: Array.from({ length: artifacts }, (_, i) => ({ id: i + 1 })) })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => { server.resetHandlers(); artifacts = 0; fail = false; verdicts.length = 0 })
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

const settings = { mode: 'inform' as const, min_level: 'self_checked' as const, project_mode: 'inform' as const, org_mode: 'off' as const }
const check = (over: Partial<ReceiptCheck> = {}): ReceiptCheck => ({
  id: 1, receipt_id: 11, role: 'worker', kind: 'test', verdict: 'pass', criterion_ref: '1', checked_by: 'amp-worker',
  created_at: '2026-11-04T10:00:00Z', ...over,
})
const serverReceipt = (over: Partial<Extract<TaskReceipt, { attempts: number }>> = {}): TaskReceipt => ({
  receipt: {
    id: 11, task_id: 5, project_id: 2, state: 'final', mode: 'inform', held: false, agent_id: 'amp-worker-frontend', summary: '',
    files_changed: [], commit_sha: '3f2a9c1deadbeef', tree_sha: '', branch: 'ux/receipts', base_branch: 'main', pr_url: '', kept: false,
    created_at: '2026-11-04T10:00:00Z',
  },
  checks: [check(), check({ id: 2, criterion_ref: '2', kind: 'visual' })], evidence: [], status: 'self_checked', held: false,
  attempts: 0, settings, ...over,
})

describe('CompletionReceipt with a server receipt (#126)', () => {
  it('shows the status, the checks by role and the code from the receipt', () => {
    render(<CompletionReceipt task={task()} runs={[run()]} comments={[comment(CHECK)]} receipt={serverReceipt()} />)
    expect(screen.getByTestId('receipt-status')).toHaveTextContent('Self-checked 2/2')
    expect(screen.getByTestId('receipt-check')).not.toHaveTextContent('from comment')
    expect(screen.getByTestId('receipt-code')).toHaveTextContent('ux/receipts @ 3f2a9c1 → main')
    expect(screen.getAllByTestId('receipt-check-row')).toHaveLength(2)
    expect(screen.queryByTestId('receipt-verdict')).toBeNull() // not someone who can work it
  })

  it('falls back to the comment when the ticket has no receipt', () => {
    render(<CompletionReceipt task={task()} runs={[run()]} comments={[comment(CHECK)]} receipt={{ receipt: null, mode: 'off', settings: { ...settings, mode: 'off' } }} canWork />)
    expect(screen.getByTestId('receipt-check')).toHaveTextContent('Self-checked (from comment) 3/4')
    expect(screen.queryByTestId('receipt-verdict')).toBeNull()
  })

  it('offers Accept and Reject to someone who can work the ticket; Reject needs a note', async () => {
    const user = userEvent.setup()
    let changed = 0
    render(<CompletionReceipt task={task()} runs={[run()]} comments={[]} receipt={serverReceipt()} canWork onChanged={() => changed++} />)
    await user.click(screen.getByRole('button', { name: 'Accept' }))
    await waitFor(() => expect(verdicts).toEqual([{ kind: 'accept', note: '' }]))
    await waitFor(() => expect(changed).toBe(1))

    await user.click(screen.getByRole('button', { name: 'Reject…' }))
    const submit = screen.getByRole('button', { name: 'Reject' })
    expect(submit).toBeDisabled()
    await user.type(screen.getByLabelText("What's wrong"), '   ')
    expect(submit).toBeDisabled()
    await user.clear(screen.getByLabelText("What's wrong"))
    await user.type(screen.getByLabelText("What's wrong"), 'The button is missing')
    await user.click(submit)
    await waitFor(() => expect(verdicts[1]).toEqual({ kind: 'reject', note: 'The button is missing' }))
  })

  it('leads with "Couldn\'t verify, needs you" on a held, in-progress ticket', () => {
    const held = serverReceipt({ held: true, status: 'unverified', hold_reason: 'no checks were recorded', checks: [] })
    render(<CompletionReceipt task={task({ state: 'in_progress', completed_at: undefined })} runs={[]} comments={[]} receipt={held} canWork />)
    expect(screen.getByTestId('receipt-held')).toHaveTextContent("Couldn't verify, needs you")
    expect(screen.getByTestId('receipt-held')).toHaveTextContent('No checks were recorded.')
    expect(screen.getByRole('button', { name: 'Accept' })).toBeInTheDocument()
    expect(screen.getByTestId('receipt-status')).toHaveTextContent('Unverified')
  })

  it('names who rejected it and why, and counts earlier attempts', () => {
    const rejected = serverReceipt({ attempts: 2, checks: [check(), check({ id: 3, role: 'human', kind: 'verdict', verdict: 'fail', criterion_ref: '',
      checked_by: 'ann@example.com', note: 'wrong colour', created_at: '2026-11-04T11:00:00Z' })] })
    render(<CompletionReceipt task={task()} runs={[run()]} comments={[]} receipt={rejected} />)
    expect(screen.getByTestId('receipt-rejected')).toHaveTextContent('Rejected by ann@example.com: wrong colour')
    expect(screen.getByTestId('receipt-attempts')).toHaveTextContent('2 earlier attempts')
  })

  it('is hidden on an in-progress ticket that isn\'t held', () => {
    render(<CompletionReceipt task={task({ state: 'in_progress' })} runs={[]} comments={[]} receipt={serverReceipt()} />)
    expect(screen.queryByTestId('completion-receipt')).toBeNull()
  })
})

describe('CompletionReceipt links (security)', () => {
  it('renders no link for a javascript: pr_url, from a run or a server receipt', () => {
    const { unmount } = render(<CompletionReceipt task={task()} runs={[run({ branch: 'b', pr_url: 'javascript:alert(1)' })]} comments={[]} />)
    expect(screen.queryByRole('link', { name: 'PR' })).toBeNull()
    unmount()
    const bad = serverReceipt()
    if (bad.receipt) bad.receipt.pr_url = ' JavaScript:alert(1)'
    render(<CompletionReceipt task={task()} runs={[]} comments={[]} receipt={bad} />)
    expect(screen.queryByRole('link', { name: 'PR' })).toBeNull()
  })

  it('plays the evidence of the receipt in the drawer', () => {
    const r = serverReceipt({ evidence: [{ id: 1, receipt_id: 11, key: '2/5/attempts/11/abc/video.mp4', name: 'video.mp4', content_type: 'video/mp4',
      bytes: 1000, status: 'stored', created_at: '2026-11-04T10:00:00Z' }] })
    render(<CompletionReceipt task={task()} runs={[]} comments={[]} receipt={r} />)
    expect(screen.getByTestId('evidence-video')).toHaveAttribute('src', '/api/receipts/2/5/attempts/11/abc/video.mp4')
  })

  it("shows the comment view when the ticket completed again after its receipt (an earlier attempt's)", () => {
    const r = serverReceipt()
    if (r.receipt) r.receipt.finalized_at = '2026-11-01T10:00:00Z'
    render(<CompletionReceipt task={task({ completed_at: '2026-11-04T10:00:00Z' })} runs={[run()]} comments={[comment(CHECK)]} receipt={r} />)
    expect(screen.getByTestId('receipt-check')).toHaveTextContent('Self-checked (from comment) 3/4')
  })

  it('keeps an accepted receipt that completed the held ticket later', () => {
    const r = serverReceipt({ status: 'human_accepted' })
    if (r.receipt) r.receipt.finalized_at = '2026-11-01T10:00:00Z'
    render(<CompletionReceipt task={task({ completed_at: '2026-11-04T10:00:00Z' })} runs={[]} comments={[]} receipt={r} />)
    expect(screen.getByTestId('receipt-status')).toHaveTextContent('Accepted')
  })
})
