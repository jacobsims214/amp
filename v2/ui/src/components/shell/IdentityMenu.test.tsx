import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { IdentityMenu } from './IdentityMenu'
import type { Me } from '../../types'

const local: Me = { subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: ['admin'], user_id: 1, local_dev: true }
const person: Me = { subject: 'oidc|1', email: 'jo@x.io', name: 'Jo', roles: [], user_id: 7 }
const noUser: Me = { subject: 'oidc|2', email: 'sam@x.io', name: 'Sam', roles: [] }

const open = async (me: Me) => {
  render(<MemoryRouter><IdentityMenu me={me} roleLabel="admin" /></MemoryRouter>)
  const button = screen.getByRole('button', { name: /Account:/ })
  expect(button).toHaveAttribute('aria-expanded', 'false')
  await userEvent.click(button)
  expect(button).toHaveAttribute('aria-expanded', 'true')
  return screen.getByRole('group', { name: 'Your account' })
}

describe('IdentityMenu (#124)', () => {
  it('local mode: identity details and "sign-in is off", no Sign in or Sign out', async () => {
    const menu = await open(local)
    expect(menu).toHaveTextContent('local-dev@amp.local')
    expect(menu).toHaveTextContent('Local mode — sign-in is off')
    expect(screen.queryByRole('link', { name: /Sign (in|out)/ })).toBeNull()
    expect(screen.getByRole('link', { name: /Agents & tokens/ })).toHaveAttribute('href', '/access')
  })
  it('signed in: Sign out', async () => {
    await open(person)
    expect(screen.getByRole('link', { name: /Sign out/ })).toHaveAttribute('href', '/oauth2/sign_out?rd=%2F')
    expect(screen.queryByText(/sign-in is off/)).toBeNull()
  })
  it('sign-in on but no AMP user: Sign in', async () => {
    await open(noUser)
    expect(screen.getByRole('link', { name: /Sign in/ }).getAttribute('href')).toMatch(/^\/oauth2\/start\?rd=/)
  })
  it('Escape closes it', async () => {
    await open(local)
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('group', { name: 'Your account' })).toBeNull()
  })
})
