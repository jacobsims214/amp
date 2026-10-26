import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { StoppedStreamWarning, WebhookForm } from './WebhooksPanel'
import type { Webhook } from '../types'

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
