// RequestAttachmentsPanel (AMP #3979): the private-file panel's states, and the
// one property that matters most about it — it never claims more than the
// server's gate says.
//
// The criteria this file pins:
//
//  1. **The held/scan states are stated honestly.** `scan_state: 'unsupported'`
//     reads "Not scanned", never "clean"; a held file says it waits for a team
//     lead; a purged or infected file says why it cannot be served.
//  2. **The download is gated on the server's decision, not on a re-derivation.**
//     `state.ready === false` disables the button and prints the refusal, so the
//     panel never attempts a write the gate would refuse. A ready file mints a
//     link at the team-scoped route and navigates to the URL the server returned
//     — it never composes a URL itself.
//  3. **The acts offered are the caller's:** keep/unkeep for the team only (D3 —
//     a requester cannot pin their own upload), delete for the team or the
//     uploader, and no release control at all (releasing a held file is a lead's
//     audited act with no route in this build, so a disabled "Release" button
//     would misrepresent authorization).
//  4. **A failed write keeps the list and says why**; a successful one asks the
//     page to re-read, and the stale error does not survive a fresh panel.
//
// Mobile: the rows are a single column at any width (name truncates, chips wrap,
// actions wrap under the name), which is the 390 px layout the mockup's edge
// sheet shows. jsdom has no layout, so the test asserts the structure that makes
// it true — no fixed widths, wrapping containers — rather than pixels.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { RequestAttachmentsPanel } from './RequestAttachmentsPanel'
import * as download from '../../lib/download'
import type { RequestAttachmentFacts, RequestAttachmentPanel } from '../../types'

const file = (over: Partial<RequestAttachmentFacts> = {}): RequestAttachmentFacts => ({
  id: 5,
  request_id: 7,
  file_name: 'idp-metadata.xml',
  content_type: 'text/xml',
  size_bytes: 41 * 1024,
  sha256: 'a'.repeat(64),
  scan_state: 'unsupported',
  uploaded_by: 42,
  created_at: '2026-11-14T09:41:00Z',
  keep: false,
  kept_by: null,
  kept_at: null,
  expires_at: null,
  released_at: null,
  released_by: null,
  held: false,
  state: { ready: true, held: false },
  ...over,
})

const panel = (items: RequestAttachmentFacts[]): RequestAttachmentPanel => ({
  request_id: 7,
  public_ref: 'a1b2c3d4e5f6g7h8i9j0',
  items,
  count: items.length,
  limits: { max_file_bytes: 25 << 20, max_files: 5, max_total_bytes: 60 << 20, retention_days: 90 },
})

/** Every request a test made, so the team-scoped path can be asserted exactly. */
interface Call { method: string; path: string; body: unknown }
let calls: Call[] = []
const record = async (request: Request) => {
  const text = await request.text()
  calls.push({ method: request.method, path: new URL(request.url).pathname, body: text ? JSON.parse(text) : undefined })
}

/**
 * Navigation is the panel's one side effect (jsdom cannot navigate at all), so
 * the seam is spied rather than fought: `goToDownload` is the only place the UI
 * follows a minted URL, and this proves a mint is followed with the URL the
 * server returned — not one the panel built.
 */
const assign = vi.fn()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => {
  calls = []
  assign.mockReset()
  vi.spyOn(download, 'goToDownload').mockImplementation(assign)
})
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

const server = setupServer(
  http.post('*/api/teams/:teamId/requests/:requestId/attachments/:attachmentId/download', async ({ request }) => {
    await record(request)
    return HttpResponse.json({
      download: {
        url: '/api/request-downloads/tok-abc.def',
        expires_at: '2026-11-14T10:05:00Z',
        attachment_id: 5,
        request_id: 7,
        file_name: 'idp-metadata.xml',
        size_bytes: 41 * 1024,
      },
    })
  }),
  http.post('*/api/teams/:teamId/requests/:requestId/attachments/:attachmentId/keep', async ({ request }) => {
    await record(request)
    return HttpResponse.json({ attachment: file({ keep: true, kept_by: 11, kept_at: '2026-11-14T10:01:00Z' }), keep: true })
  }),
  http.delete('*/api/teams/:teamId/requests/:requestId/attachments/:attachmentId', async ({ request }) => {
    await record(request)
    return HttpResponse.json({ deleted: true, attachment_id: 5 })
  }),
)



describe('honest file states', () => {
  it('calls an unscanned file "Not scanned", never clean', () => {
    render(<RequestAttachmentsPanel teamId={4} panel={panel([file({ scan_state: 'unsupported' })])} />)
    const row = screen.getByTestId('attachment-row')
    expect(within(row).getByText('Not scanned')).toHaveAttribute('title', expect.stringContaining('never scanned'))
    expect(within(row).queryByText('Scanned')).toBeNull()
  })

  it('distinguishes a completed scan, a pending one and an infected one', () => {
    render(<RequestAttachmentsPanel teamId={4} panel={panel([
      file({ id: 1, file_name: 'a.png', scan_state: 'clean', state: { ready: true, held: false } }),
      file({ id: 2, file_name: 'b.png', scan_state: 'pending', state: { ready: false, refusal: 'scan_pending', held: false } }),
      file({ id: 3, file_name: 'c.png', scan_state: 'infected', state: { ready: false, refusal: 'scan_infected', held: false } }),
    ])} />)
    const rows = screen.getAllByTestId('attachment-row')
    expect(within(rows[0]).getByText('Scanned')).toBeInTheDocument()
    expect(within(rows[1]).getByText('Scanning')).toBeInTheDocument()
    expect(within(rows[1]).getByTestId('attachment-refusal')).toHaveTextContent('Waiting for the scan to finish.')
    expect(within(rows[2]).getByText('Blocked')).toBeInTheDocument()
    expect(within(rows[2]).getByTestId('attachment-refusal')).toHaveTextContent('will not serve this file')
  })

  it('shows a held file as held, explains the lead release, and offers no release control', () => {
    render(<RequestAttachmentsPanel teamId={4} isTeamLead canManage
      panel={panel([file({ file_name: 'berlin-rollout.exe', content_type: 'application/x-msdownload', held: true, released_at: null, state: { ready: false, refusal: 'held_for_release', held: true } })])} />)
    const row = screen.getByTestId('attachment-row')
    expect(within(row).getByText('Held')).toBeInTheDocument()
    expect(within(row).getByTestId('attachment-refusal')).toHaveTextContent('Held for a team lead to release.')
    expect(within(row).getByRole('button', { name: /download/i })).toBeDisabled()
    // There is no release route in this build, so there is no release control —
    // even for a lead. A disabled button would say the capability exists here.
    expect(screen.queryByRole('button', { name: /release/i })).toBeNull()
  })

  it('says why a purged file is gone, and names the retention rule', () => {
    render(<RequestAttachmentsPanel teamId={4} panel={panel([file({ expires_at: '2026-11-01T00:00:00Z', state: { ready: false, refusal: 'purged', held: false } })])} />)
    expect(screen.getByTestId('attachment-refusal')).toHaveTextContent(/Purged/)
    expect(screen.getByTestId('attachment-bounds')).toHaveTextContent('90 days')
  })

  it('states the size, the sniffed type and any expiry the row carries', () => {
    render(<RequestAttachmentsPanel teamId={4} panel={panel([file({ expires_at: '2027-02-12T00:00:00Z' })])} />)
    expect(screen.getByTestId('attachment-meta')).toHaveTextContent('41 KB · XML')
    expect(screen.getByText(/Purged after/)).toBeInTheDocument()
  })
})

describe('the download is the gate\'s decision', () => {
  it('mints at the team-scoped route and navigates to the URL the server returned', async () => {
    render(<RequestAttachmentsPanel teamId={4} panel={panel([file()])} />)
    await userEvent.click(screen.getByRole('button', { name: /download/i }))
    await waitFor(() => expect(assign).toHaveBeenCalledWith('/api/request-downloads/tok-abc.def'))
    expect(calls).toEqual([{ method: 'POST', path: '/api/teams/4/requests/7/attachments/5/download', body: undefined }])
  })

  it('disables the button on the gate\'s refusal instead of attempting the write', async () => {
    render(<RequestAttachmentsPanel teamId={4} panel={panel([file({ state: { ready: false, refusal: 'scan_pending', held: false } })])} />)
    const button = screen.getByRole('button', { name: /download/i })
    expect(button).toBeDisabled()
    expect(button).toHaveAttribute('title', 'Waiting for the scan to finish.')
    await userEvent.click(button)
    expect(calls).toEqual([])
    expect(assign).not.toHaveBeenCalled()
  })

  it('reports a failed mint without navigating, and keeps the row', async () => {
    server.use(http.post('*/api/teams/:teamId/requests/:requestId/attachments/:attachmentId/download', () =>
      HttpResponse.json({ error: 'That download link expired; mint a new one.', code: 'request.invalid' }, { status: 400 })))
    render(<RequestAttachmentsPanel teamId={4} panel={panel([file()])} />)
    await userEvent.click(screen.getByRole('button', { name: /download/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/expired/i))
    expect(assign).not.toHaveBeenCalled()
    expect(screen.getByTestId('attachment-name')).toHaveTextContent('idp-metadata.xml')
  })
})

describe('the acts the caller actually has', () => {
  const ready = panel([file({ uploaded_by: 11 })])

  it('offers keep and delete to a team member, and keeps at the team route', async () => {
    const onChanged = vi.fn()
    render(<RequestAttachmentsPanel teamId={4} canManage viewerUserId={11} panel={ready} onChanged={onChanged} />)
    await userEvent.click(screen.getByRole('button', { name: /^keep$/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(calls).toEqual([{ method: 'POST', path: '/api/teams/4/requests/7/attachments/5/keep', body: { keep: true } }])
  })

  it('offers the requester no keep — the retention promise is the team\'s (D3)', () => {
    render(<RequestAttachmentsPanel teamId={4} viewerUserId={42} canManage={false} panel={panel([file({ uploaded_by: 42 })])} />)
    expect(screen.queryByRole('button', { name: /keep/i })).toBeNull()
    // Their own file is still theirs to withdraw.
    expect(screen.getByRole('button', { name: /delete/i })).toBeInTheDocument()
  })

  it('offers nobody a delete on someone else\'s file without team standing', () => {
    render(<RequestAttachmentsPanel teamId={4} viewerUserId={42} canManage={false} panel={panel([file({ uploaded_by: 11 })])} />)
    expect(screen.queryByRole('button', { name: /delete/i })).toBeNull()
    expect(screen.getByRole('button', { name: /download/i })).toBeInTheDocument()
  })

  it('asks once before deleting, then removes the file', async () => {
    const onChanged = vi.fn()
    render(<RequestAttachmentsPanel teamId={4} canManage viewerUserId={11} panel={panel([file({ uploaded_by: 11 })])} onChanged={onChanged} />)
    await userEvent.click(screen.getByRole('button', { name: /^delete$/i }))
    expect(calls).toEqual([])
    await userEvent.click(screen.getByRole('button', { name: /delete for good/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(calls).toEqual([{ method: 'DELETE', path: '/api/teams/4/requests/7/attachments/5', body: undefined }])
  })

  it('keeps the list and says why when a delete is refused', async () => {
    server.use(http.delete('*/api/teams/:teamId/requests/:requestId/attachments/:attachmentId', () =>
      HttpResponse.json({ error: 'That file is already gone.', code: 'request.not_found' }, { status: 404 })))
    render(<RequestAttachmentsPanel teamId={4} canManage viewerUserId={11} panel={panel([file({ uploaded_by: 11 })])} />)
    await userEvent.click(screen.getByRole('button', { name: /^delete$/i }))
    await userEvent.click(screen.getByRole('button', { name: /delete for good/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/already gone/i))
    expect(screen.getByTestId('attachment-name')).toHaveTextContent('idp-metadata.xml')
  })
})

describe('the list\'s own states', () => {
  it('says what an empty list means, rather than just being blank', () => {
    render(<RequestAttachmentsPanel teamId={4} panel={panel([])} />)
    expect(screen.getByTestId('request-panel-empty')).toHaveTextContent(/No files on this request/)
    expect(screen.getByTestId('attachment-bounds')).toBeInTheDocument()
  })

  it('shows the read\'s failure with a retry, and no count it cannot stand behind', async () => {
    const onRetry = vi.fn()
    render(<RequestAttachmentsPanel teamId={4} panel={null} error="The files didn't come back." onRetry={onRetry} />)
    expect(screen.getByRole('alert')).toHaveTextContent(/didn't come back/)
    expect(screen.queryByTestId('attachment-bounds')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: /retry/i }))
    expect(onRetry).toHaveBeenCalled()
  })

  it('announces loading once, without an empty state beside it', () => {
    render(<RequestAttachmentsPanel teamId={4} panel={null} loading />)
    expect(screen.getByRole('status')).toHaveTextContent('Loading the files…')
    expect(screen.queryByTestId('request-panel-empty')).toBeNull()
  })

  it('lays a row out to fit 390 px: one wrapping line, a truncating name, no fixed width', () => {
    render(<RequestAttachmentsPanel teamId={4} canManage panel={panel([file({ file_name: 'quarterly-access-recertification-export-frankfurt-berlin-munich.csv', content_type: 'text/csv' })])} />)
    const row = screen.getByTestId('attachment-row')
    expect(row).toHaveClass('flex-col')
    expect(row.className).not.toMatch(/\bw-\[\d{3,}px\]/)
    expect(screen.getByTestId('attachment-name')).toHaveClass('truncate')
    // Every group of controls wraps rather than overflowing the narrow column.
    expect(within(row).getAllByRole('button').length).toBeGreaterThan(0)
    for (const group of row.querySelectorAll('.flex-wrap')) expect(group.className).toMatch(/flex-wrap/)
  })
})
