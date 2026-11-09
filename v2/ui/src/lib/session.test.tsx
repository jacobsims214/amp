// A session oauth2-proxy ended (#153): the API client spots the proxy's
// answer (401, redirect to sign-in, or an HTML page), marks the session ended
// once for the whole app, and SessionEndedGate shows one "sign in again"
// prompt over the pages, which stay mounted with what was typed. amp-api's own
// refusals (#69) are not that.
import { act, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import { httpTransport, RelayError } from '../remote/transport'
import { api } from '../api/client'
import { SessionEndedGate } from '../components/shell/SessionEnded'
import { NOT_SIGNED_IN } from './apiError'
import { isSessionEnded, markSessionEnded, resetSession, sessionLost, setSignInOff } from './session'

let projects: () => Response = () => HttpResponse.json({ projects: [] })
let sent = 0
const server = setupServer(
  http.get('/api/projects', () => { sent++; return projects() }),
  http.get('/api/sessions', () => { sent++; return HttpResponse.json({ sessions: [] }) }),
  http.get('/api/me', () => HttpResponse.json({ subject: 's', email: 'a@b.c', name: 'A', roles: [] })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { resetSession(); sent = 0 })
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('sessionLost', () => {
  const html = { 'Content-Type': 'text/html; charset=utf-8' }
  it('is the proxy: a 401, a redirect, or a 2xx HTML page', () => {
    expect(sessionLost(new Response('Unauthorized', { status: 401 }))).toBe(true)
    expect(sessionLost(new Response(null, { status: 302, headers: { Location: '/oauth2/start' } }))).toBe(true)
    expect(sessionLost(new Response('<html>Sign in</html>', { status: 200, headers: html }))).toBe(true)
  })
  it('is not an HTML error page: an outage, not a lost session', () => {
    expect(sessionLost(new Response('<html>502 Bad Gateway</html>', { status: 502, headers: html }))).toBe(false)
    expect(sessionLost(new Response('<html>503</html>', { status: 503, headers: html }))).toBe(false)
    expect(sessionLost(new Response('<html>Forbidden</html>', { status: 403, headers: html }))).toBe(false)
  })
  it("is not amp-api answering, refusals included", () => {
    expect(sessionLost(Response.json({ projects: [] }))).toBe(false)
    expect(sessionLost(Response.json({ error: 'Signed in as a@b.c, but this AMP has no account for that email.' }, { status: 403 }))).toBe(false)
    expect(sessionLost(Response.json({ error: 'not found' }, { status: 404 }))).toBe(false)
    expect(sessionLost(new Response(null, { status: 204 }))).toBe(false)
  })
})

describe('the API client', () => {
  it('a 401 ends the session and throws the sign-in-again sentence', async () => {
    projects = () => new HttpResponse('Unauthorized', { status: 401, headers: { 'Content-Type': 'text/plain' } })
    await expect(api.listProjects()).rejects.toMatchObject({ message: NOT_SIGNED_IN, status: 401, sessionEnded: true })
    expect(isSessionEnded()).toBe(true)
  })

  it('a redirect to sign-in ends the session (not followed)', async () => {
    projects = () => new HttpResponse(null, { status: 302, headers: { Location: '/oauth2/start?rd=%2Fapi%2Fprojects' } })
    await expect(api.listProjects()).rejects.toMatchObject({ message: NOT_SIGNED_IN, sessionEnded: true })
    expect(isSessionEnded()).toBe(true)
  })

  it('an HTML page where JSON belongs ends the session', async () => {
    projects = () => new HttpResponse('<!doctype html><title>Sign in</title>', { status: 200, headers: { 'Content-Type': 'text/html' } })
    await expect(api.listProjects()).rejects.toMatchObject({ sessionEnded: true })
    expect(isSessionEnded()).toBe(true)
  })

  it("a gateway's 502 HTML page is an error, not an ended session", async () => {
    projects = () => new HttpResponse('<html><center><h1>502 Bad Gateway</h1></center></html>', { status: 502, headers: { 'Content-Type': 'text/html' } })
    await expect(api.listProjects()).rejects.toMatchObject({ status: 502 })
    expect(isSessionEnded()).toBe(false)
  })

  it("amp-api's 403 keeps its reason and leaves the session alone (#69)", async () => {
    const why = 'Only an org owner or admin can see this.'
    projects = () => HttpResponse.json({ error: why }, { status: 403 })
    await expect(api.listProjects()).rejects.toMatchObject({ message: why, status: 403 })
    expect(isSessionEnded()).toBe(false)
  })

  it('once the session ended, nothing more is sent: pollers go quiet', async () => {
    markSessionEnded()
    await expect(api.listProjects()).rejects.toThrow(NOT_SIGNED_IN)
    await expect(httpTransport.listSessions()).rejects.toBeInstanceOf(RelayError)
    expect(sent).toBe(0)
  })

  it('with sign-in off (Local Dev) nothing is marked ended', async () => {
    setSignInOff(true)
    projects = () => new HttpResponse('Unauthorized', { status: 401 })
    await expect(api.listProjects()).rejects.toMatchObject({ status: 401 })
    expect(isSessionEnded()).toBe(false)
  })
})

function Draft() {
  const [text, setText] = useState('')
  return <textarea aria-label="Comment" value={text} onChange={e => setText(e.target.value)} />
}

describe('SessionEndedGate', () => {
  it('puts one sign-in prompt over the page and keeps what was typed', async () => {
    window.history.replaceState(null, '', '/project/3/board?focus=T-1')
    render(<SessionEndedGate><Draft /><p role="alert">Load failed</p></SessionEndedGate>)
    fireEvent.change(screen.getByLabelText('Comment'), { target: { value: 'a long comment' } })

    act(() => markSessionEnded())
    const dialog = await screen.findByRole('alertdialog', { name: 'Your session ended' })
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    const signIn = screen.getByRole('link', { name: 'Sign in again' })
    expect(signIn).toHaveAttribute('href', '/oauth2/start?rd=%2Fproject%2F3%2Fboard%3Ffocus%3DT-1')
    expect(signIn).toHaveFocus()
    // The page is still there, untouched but inert behind the prompt.
    const draft = screen.getByDisplayValue('a long comment')
    expect(draft.closest('[inert]')).not.toBeNull()

    // Stay on this page: the prompt becomes a banner and the page is usable to copy from.
    fireEvent.click(screen.getByRole('button', { name: 'Stay on this page' }))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Session ended' })).toBeInTheDocument()
    expect(screen.getByLabelText('Comment')).toHaveValue('a long comment')
    expect(screen.getByLabelText('Comment').closest('[inert]')).toBeNull()
  })
})
