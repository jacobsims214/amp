// The project's GitHub token form (AMP #3161, API #3155): write-only, owner-only.
//
// The tests here are the rules the ticket names, one describe each:
//   - nothing prefills or retains the token (the field starts empty and is
//     emptied on every answer, refusals included);
//   - the token travels in a body, never in a URL, and reaches no storage;
//   - the status a person reads is configured / on / error, and an error is
//     the server's problem class, never a message and never the token.
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import type { GitHubCredentialInfo } from '../types'

const { GitHubCredentialPanel } = await import('./GitHubCredentialPanel')

// A made-up shape for the test, not a real credential.
const TOKEN = 'ghp_TESTONLY0000000000000000000000000000'
const OTHER_TOKEN = 'ghp_TESTONLY1111111111111111111111111111'

const off: GitHubCredentialInfo = { configured: false, host: 'github.com', encrypted: true }
const on: GitHubCredentialInfo = {
  configured: true,
  host: 'github.com',
  encrypted: true,
  credential: {
    id: 7, project_id: 3, host: 'github.com', label: '', created_by: 1,
    created_at: '2026-11-01T10:00:00Z',
  },
}

let status: () => Response | Promise<Response> = () => HttpResponse.json(off)
let putAnswer: () => Response | Promise<Response> = () => HttpResponse.json(on)
let rotateAnswer: () => Response | Promise<Response> = () => HttpResponse.json(on)
let deleteAnswer: () => Response | Promise<Response> = () => HttpResponse.json({ deleted: true, host: 'github.com' })

/** Every request the panel made, so a test can look at the URL and body. */
interface Call { method: string; url: string; body: string }
let calls: Call[] = []

const server = setupServer(
  http.get('*/api/projects/3/github-credential', () => status()),
  http.put('*/api/projects/3/github-credential', async ({ request }) => { calls.push({ method: 'PUT', url: request.url, body: await request.text() }); return putAnswer() }),
  http.post('*/api/projects/3/github-credential/rotate', async ({ request }) => { calls.push({ method: 'POST', url: request.url, body: await request.text() }); return rotateAnswer() }),
  http.delete('*/api/projects/3/github-credential', ({ request }) => { calls.push({ method: 'DELETE', url: request.url, body: '' }); return deleteAnswer() }),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
beforeEach(() => {
  status = () => HttpResponse.json(off)
  putAnswer = () => HttpResponse.json(on)
  rotateAnswer = () => HttpResponse.json(on)
  deleteAnswer = () => HttpResponse.json({ deleted: true, host: 'github.com' })
  calls = []
  localStorage.clear()
  sessionStorage.clear()
})
afterEach(() => {
  server.resetHandlers()
})
afterAll(() => server.close())

const renderPanel = () => render(<MemoryRouter><GitHubCredentialPanel projectId={3} /></MemoryRouter>)
const field = () => screen.getByLabelText(/^token$/i) as HTMLInputElement

describe('GitHubCredentialPanel: the token is write-only', () => {
  it('starts empty and never prefills from the status answer', async () => {
    status = () => HttpResponse.json(on)
    renderPanel()
    const input = await waitFor(() => field())
    expect(input).toHaveValue('')
    expect(input).toHaveAttribute('type', 'password')
    expect(input).toHaveAttribute('autocomplete', 'off')
  })

  it('attaches in a body, then clears the field and never shows the token again', async () => {
    const user = userEvent.setup()
    renderPanel()
    const input = await waitFor(() => field())
    await user.type(input, TOKEN)
    expect(input).toHaveValue(TOKEN)

    // The answer is the configured status the server would send: no token in it.
    status = () => HttpResponse.json(on)
    await user.click(screen.getByRole('button', { name: /save token/i }))

    expect(await screen.findByRole('status')).toHaveTextContent(/token saved/i)
    expect(calls).toHaveLength(1)
    expect(calls[0].method).toBe('PUT')
    // The token is in the body, and nowhere in the URL.
    expect(JSON.parse(calls[0].body)).toMatchObject({ token: TOKEN })
    expect(calls[0].url).not.toContain(TOKEN)
    expect(calls[0].url).not.toContain(encodeURIComponent(TOKEN))

    // The field is empty again, and the page holds no copy of the value.
    await waitFor(() => expect(field()).toHaveValue(''))
    expect(document.body.textContent).not.toContain(TOKEN)
  })

  it('replaces through rotate when a token is already attached', async () => {
    status = () => HttpResponse.json(on)
    const user = userEvent.setup()
    renderPanel()
    const input = await waitFor(() => field())
    await user.type(input, OTHER_TOKEN)
    await user.click(screen.getByRole('button', { name: /replace token/i }))

    expect(await screen.findByRole('status')).toHaveTextContent(/token replaced/i)
    expect(calls.map(c => c.method)).toEqual(['POST'])
    expect(calls[0].url).toContain('/github-credential/rotate')
    expect(calls[0].url).not.toContain(OTHER_TOKEN)
    expect(JSON.parse(calls[0].body)).toMatchObject({ token: OTHER_TOKEN })
  })

  it('keeps the typed value when the server refuses, so a retry needs no re-paste', async () => {
    rotateAnswer = () => HttpResponse.json({ error: 'storing the GitHub credential failed' }, { status: 400 })
    status = () => HttpResponse.json(on)
    const user = userEvent.setup()
    renderPanel()
    const input = await waitFor(() => field())
    await user.type(input, OTHER_TOKEN)
    await user.click(screen.getByRole('button', { name: /replace token/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent(/storing the GitHub credential failed/i)
    expect(field()).toHaveValue(OTHER_TOKEN)
  })

  it('never writes the token to local or session storage', async () => {
    const user = userEvent.setup()
    renderPanel()
    const input = await waitFor(() => field())
    await user.type(input, TOKEN)
    status = () => HttpResponse.json(on)
    await user.click(screen.getByRole('button', { name: /save token/i }))
    await screen.findByRole('status')

    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    expect(JSON.stringify({ ...localStorage })).not.toContain(TOKEN)
  })

  it('sends nothing at all until the person asks, and says so when the field is empty', async () => {
    const user = userEvent.setup()
    renderPanel()
    await waitFor(() => field())
    await user.click(screen.getByRole('button', { name: /save token/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/paste the token first/i)
    expect(calls).toEqual([])
  })
})

describe('GitHubCredentialPanel: status and errors', () => {
  it('shows an unconfigured project what a token is for', async () => {
    renderPanel()
    expect(await screen.findByText(/no token attached/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /save token/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /remove token/i })).not.toBeInTheDocument()
  })

  it('shows the attached credential with when it was attached, and no way to read it back', async () => {
    status = () => HttpResponse.json({
      ...on, credential: { ...on.credential!, rotated_at: '2026-11-03T09:00:00Z', label: 'ci' },
    })
    renderPanel()
    expect(await screen.findByText(/^On$/)).toBeInTheDocument()
    expect(screen.getByText('ci')).toBeInTheDocument()
    expect(screen.getByText(/^Replaced/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /replace token/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /remove token/i })).toBeInTheDocument()
    // Nothing on the page is a reveal control: there is no value to reveal.
    expect(screen.queryByRole('button', { name: /show|reveal/i })).not.toBeInTheDocument()
  })

  it('renders the no-encryption-key class as a sentence and disables saving', async () => {
    status = () => HttpResponse.json({ configured: false, host: 'github.com', encrypted: false, last_error: 'no_encryption_key' })
    renderPanel()
    expect(await screen.findByText(/has no encryption key/i)).toBeInTheDocument()
    expect(field()).toBeDisabled()
    expect(screen.getByRole('button', { name: /save token/i })).toBeDisabled()
  })

  it('reports an unknown problem class as unknown, without echoing it', async () => {
    status = () => HttpResponse.json({ configured: false, host: 'github.com', encrypted: true, last_error: 'token_refused' })
    renderPanel()
    expect(await screen.findByText(/problem with this credential that this page does not recognise/i)).toBeInTheDocument()
    // The raw class is not a message; it never reaches the page.
    expect(document.body.textContent).not.toContain('token_refused')
  })

  it('reports a permission refusal in the owner\'s words rather than the API\'s', async () => {
    putAnswer = () => HttpResponse.json({ error: 'project 3: requires owner access, you have lead' }, { status: 403 })
    const user = userEvent.setup()
    renderPanel()
    const input = await waitFor(() => field())
    await user.type(input, TOKEN)
    await user.click(screen.getByRole('button', { name: /save token/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/only project owners can attach or replace/i)
  })

  it('removes only after confirming, and says what removing did', async () => {
    status = () => HttpResponse.json(on)
    const user = userEvent.setup()
    renderPanel()
    await user.click(await screen.findByRole('button', { name: /remove token/i }))
    expect(calls).toEqual([])  // the first click only arms the confirmation
    await user.click(screen.getByRole('button', { name: /remove it/i }))

    expect(await screen.findByRole('status')).toHaveTextContent(/token removed/i)
    expect(calls.map(c => c.method)).toEqual(['DELETE'])
    expect(calls[0].url).not.toContain(TOKEN)
    // Back to the unconfigured state the person started from.
    expect(await screen.findByText(/no token attached/i)).toBeInTheDocument()
  })

  it('tells the person when a replacement found nothing left to replace', async () => {
    rotateAnswer = () => HttpResponse.json({ error: 'this project has no GitHub credential to rotate: attach one instead' }, { status: 404 })
    status = () => HttpResponse.json(on)
    const user = userEvent.setup()
    renderPanel()
    const input = await waitFor(() => field())
    await user.type(input, OTHER_TOKEN)
    await user.click(screen.getByRole('button', { name: /replace token/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/nothing changed/i)
  })

  it('says the load failed instead of rendering an empty form', async () => {
    status = () => HttpResponse.json({ error: 'project 3 not found' }, { status: 404 })
    renderPanel()
    expect(await screen.findByRole('alert')).toHaveTextContent('project 3 not found')
    expect(screen.queryByLabelText(/github token/i)).not.toBeInTheDocument()
  })
})

describe('GitHubCredentialPanel: the secret never leaves the body', () => {
  it('no URL, error or rendered text a person can see carries the token', async () => {
    const user = userEvent.setup()
    renderPanel()
    const input = await waitFor(() => field())
    await user.type(input, TOKEN)
    status = () => HttpResponse.json(on)
    await user.click(screen.getByRole('button', { name: /save token/i }))
    await screen.findByRole('status')

    const seen = [
      ...calls.map(c => c.url),
      ...calls.map(c => c.method),
      screen.getByText(/token saved/i).textContent ?? '',
      document.body.innerHTML,
    ]
    for (const s of seen) expect(s).not.toContain(TOKEN)
    // The body is the one place it does travel, which is the design.
    expect(JSON.parse(calls[0].body).token).toBe(TOKEN)
  })
})
