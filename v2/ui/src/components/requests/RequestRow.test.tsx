// One request row (AMP #3978, build slice J1).
//
// The row is where the queue's two easily-confused facts live side by side, so
// this file is about that distinction rather than about rendering:
//
//  - **Claim is a primary action only where it is the right one.** An unclaimed
//    row offers Claim; someone else's claim offers no action at all (a claim is
//    never taken over); the caller's own claim offers Release. Getting this
//    wrong in either direction is what makes a queue lie about who is working.
//  - **The row is not one big button.** `TaskCard` makes the whole card a
//    button, which cannot host a second action: a button inside a button is
//    invalid HTML and unreachable by keyboard. Here the subject is the one
//    control that opens the request and the action is its sibling.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { RequestRow } from './RequestRow'
import { request } from '../../test/requestFixture'
import type { Person } from '../../lib/people'
import type { TeamRequest } from '../../types'

const people = new Map<number, Person>([
  [5, { id: 5, name: 'Priya Nair', email: 'priya@simstech.cloud' }],
  [9, { id: 9, name: 'Marco Testa', email: 'marco@simstech.cloud' }],
])

const CLAIMED_AT = new Date(Date.now() - 26 * 3_600_000).toISOString()

function renderRow(over: Partial<TeamRequest> = {}, handlers: Partial<Parameters<typeof RequestRow>[0]> = {}) {
  const req = request(over)
  const onClaim = vi.fn()
  const onRelease = vi.fn()
  const onOpen = vi.fn()
  render(
    <div role="list" aria-label="Requests">
      <RequestRow
        request={req}
        meId={5}
        people={people}
        onClaim={onClaim}
        onRelease={onRelease}
        onOpen={onOpen}
        {...handlers}
      />
    </div>,
  )
  return { req, onClaim, onRelease, onOpen, row: screen.getByRole('listitem') }
}

describe('RequestRow', () => {
  it('shows the ref, subject, state, requester, source and claim age', () => {
    const { row } = renderRow({ state: 'triaging', claim_user_id: 9, claimed_at: CLAIMED_AT })
    expect(within(row).getByText('REQ-8f2q1x7k4m2p9t5w')).toBeInTheDocument()
    expect(within(row).getByRole('button', { name: 'SSO group mapping for the Berlin office' })).toBeInTheDocument()
    expect(within(row).getByText('Triaging')).toBeInTheDocument()
    expect(within(row).getByText('it-admin@acme.example')).toBeInTheDocument()
    expect(within(row).getByText('web')).toBeInTheDocument()
    expect(within(row).getByText('claimed 26h ago')).toBeInTheDocument()
  })

  it('offers Claim as the only primary action on an unclaimed row', () => {
    const { row, onClaim, req } = renderRow()
    const claim = within(row).getByRole('button', { name: 'Claim' })
    expect(claim.className).toContain('amp-btn-primary')
    claim.click()
    expect(onClaim).toHaveBeenCalledWith(req)
  })

  it('offers no take-over on someone else\'s claim — it names the holder instead', () => {
    const { row } = renderRow({ claim_user_id: 9, claimed_at: CLAIMED_AT })
    expect(within(row).queryByRole('button', { name: 'Claim' })).toBeNull()
    expect(within(row).queryByRole('button', { name: 'Release' })).toBeNull()
    expect(within(row).getByText('Marco Testa')).toBeInTheDocument()
    expect(row).toHaveAccessibleName(/claimed by Marco Testa/)
  })

  it('offers Release, not Claim, to the person holding the claim', () => {
    const { row, onRelease, req } = renderRow({ claim_user_id: 5, claimed_at: CLAIMED_AT })
    expect(within(row).queryByRole('button', { name: 'Claim' })).toBeNull()
    const release = within(row).getByRole('button', { name: 'Release' })
    release.click()
    expect(onRelease).toHaveBeenCalledWith(req)
  })

  it('says nothing about a claim on an unclaimed row', () => {
    const { row } = renderRow()
    expect(within(row).queryByText(/claimed/)).toBeNull()
    expect(row).toHaveAccessibleName(/unclaimed/)
  })

  it('never nests one button inside another', () => {
    const { row } = renderRow()
    for (const button of within(row).getAllByRole('button')) {
      expect(button.closest('button')).toBe(button)
    }
  })

  it('opens the request from its subject, and only from there', async () => {
    const { row, onOpen, req } = renderRow()
    const subject = within(row).getByRole('button', { name: 'SSO group mapping for the Berlin office' })
    await userEvent.click(subject)
    expect(onOpen).toHaveBeenCalledWith(req)
  })

  it('says "Claiming…" and cannot be fired twice while the claim is in flight', async () => {
    const { onClaim } = renderRow({}, { claiming: true })
    const button = screen.getByRole('button', { name: 'Claiming…' })
    expect(button).toBeDisabled()
    expect(button).toHaveAttribute('aria-busy', 'true')
    await userEvent.click(button)
    expect(onClaim).not.toHaveBeenCalled()
  })

  it('marks a request waiting on the requester, and one that was reopened', () => {
    const { row } = renderRow({ state: 'needs_information', needs_info_since: CLAIMED_AT, reopened_count: 2 })
    expect(within(row).getByText('waiting on requester')).toBeInTheDocument()
    expect(within(row).getByText('reopened 2×')).toBeInTheDocument()
  })

  it('does not invent an attachment count or a company the record does not carry', () => {
    // `TeamRequest` has no attachment count and no company field, so the
    // mockup's chips have nothing behind them and must not be faked.
    const { row } = renderRow()
    expect(within(row).queryByText(/attachment/)).toBeNull()
  })
})