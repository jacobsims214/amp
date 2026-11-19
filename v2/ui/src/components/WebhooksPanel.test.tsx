import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { StoppedStreamWarning, WebhookForm, WebhooksPanel } from './WebhooksPanel'
import type { Webhook, WebhookDelivery } from '../types'

const noop = async () => {}

describe('WebhookForm request context (#2439)', () => {
  it('offers the opt-in for audit webhooks, off unless the webhook has it', () => {
    const html = renderToStaticMarkup(<WebhookForm initial={{ format: 'json', events: ['audit.*'] }} submitLabel="Add" onSubmit={noop} />)
    expect(html).toContain('Include request context')
    expect(html).not.toMatch(/type="checkbox"[^>]*checked/)
    const on = renderToStaticMarkup(<WebhookForm initial={{ format: 'json', events: ['audit.*'], include_context: true }} submitLabel="Save" onSubmit={noop} />)
    expect(on).toMatch(/type="checkbox"[^>]*checked/)
  })
  it('defaults to on for an instance webhook, and is not offered without audit events', () => {
    expect(renderToStaticMarkup(<WebhookForm initial={{ format: 'json', events: ['audit.*'] }} instance submitLabel="Add" onSubmit={noop} />))
      .toMatch(/type="checkbox"[^>]*checked/)
    expect(renderToStaticMarkup(<WebhookForm initial={{ format: 'json', events: ['task.completed'] }} submitLabel="Add" onSubmit={noop} />))
      .not.toContain('Include request context')
  })
})

describe('StoppedStreamWarning (#2439)', () => {
  const hook = (over: Partial<Webhook>): Webhook => ({ id: 7, name: 'Splunk', url: 'https://hec.example', format: 'json', events: ['audit.*'], active: true,
    instance: true, created_at: '', updated_at: '', consecutive_failures: 0, ...over })
  it('warns that the SIEM stream stopped, naming the webhook and why', () => {
    const html = renderToStaticMarkup(<StoppedStreamWarning siem hooks={[hook({ consecutive_failures: 4, last_error: 'HTTP 503' })]} />)
    expect(html).toContain('SIEM stream stopped')
    expect(html).toContain('Splunk: 4 deliveries in a row failed: HTTP 503')
    expect(html).toContain('role="alert"')
  })
  it('says nothing while every stream is healthy', () => {
    expect(renderToStaticMarkup(<StoppedStreamWarning siem hooks={[hook({})]} />)).toBe('')
  })
})

describe('Deliveries polling (#2517)', () => {
  const hook: Webhook = { id: 7, name: 'Builds', url: 'https://hooks.example/amp', format: 'json', events: ['task.completed'], active: true,
    created_at: '', updated_at: '', consecutive_failures: 0 }
  const delivery: WebhookDelivery = { id: 1, webhook_id: 7, event_type: 'task.completed', status: 'delivered', attempts: 1,
    next_attempt_at: '', created_at: '2026-11-18T12:00:00Z' }
  const deliveriesPath = '/api/webhooks/7/deliveries'
  const server = setupServer(
    http.get('*/api/projects/5/webhooks', () => HttpResponse.json({ webhooks: [hook] })),
    http.get(`*${deliveriesPath}`, () => HttpResponse.json({ deliveries: [delivery] })),
  )
  beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
  afterAll(() => server.close())

  const realSetTimeout = globalThis.setTimeout
  let visibility: DocumentVisibilityState = 'visible'
  let fetches = 0
  const countDeliveryFetches = ({ request }: { request: Request }) => {
    if (new URL(request.url).pathname === deliveriesPath) fetches++
  }
  const setVisibility = (state: DocumentVisibilityState) => act(() => {
    visibility = state
    document.dispatchEvent(new Event('visibilitychange'))
  })
  const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms))
  // Give any fetch that was started time to reach the server.
  const settle = () => act(() => new Promise(r => realSetTimeout(r, 50)))

  // Render the panel and expand the webhook's row, which shows its deliveries.
  const openDeliveries = async () => {
    render(<WebhooksPanel projectId={5} canManage />)
    fireEvent.click(await screen.findByRole('button', { name: 'Show deliveries' }))
    await screen.findByText('delivered')
  }

  beforeEach(() => {
    // Fake time still moves with real time, so findBy and waitFor keep working.
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
    visibility = 'visible'
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
    fetches = 0
    server.events.on('request:start', countDeliveryFetches)
  })
  afterEach(() => {
    cleanup()
    server.events.removeListener('request:start', countDeliveryFetches)
    vi.useRealTimers()
    // Drop the override so jsdom's own getter shows through again.
    delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
  })

  it('loads when the row opens, refreshes every 4 s while visible, and stops when it closes', async () => {
    await openDeliveries()
    expect(fetches).toBe(1)

    await advance(4000)
    await waitFor(() => expect(fetches).toBe(2))
    await advance(4000)
    await waitFor(() => expect(fetches).toBe(3))

    fireEvent.click(screen.getByRole('button', { name: 'Show deliveries' }))
    expect(screen.queryByText('delivered')).not.toBeInTheDocument()
    await advance(5 * 4000)
    await settle()
    expect(fetches).toBe(3)
  })

  it('makes no fetch while the tab is hidden, then one when it is shown', async () => {
    await openDeliveries()
    expect(fetches).toBe(1)

    await setVisibility('hidden')
    await advance(5 * 4000)
    await settle()
    expect(fetches).toBe(1)

    // One catch-up fetch, and the next refresh is a full 4 s after it.
    await setVisibility('visible')
    await waitFor(() => expect(fetches).toBe(2))
    await advance(3500)
    await settle()
    expect(fetches).toBe(2)
    await advance(500)
    await waitFor(() => expect(fetches).toBe(3))
  })
})
