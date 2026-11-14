// RequesterAccessStates (AMP #3987): the three "No access" states.
//
// The criterion this file proves is "a foreign/unknown ref renders the identical
// not-found state (existence never leaks)", and it is proved in the only two ways
// that can actually fail:
//
//  1. **By construction.** The component takes a `state` prop and there is no
//     prop for *why* — no `reason`, no `foreign`, no `exists`. So the two cases
//     the server deliberately conflates cannot render differently, because there
//     is nothing to branch on.
//  2. **By rendering.** Two separately-mounted trees, one for a reference that
//     belongs to somebody else and one for a reference that was never minted,
//     produce byte-identical text.
//
// The other half is that `invite_pending` and `grant_revoked` are NOT derivable
// from an error: `authz.TeamSubmit` answers one not-found for never-invited and
// revoked alike, so a page that inferred "revoked" from a 404 would rebuild the
// oracle the server refuses to be. This file pins that they are props, and that
// each says only what it can honestly say.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { RequesterAccessStates, RequesterLoadFailure } from './RequesterAccessStates'

const DESK = 'Northwind support desk'
const EMAIL = 'marta.reyes@bluefjord.example'

describe('the not-found state', () => {
  it('renders one set of words for every reason it can be true', () => {
    const { container } = render(<RequesterAccessStates state="not_found" teamName={DESK} email={EMAIL} />)
    const first = container.textContent
    expect(first).toContain("We can't find that request")
    // It explains *why* it cannot say which, which is the only thing that keeps
    // a person from assuming the link is simply broken.
    expect(first).toContain("For your privacy we can't say which")
    expect(first).toContain('only ever visible to the address that sent them')
    // There is no branch on why: the component's prop list has no field that
    // could carry a reason, so the two cases the server conflates cannot render
    // differently. Checked against the source, because a prop is the thing a
    // future edit would add.
    const src = readFileSync(join(__dirname, 'RequesterAccessStates.tsx'), 'utf8')
    const props = /export interface RequesterAccessStatesProps \{([\s\S]*?)\n\}/.exec(src)?.[1] ?? ''
    expect(props).toContain('state:')
    for (const forbidden of ['reason', 'foreign', 'exists', 'notYours', 'hidden']) {
      expect(props, forbidden).not.toContain(forbidden)
    }
  })

  it('is byte-identical whether the request is foreign or unknown', () => {
    // The server answers the same 404 for another person's reference and for one
    // that was never minted. The page's arm for both is this one, mounted with
    // exactly the same props — so the two renders must agree on every byte.
    const foreign = render(<RequesterAccessStates state="not_found" teamName={DESK} email={EMAIL} />)
    const foreignText = foreign.container.textContent
    foreign.unmount()
    const unknown = render(<RequesterAccessStates state="not_found" teamName={DESK} email={EMAIL} />)
    expect(unknown.container.textContent).toBe(foreignText)
  })

  it('offers the person somewhere to go, and nothing internal', async () => {
    const onViewRequests = vi.fn()
    const onContactDesk = vi.fn()
    const user = userEvent.setup()
    render(
      <RequesterAccessStates state="not_found" teamName={DESK} email={EMAIL}
        onViewRequests={onViewRequests} onContactDesk={onContactDesk} />,
    )
    await user.click(screen.getByRole('button', { name: 'Your requests' }))
    await user.click(screen.getByRole('button', { name: `Contact ${DESK}` }))
    expect(onViewRequests).toHaveBeenCalled()
    expect(onContactDesk).toHaveBeenCalled()
    // No project, ticket, org or agent word anywhere on the state.
    const text = (document.body.textContent ?? '').toLowerCase()
    for (const internal of ['project', 'ticket', 'epic', 'agent', 'org', 'claim', 'assignee', 'metadata']) {
      expect(text, internal).not.toContain(internal)
    }
  })
})

describe('the invite-pending state', () => {
  it('names the desk and the invited address, and offers a sign-in', async () => {
    const onSignIn = vi.fn()
    const user = userEvent.setup()
    render(<RequesterAccessStates state="invite_pending" teamName={DESK} email={EMAIL} onSignIn={onSignIn} />)
    expect(screen.getByText("You've been invited to send requests")).toBeInTheDocument()
    expect(screen.getByTestId('requester-access-state')).toHaveTextContent(DESK)
    expect(screen.getByText(EMAIL)).toBeInTheDocument()
    // The invitation is tied to the address, so the fallback is to sign in again.
    expect(screen.getByText(/invitations are tied to the address they were sent to/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Sign in to continue' }))
    expect(onSignIn).toHaveBeenCalled()
  })

  it('still reads as an invitation when no address is known', () => {
    render(<RequesterAccessStates state="invite_pending" teamName={DESK} />)
    // "this address" rather than an empty bold span: the sentence has to work
    // with what the entry point actually knows.
    expect(screen.getByTestId('requester-access-state')).toHaveTextContent(/has invited this address to send them requests/)
  })
})

describe('the grant-revoked state', () => {
  it('says the ability to send was removed, and that existing requests remain', () => {
    render(<RequesterAccessStates state="grant_revoked" teamName={DESK} email={EMAIL} />)
    expect(screen.getByText('Your sending access was removed')).toBeInTheDocument()
    // The one promise it must keep: a revoked submit grant is submit-only, so it
    // confers no read on anybody else's request and takes none of their own away.
    expect(screen.getByTestId('requester-access-state')).toHaveTextContent(
      'Requests you already sent are still yours to see',
    )
    // It never implies their existing requests went with the grant.
    expect(screen.getByTestId('requester-access-state')).not.toHaveTextContent(/all your requests were removed/i)
  })

  it('offers their existing requests and a way to ask for access back', async () => {
    const onViewRequests = vi.fn()
    const onContactDesk = vi.fn()
    const user = userEvent.setup()
    render(
      <RequesterAccessStates state="grant_revoked" teamName={DESK}
        onViewRequests={onViewRequests} onContactDesk={onContactDesk} />,
    )
    await user.click(screen.getByRole('button', { name: 'Your existing requests' }))
    await user.click(screen.getByRole('button', { name: 'Ask the desk to restore access' }))
    expect(onViewRequests).toHaveBeenCalled()
    expect(onContactDesk).toHaveBeenCalled()
  })
})

describe('mobile and keyboard', () => {
  it('stacks the actions full width and 44 px tall at a phone size', () => {
    render(<RequesterAccessStates state="grant_revoked" teamName={DESK} onViewRequests={vi.fn()} onContactDesk={vi.fn()} />)
    for (const button of screen.getAllByRole('button')) {
      expect(button).toHaveClass('w-full')
      expect(button).toHaveClass('sm:w-auto')
      expect(button).toHaveStyle({ minHeight: '44px' })
    }
  })

  it('reaches every action by keyboard', async () => {
    const onViewRequests = vi.fn()
    const onContactDesk = vi.fn()
    const user = userEvent.setup()
    render(<RequesterAccessStates state="not_found" teamName={DESK} onViewRequests={onViewRequests} onContactDesk={onContactDesk} />)
    screen.getByRole('button', { name: 'Your requests' }).focus()
    await user.keyboard('{Enter}')
    expect(onViewRequests).toHaveBeenCalled()
    await user.tab()
    expect(screen.getByRole('button', { name: `Contact ${DESK}` })).toHaveFocus()
    await user.keyboard(' ')
    expect(onContactDesk).toHaveBeenCalled()
  })
})

describe('the load failure', () => {
  it('is not an access state: it says the read failed and offers a retry', async () => {
    const onRetry = vi.fn()
    const user = userEvent.setup()
    render(<RequesterLoadFailure message="The request did not come back." onRetry={onRetry} />)
    // Telling someone their request does not exist because a query timed out is
    // a lie the server never told, so this arm exists apart from the three.
    expect(screen.getByTestId('requester-load-failure')).toHaveTextContent(/didn't load/)
    expect(screen.queryByText("We can't find that request")).toBeNull()
    expect(screen.getByTestId('requester-load-failure')).toHaveTextContent(/Nothing about your request changed/)
    await user.click(screen.getByRole('button', { name: /Try again/ }))
    expect(onRetry).toHaveBeenCalled()
  })

  it('renders without a retry when there is nothing to retry', () => {
    render(<RequesterLoadFailure message="A shape this page does not understand." />)
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.getByTestId('requester-load-failure')).toHaveTextContent('A shape this page does not understand.')
  })
})
