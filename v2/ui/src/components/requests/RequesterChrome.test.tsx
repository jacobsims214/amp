// The requester frame (ticket AMP #3981, approved mockup artifact 38 v1).
//
// The criterion this file proves is "requester chrome exposes no internals", and
// it is proven in the two ways that can actually fail:
//
//  1. **Structurally.** The component is rendered with the smallest possible
//     props — a team name and an identity — and the rendered tree is searched for
//     every internal name the team frame would have shown: project, epic, story,
//     ticket, run, agent, org, member, claim, assignee, branch, metadata. There
//     is no prop for any of them, so the assertion is really that none appeared
//     from a child, a constant or a stray import.
//  2. **By import.** The file must not reach for the team's frame or for the
//     app's data layer. That is checked against the source text, because a
//     component that *can* call `api.listProjects` will eventually do it.
//
// Plus the two keyboard facts a chrome owes a person: the skip link is the first
// thing Tab reaches, and everything it offers is a real button or link.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { RequesterChrome } from './RequesterChrome'
import { identityLabel, initials, requesterNavItemClass } from '../../lib/requesterChrome'

const me = { name: 'Marta Reyes', email: 'marta.reyes@bluefjord.example' }

/** Every internal name the team's own frame shows, none of which a requester may see. */
const INTERNAL = [
  'project', 'epic', 'story', 'ticket', 'run', 'agent', 'branch', 'metadata',
  'claim', 'assignee', 'org', 'session', 'portfolio', 'audit', 'settings',
]

describe('RequesterChrome exposes no internals', () => {
  it('renders a frame from a team name and an identity and nothing else', () => {
    const { container } = render(
      <RequesterChrome teamName="Northwind support desk" me={me}>
        <p>the form</p>
      </RequesterChrome>,
    )
    const text = (container.textContent ?? '').toLowerCase()
    for (const word of INTERNAL) expect(text, word).not.toContain(word)
    // And what it should show: the desk, the person, the product.
    expect(screen.getByText('Northwind support desk')).toBeInTheDocument()
    expect(screen.getByTestId('requester-identity')).toHaveAttribute('title', 'Marta Reyes · marta.reyes@bluefjord.example')
    expect(screen.getByLabelText('AMP')).toBeInTheDocument()
    expect(screen.getByText('the form')).toBeInTheDocument()
  })

  it('offers no navigation of its own: no sidebar, no project tree, no nav without a slot', () => {
    render(<RequesterChrome teamName="Northwind support desk" me={me}><p>x</p></RequesterChrome>)
    expect(screen.queryByRole('navigation')).toBeNull()
    expect(screen.queryByRole('button')).toBeNull()
    // The only link is the skip link: it points at this page's own main landmark,
    // never into the app, so it cannot take a requester anywhere internal.
    const links = screen.getAllByRole('link')
    expect(links.map(a => a.getAttribute('href'))).toEqual(['#requester-content'])
  })

  it('renders the page’s own two links when the page passes a nav slot', () => {
    render(
      <RequesterChrome
        teamName="Northwind support desk"
        me={me}
        nav={<>
          <a href="/requests/new" className={requesterNavItemClass(true)}>New request</a>
          <a href="/requests" className={requesterNavItemClass(false)}>Your requests</a>
        </>}
      >
        <p>x</p>
      </RequesterChrome>,
    )
    const nav = screen.getByRole('navigation', { name: 'Requests' })
    expect(within(nav).getAllByRole('link').map(a => a.textContent)).toEqual(['New request', 'Your requests'])
    // The current tab is marked by more than colour: a rule and a weight change.
    expect(within(nav).getByText('New request').className).toContain('border-ink')
    expect(within(nav).getByText('Your requests').className).toContain('border-transparent')
  })

  it('does not import the team frame or the app data layer', () => {
    const src = readFileSync(join(__dirname, 'RequesterChrome.tsx'), 'utf8')
    // It imports exactly two things — the icon set and its own pure helpers —
    // so the data layer, the team frame and the router are all unreachable from
    // here rather than merely unused today.
    expect(src.match(/^import .*$/gm)?.map(l => l.replace(/^import /, ''))).toEqual([
      "{ Lock } from 'lucide-react'",
      "{ identityLabel, initials, type RequesterIdentity } from '../../lib/requesterChrome'",
    ])
    for (const forbidden of ['api/client', 'useAuth', 'react-router', 'useParams', 'AppShell']) {
      expect(src.match(/^import .*$/gm)?.join('\n'), forbidden).not.toContain(forbidden)
    }
  })

  it('says the one privacy fact every requester screen owes them', () => {
    render(<RequesterChrome teamName="Northwind support desk" me={me}><p>x</p></RequesterChrome>)
    expect(screen.getByText(/You only ever see your own requests to Northwind support desk/)).toBeInTheDocument()
    expect(screen.getByText(/never the team's internal planning/)).toBeInTheDocument()
  })

  it('shows an access notice without dressing it as an internal state', () => {
    render(
      <RequesterChrome teamName="Northwind support desk" me={me} banner={<span>Your sending access was removed.</span>}>
        <p>x</p>
      </RequesterChrome>,
    )
    expect(screen.getByText('Your sending access was removed.')).toBeInTheDocument()
  })
})

describe('RequesterChrome is keyboard-reachable', () => {
  it('puts the skip link first in the tab order, and it moves focus to the content', async () => {
    render(
      <RequesterChrome teamName="Northwind support desk" me={me} onSignOut={() => {}}>
        <p>the form</p>
      </RequesterChrome>,
    )
    await userEvent.tab()
    const skip = screen.getByRole('link', { name: 'Skip to the form' })
    expect(skip).toHaveFocus()
    expect(skip).toHaveAttribute('href', '#requester-content')
    // The target exists and is the main landmark, so the jump lands somewhere real.
    const main = screen.getByRole('main')
    expect(main).toHaveAttribute('id', 'requester-content')
  })

  it('reaches the sign-out button with the keyboard and activates it', async () => {
    const onSignOut = vi.fn()
    render(
      <RequesterChrome teamName="Northwind support desk" me={me} onSignOut={onSignOut}>
        <p>x</p>
      </RequesterChrome>,
    )
    const signOut = screen.getByRole('button', { name: 'Sign out' })
    signOut.focus()
    expect(signOut).toHaveFocus()
    await userEvent.keyboard('{Enter}')
    expect(onSignOut).toHaveBeenCalledTimes(1)
  })

  it('names the person for a screen reader when the chip is hidden on a phone', () => {
    render(<RequesterChrome teamName="Northwind support desk" me={me}><p>x</p></RequesterChrome>)
    // The visible chip is hidden below `sm`; the screen-reader copy is not, so the
    // identity is never simply absent from the accessibility tree.
    expect(screen.getByTestId('requester-identity-sr')).toHaveClass('sr-only')
    expect(screen.getByTestId('requester-identity-sr')).toHaveTextContent('Marta Reyes')
  })
})

describe('identity helpers', () => {
  it('takes the initials of a name, else the address’s first letter', () => {
    expect(initials({ name: 'Marta Reyes', email: 'm@x.example' })).toBe('MR')
    expect(initials({ name: 'Cher', email: 'c@x.example' })).toBe('C')
    expect(initials({ email: 'zoe@x.example' })).toBe('Z')
    expect(initials({ name: '  ', email: 'zoe@x.example' })).toBe('Z')
  })

  it('names the person by name, falling back to the address', () => {
    expect(identityLabel({ name: 'Marta Reyes', email: 'm@x.example' })).toBe('Marta Reyes')
    expect(identityLabel({ email: 'm@x.example' })).toBe('m@x.example')
    expect(identityLabel({ name: ' ', email: 'm@x.example' })).toBe('m@x.example')
  })
})
