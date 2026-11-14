// RequestHistory (AMP #3979): the append-only event list, its sentences, and the
// two rules a history must not break — it invents nothing, and a requester's
// arm cannot show an internal actor.
//
//  1. **One row per act, in the row's own words.** The sentence comes from the
//     event's fields (its from/to states, its own detail JSON), so a row cannot
//     claim a state change the record did not make.
//  2. **The detail JSON is never rendered raw.** A malformed or unexpected
//     `detail` degrades to a generic verb; `link`/`edit` rows read their
//     documented shapes and ignore the rest.
//  3. **Actor labels are the recorded snapshot**, relayed rows are marked, and
//     timestamps are text with the exact moment on `title` (never a live clock).
//  4. **A requester's activity is milestones, not events.** The requester arm
//     takes `RequesterMilestone[]`, so the claim/assignee/link rows that carry
//     user ids have no way in.
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { RequestHistory } from './RequestHistory'
import { REQUEST_STATE_LABEL } from '../../lib/requestState'
import type { RequestEvent, RequesterMilestone } from '../../types'

const event = (over: Partial<RequestEvent> = {}): RequestEvent => ({
  id: 1,
  request_id: 7,
  org_id: 2,
  team_id: 4,
  action: 'submit',
  actor: 'it-admin@acme.example',
  actor_user_id: 42,
  created_at: '2026-11-14T10:00:00Z',
  ...over,
})

describe('the event list', () => {
  it('renders each act as one row, with the actor the row recorded', () => {
    const events: RequestEvent[] = [
      event({ id: 1, action: 'submit' }),
      event({ id: 2, action: 'claim', actor: 'Priya Nair', actor_user_id: 11, detail: '{"claim_user_id":11}' }),
      event({ id: 3, action: 'transition', from_status: 'triaging', to_status: 'needs_information', actor: 'Priya Nair', actor_user_id: 11 }),
    ]
    render(<RequestHistory mode="team" events={events} />)
    const rows = screen.getAllByTestId('history-row')
    expect(rows.map(r => r.getAttribute('data-action'))).toEqual(['submit', 'claim', 'transition'])
    expect(within(rows[0]).getByText('it-admin@acme.example')).toBeInTheDocument()
    expect(within(rows[0]).getByTestId('history-sentence')).toHaveTextContent('submitted.')
    expect(within(rows[1]).getByTestId('history-sentence')).toHaveTextContent('claimed the request.')
    expect(within(rows[2]).getByTestId('history-sentence')).toHaveTextContent('asked the requester a question.')
    // The two states are stated too, in words rather than enum values — and in
    // the queue's own words, from the one table both surfaces read
    // (`REQUEST_STATE_LABEL`), so a pill and a history row never disagree.
    expect(within(rows[2]).getByTestId('history-states')).toHaveTextContent(`${REQUEST_STATE_LABEL.triaging} → ${REQUEST_STATE_LABEL.needs_information}`)
  })

  it('names the destination of every transition, so a row never reads "from X to Y"', () => {
    const cases: [RequestEvent['to_status'], string][] = [
      ['triaging', 'started triage'],
      ['in_progress', 'started work'],
      ['delivered', 'delivered the outcome'],
      ['requester_accepted', 'the requester accepted the outcome'],
      ['rejected', 'rejected the request'],
      ['duplicate', 'marked it a duplicate'],
    ]
    for (const [to, sentence] of cases) {
      const { unmount } = render(
        <RequestHistory mode="team" events={[event({ action: 'transition', from_status: 'accepted', to_status: to })]} />,
      )
      expect(screen.getByTestId('history-sentence')).toHaveTextContent(sentence)
      unmount()
    }
  })

  it('reads a link row\'s own detail, and never renders the JSON', () => {
    render(<RequestHistory mode="team" events={[
      event({ id: 1, action: 'link', detail: '{"link_id":3,"link_kind":"planned_as","target_kind":"task","target_id":3711}' }),
      event({ id: 2, action: 'unlink', detail: '{"link_id":4,"link_kind":"relates_to","target_kind":"request","target_id":88}' }),
    ]} />)
    const rows = screen.getAllByTestId('history-row')
    expect(within(rows[0]).getByTestId('history-sentence')).toHaveTextContent('linked a planned ticket #3711.')
    expect(within(rows[1]).getByTestId('history-sentence')).toHaveTextContent('removed the link to a related request #88.')
    expect(screen.queryByText(/link_kind/)).toBeNull()
  })

  it('names the fields an edit changed, and degrades a malformed detail to a generic verb', () => {
    render(<RequestHistory mode="team" events={[
      event({ id: 1, action: 'edit', detail: '{"fields":["subject","input_brief"],"from_rev":7,"to_rev":8}' }),
      event({ id: 2, action: 'edit', detail: 'not json at all' }),
      event({ id: 3, action: 'link', detail: '{"target_kind":"artifact"' }),
    ]} />)
    const rows = screen.getAllByTestId('history-row')
    expect(within(rows[0]).getByTestId('history-sentence')).toHaveTextContent('edited the subject and the brief.')
    expect(within(rows[1]).getByTestId('history-sentence')).toHaveTextContent('edited the request.')
    // A truncated JSON body never reaches the page as text.
    expect(within(rows[2]).getByTestId('history-sentence')).not.toHaveTextContent('target_kind')
    expect(screen.queryByText(/target_kind/)).toBeNull()
  })

  it('marks a relayed act and shows the person behind it', () => {
    render(<RequestHistory mode="team" events={[event({ action: 'claim', actor: 'jacob.sims@simstech.cloud (relayed by agent)' })]} />)
    expect(screen.getByText('jacob.sims@simstech.cloud')).toBeInTheDocument()
    expect(screen.getByText('Relayed')).toBeInTheDocument()
  })

  it('puts the exact moment on the timestamp\'s title, so the list shows an age not a clock', () => {
    render(<RequestHistory mode="team" events={[event({ created_at: '2026-11-14T10:00:00Z' })]} />)
    const time = screen.getByText((_, el) => el?.tagName === 'TIME' && el.getAttribute('datetime') === '2026-11-14T10:00:00Z')
    expect(time).toHaveAttribute('title', new Date('2026-11-14T10:00:00Z').toLocaleString())
  })

  it('collapses a long history behind "Show all" and restores it', async () => {
    const events = Array.from({ length: 9 }, (_, i) => event({ id: i + 1, action: 'edit', detail: '{"fields":["subject"]}' }))
    render(<RequestHistory mode="team" events={events} collapsedAfter={3} />)
    expect(screen.getAllByTestId('history-row')).toHaveLength(3)
    const toggle = screen.getByRole('button', { name: /show all 9/i })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    await userEvent.click(toggle)
    expect(screen.getAllByTestId('history-row')).toHaveLength(9)
    expect(screen.getByRole('button', { name: /show the last few/i })).toHaveAttribute('aria-expanded', 'true')
  })

  it('says so when nothing has happened, and does not invent a submit row', () => {
    render(<RequestHistory mode="team" events={[]} />)
    expect(screen.getByTestId('request-panel-empty')).toHaveTextContent('No acts recorded yet.')
    expect(screen.queryAllByTestId('history-row')).toHaveLength(0)
  })

  it('never turns a linked ticket\'s closure into a request event (I8)', () => {
    // The record is what it is: the states here are the request's own, so the
    // row's sentence cannot claim a delivery that did not happen.
    render(<RequestHistory mode="team" events={[event({ action: 'transition', from_status: 'in_progress', to_status: 'in_progress' })]} />)
    expect(screen.getByTestId('history-sentence')).not.toHaveTextContent(/delivered/i)
  })
})

describe('the requester\'s activity', () => {
  const milestones: RequesterMilestone[] = [
    { key: 'received', done: true, at: '2026-11-08T09:41:00Z' },
    { key: 'reviewing', done: true, at: '2026-11-08T11:05:00Z' },
    { key: 'in_progress', done: true, at: '2026-11-09T10:15:00Z', note: 'Re-running the export.' },
    { key: 'delivered', done: false },
    { key: 'accepted', done: false },
  ]

  it('lists only the steps that happened, with their public sentences', () => {
    render(<RequestHistory mode="requester" milestones={milestones} />)
    const rows = screen.getAllByTestId('history-row')
    expect(rows.map(r => r.getAttribute('data-milestone'))).toEqual(['received', 'reviewing', 'in_progress'])
    expect(within(rows[0]).getByText('Request received')).toBeInTheDocument()
    expect(within(rows[2]).getByText('Re-running the export.')).toBeInTheDocument()
    // A pending step is not an event: the stepper above already shows what waits.
    expect(screen.queryByText('Delivered')).toBeNull()
  })

  it('says nothing rather than "not yet" when no step has happened', () => {
    render(<RequestHistory mode="requester" milestones={[]} />)
    expect(screen.getByTestId('request-panel-empty')).toHaveTextContent('Nothing yet.')
  })
})
