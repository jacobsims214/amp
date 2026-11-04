// The GitHub merge webhook settings (AMP #2485): the secret is shown once.
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { GitHubHookInfo } from '../types'

const api = vi.hoisted(() => ({
  getGitHubHook: vi.fn(),
  rotateGitHubHookSecret: vi.fn(),
  deleteGitHubHook: vi.fn(),
}))
vi.mock('../api/client', () => ({ api }))

const { GitHubHookPanel } = await import('./GitHubHookPanel')
const { hookURL } = await import('../lib/githubHook')

// A made-up value for the test, not a real secret.
const FAKE_SECRET = 'test-secret-0123456789abcdef'
const setup = { payload_url: '/api/hooks/github/3', content_type: 'application/json', events: 'Pull requests' }
const off: GitHubHookInfo = { configured: false, url: '/api/hooks/github/3', deliveries: [], encrypted: true, setup }
const on: GitHubHookInfo = {
  ...off, configured: true,
  hook: { project_id: 3, created_at: '2026-10-27T10:00:00Z', rotated_at: '2026-10-27T10:00:00Z', rejected_count: 0 },
}

const renderPanel = () => render(<MemoryRouter><GitHubHookPanel projectId={3} /></MemoryRouter>)

beforeEach(() => {
  api.getGitHubHook.mockReset(); api.rotateGitHubHookSecret.mockReset(); api.deleteGitHubHook.mockReset()
})

describe('hookURL', () => {
  it('completes a path from the API with the page origin', () => {
    expect(hookURL(off, 'https://amp.example.com')).toBe('https://amp.example.com/api/hooks/github/3')
    expect(hookURL({ url: 'api/hooks/github/3' }, 'https://amp.example.com/')).toBe('https://amp.example.com/api/hooks/github/3')
  })
  it('keeps an absolute URL from the API as it is', () => {
    expect(hookURL({ url: 'https://public.example.org/api/hooks/github/3', setup: { payload_url: 'https://public.example.org/api/hooks/github/3' } },
      'http://localhost:5173')).toBe('https://public.example.org/api/hooks/github/3')
  })
})

describe('GitHubHookPanel secret', () => {
  it('shows a newly generated secret once, and never again after "I\'ve copied it"', async () => {
    api.getGitHubHook.mockResolvedValue(off)
    api.rotateGitHubHookSecret.mockResolvedValue({ ...on, secret: FAKE_SECRET })
    const user = userEvent.setup()
    renderPanel()

    await user.click(await screen.findByRole('button', { name: /generate secret/i }))
    expect(api.rotateGitHubHookSecret).toHaveBeenCalledWith(3)
    expect(await screen.findByText(FAKE_SECRET)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^copy$/i })).toBeInTheDocument()
    // The setup steps carry the full payload URL, built from this page's origin.
    expect(screen.getAllByText(`${window.location.origin}/api/hooks/github/3`).length).toBeGreaterThan(0)

    await user.click(screen.getByRole('button', { name: /i've copied it/i }))
    expect(screen.queryByText(FAKE_SECRET)).not.toBeInTheDocument()
    // Still configured, and nothing on the page can bring it back.
    expect(screen.getByRole('button', { name: /rotate secret/i })).toBeInTheDocument()
    expect(document.body.textContent).not.toContain(FAKE_SECRET)
  })

  it('does not show a secret when the page loads (the API never returns it)', async () => {
    api.getGitHubHook.mockResolvedValue(on)
    renderPanel()
    expect(await screen.findByRole('button', { name: /rotate secret/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /i've copied it/i })).not.toBeInTheDocument()
  })

  it('rotates only after confirming, and shows the new secret once', async () => {
    api.getGitHubHook.mockResolvedValue(on)
    api.rotateGitHubHookSecret.mockResolvedValue({ ...on, secret: FAKE_SECRET })
    const user = userEvent.setup()
    renderPanel()
    await user.click(await screen.findByRole('button', { name: /rotate secret/i }))
    expect(api.rotateGitHubHookSecret).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: /rotate: github needs the new one/i }))
    expect(await screen.findByText(FAKE_SECRET)).toBeInTheDocument()
  })

  it('turns the webhook off after confirming', async () => {
    api.getGitHubHook.mockResolvedValueOnce(on).mockResolvedValueOnce(off)
    api.deleteGitHubHook.mockResolvedValue({ deleted: true })
    const user = userEvent.setup()
    renderPanel()
    await user.click(await screen.findByRole('button', { name: /turn off/i }))
    await user.click(screen.getByRole('button', { name: /turn it off/i }))
    expect(api.deleteGitHubHook).toHaveBeenCalledWith(3)
    expect(await screen.findByRole('button', { name: /generate secret/i })).toBeInTheDocument()
  })
})

describe('GitHubHookPanel deliveries', () => {
  it('lists recent deliveries with links to the tickets they changed, and refused requests', async () => {
    api.getGitHubHook.mockResolvedValue({
      ...on,
      hook: { ...on.hook!, rejected_count: 2, last_rejected_at: '2026-10-27T11:00:00Z' },
      deliveries: [
        { delivery_id: 'd1', event: 'pull_request', action: 'closed', status: 200, result: 'merged PR #42', task_ids: [17, 18], received_at: '2026-10-27T12:00:00Z' },
        { delivery_id: 'd0', event: 'ping', status: 200, result: 'pong', task_ids: null, received_at: '2026-10-27T11:30:00Z' },
      ],
    })
    renderPanel()
    expect(await screen.findByText('merged PR #42', { exact: false })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '#17' })).toHaveAttribute('href', '/project/3/board?focus=17')
    expect(screen.getByText('pong')).toBeInTheDocument()
    expect(screen.getByText(/2 requests were refused/)).toBeInTheDocument()
  })

  it('says when there are no deliveries yet', async () => {
    api.getGitHubHook.mockResolvedValue(on)
    renderPanel()
    await waitFor(() => expect(screen.getByText(/no deliveries yet/i)).toBeInTheDocument())
  })
})
