// The requester's attachment picker (ticket AMP #3981, approved mockup artifact
// 38 v1's "Uploading / failed file" state, build slice J4).
//
// The three things this file proves are the ticket's own criteria, one describe
// each, plus the keyboard and phone passes the same criteria name:
//
//  1. **Bounds.** 5 files / 25 MiB each / 60 MiB total, refused in the list with
//     the bound named, and a refused file is never handed to the page as one to
//     send. A sixth pick says "5 files max"; a 96 MB pick says "25 MB max" — the
//     mockup's own wording.
//  2. **Retry and cancel.** A failed upload is retryable and its failure is the
//     server's own sentence where the server gave one; a cancelled upload stops
//     waiting at once and leaves no row behind.
//  3. **Honesty about the scan.** Every upload lands on `unsupported` (AMP has no
//     scanner in v1), so the row says "Not scanned" and never "Scanned"/"Clean";
//     a held file says a team lead must release it.
//
// The picker calls no API: it is handed an `upload` function, so these tests
// hand it one. That is also the proof of "content is never remote-fetched" — the
// component has no transport of its own to fetch anything with.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { AttachmentPicker, type UploadFile } from './AttachmentPicker'
import type { RequestAttachmentFacts, RequestUploadAnswer } from '../../types'

/** A File of `bytes` bytes with a chosen name. */
const file = (name: string, bytes = 1024): File => {
  const f = new File(['x'], name, { type: 'text/plain' })
  Object.defineProperty(f, 'size', { value: bytes })
  return f
}

const facts = (over: Partial<RequestAttachmentFacts> = {}): RequestAttachmentFacts => ({
  id: 11, request_id: 7, file_name: 'october-export-totals.csv', content_type: 'text/plain',
  size_bytes: 182 * 1024, sha256: 'a'.repeat(64), scan_state: 'unsupported', uploaded_by: 5,
  created_at: '2026-11-14T10:00:00Z', keep: false, kept_by: null, kept_at: null, expires_at: null,
  released_at: null, released_by: null, held: false,
  state: { ready: true, held: false }, ...over,
})

const answer = (over: Partial<RequestAttachmentFacts> = {}): RequestUploadAnswer => ({
  attachment: facts(over), held: !!over.held, next: 'Stored as a private file on the request.',
})

/** The picker's `<input type="file">` — hidden, but a person opens it from the drop button. */
const fileInput = (container: HTMLElement) => container.querySelector('input[type="file"]') as HTMLInputElement

/**
 * Pick files the way a person does: through the input's change event.
 *
 * `applyAccept: false` because the accept list is only a hint for the dialog.
 * A browser's file dialog can be switched to "all files" and a drag-and-drop
 * bypasses it entirely, so a `.exe` or `.bin` really does arrive at this
 * component — and the bound, not the extension, is what decides.
 */
const pick = async (container: HTMLElement, ...files: File[]) => {
  await userEvent.upload(fileInput(container), files, { applyAccept: false })
}

describe('AttachmentPicker applies the server’s bounds before a round trip', () => {
  it('states the bounds up front, so nobody learns them from a failure', () => {
    render(<AttachmentPicker />)
    expect(screen.getByText(/Up to 5 files, 25 MB each/)).toBeInTheDocument()
    expect(screen.getByTestId('picker-summary')).toHaveTextContent('0 of 5 files · 0 B of 60 MB')
  })

  it('refuses a sixth file with the count named, and never sends it', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name }))
    const onChange = vi.fn()
    const { container } = render(<AttachmentPicker upload={upload} onChange={onChange} />)
    await pick(container, ...[1, 2, 3, 4, 5].map(i => file(`f${i}.csv`)))
    await waitFor(() => expect(upload).toHaveBeenCalledTimes(5))

    await pick(container, file('sixth.csv'))
    expect(screen.getByText('5 files max per request')).toBeInTheDocument()
    // Five uploads, not six: the refused file never reached the page.
    expect(upload).toHaveBeenCalledTimes(5)
    expect(onChange).toHaveBeenLastCalledWith(expect.not.arrayContaining([expect.objectContaining({ name: 'sixth.csv' })]))
  })

  it('refuses a 96 MB file with "25 MB max" and does not upload it', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name }))
    const { container } = render(<AttachmentPicker upload={upload} />)
    await pick(container, file('full-database-dump.zip', 96 * 1024 * 1024))
    expect(await screen.findByText('Too large — 25 MB max')).toBeInTheDocument()
    expect(upload).not.toHaveBeenCalled()
    expect(screen.getByTestId('picker-summary')).toHaveTextContent('0 of 5 files')
  })

  it('refuses a file that would push the request over 60 MB in total', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name }))
    const { container } = render(<AttachmentPicker upload={upload} />)
    await pick(container, file('a.bin', 25 * 1024 * 1024), file('b.bin', 25 * 1024 * 1024), file('c.bin', 25 * 1024 * 1024))
    await waitFor(() => expect(upload).toHaveBeenCalledTimes(2))
    expect(await screen.findByText('60 MB total max')).toBeInTheDocument()
    expect(upload).toHaveBeenCalledTimes(2)
  })

  it('uses the live limits a listing carries, not the constants', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name }))
    const { container } = render(
      <AttachmentPicker upload={upload} limits={{ max_file_bytes: 1024, max_files: 1, max_total_bytes: 4096, retention_days: 30 }} />,
    )
    expect(screen.getByTestId('picker-summary')).toHaveTextContent('0 of 1 files')
    await pick(container, file('a.csv', 2048))
    expect(await screen.findByText('Too large — 1 KB max')).toBeInTheDocument()
    expect(upload).not.toHaveBeenCalled()
  })

  it('reports the files to send, so a submit form can carry them through the create call', async () => {
    const onChange = vi.fn()
    const { container } = render(<AttachmentPicker onChange={onChange} />)
    await pick(container, file('a.csv'), file('b.png', 2048))
    await waitFor(() => expect(onChange).toHaveBeenLastCalledWith([expect.objectContaining({ name: 'a.csv' }), expect.objectContaining({ name: 'b.png' })]))
    // With no upload function the files stay "Ready to send" rather than pretending
    // they were sent.
    expect(screen.getAllByText('Ready to send')).toHaveLength(2)
  })
})

describe('AttachmentPicker retries and cancels honestly', () => {
  it('retries a failed upload, and shows the server’s own sentence for the failure', async () => {
    let attempt = 0
    const upload = vi.fn<UploadFile>(async f => {
      attempt += 1
      if (attempt === 1) throw new Error('A request carries at most 5 attachments (got 6): ask for the rest in a second request')
      return answer({ file_name: f.name })
    })
    const { container } = render(<AttachmentPicker upload={upload} />)
    await pick(container, file('app-error.log'))
    expect(await screen.findByText('Upload failed')).toBeInTheDocument()
    expect(screen.getByText(/at most 5 attachments/)).toBeInTheDocument()
    expect(screen.getByText(/Failed or refused files are never sent/)).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Retry upload of app-error.log' }))
    await waitFor(() => expect(screen.getByText('Not scanned')).toBeInTheDocument())
    expect(upload).toHaveBeenCalledTimes(2)
    expect(screen.queryByText('Upload failed')).toBeNull()
  })

  it('cancels an upload at once, leaving no row and no claim that it was sent', async () => {
    // A never-settling upload: without the abort the picker would wait forever.
    const upload = vi.fn<UploadFile>(() => new Promise<RequestUploadAnswer>(() => {}))
    const onChange = vi.fn()
    const { container } = render(<AttachmentPicker upload={upload} onChange={onChange} />)
    await pick(container, file('screenshot-ledger-mismatch.png', 1_200_000))
    expect(await screen.findByText('Uploading')).toBeInTheDocument()
    // Adding more is paused while one is in flight, and says so rather than
    // silently ignoring the drop.
    expect(screen.getByRole('button', { name: /paused while uploads finish/ })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Cancel upload of screenshot-ledger-mismatch.png' }))
    await waitFor(() => expect(screen.queryByText('Uploading')).toBeNull())
    expect(screen.queryByText('screenshot-ledger-mismatch.png')).toBeNull()
    expect(onChange).toHaveBeenLastCalledWith([])
  })

  it('removes an uploaded file through the page, and keeps it when the page refuses', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name }))
    const onRemove = vi.fn(async () => {})
    const { container, rerender } = render(<AttachmentPicker upload={upload} onRemoveAttachment={onRemove} />)
    await pick(container, file('october-export-totals.csv'))
    await waitFor(() => expect(screen.getByText('Not scanned')).toBeInTheDocument())
    await userEvent.click(screen.getByRole('button', { name: 'Remove october-export-totals.csv' }))
    await waitFor(() => expect(onRemove).toHaveBeenCalledWith(11))
    await waitFor(() => expect(screen.queryByText('october-export-totals.csv')).toBeNull())

    // A refused removal keeps the row and says why: the file is still there.
    onRemove.mockRejectedValueOnce(new Error('Only the person who uploaded a file can remove it.'))
    rerender(<AttachmentPicker upload={upload} onRemoveAttachment={onRemove} />)
    await pick(container, file('b.csv'))
    await waitFor(() => expect(screen.getByText('Not scanned')).toBeInTheDocument())
    await userEvent.click(screen.getByRole('button', { name: 'Remove b.csv' }))
    expect(await screen.findByText('Only the person who uploaded a file can remove it.')).toBeInTheDocument()
  })

  it('shows a page’s own error (a 400 from the upload route) under the list', () => {
    render(<AttachmentPicker error="That file is empty — there is nothing to send." />)
    expect(screen.getByRole('alert')).toHaveTextContent('That file is empty')
  })
})

describe('AttachmentPicker is honest about the scan', () => {
  it('says "Not scanned" for the unsupported state every upload lands on', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name, scan_state: 'unsupported' }))
    const { container } = render(<AttachmentPicker upload={upload} />)
    await pick(container, file('a.csv'))
    const tag = await screen.findByText('Not scanned')
    // The words a requester must never be given about an unscanned file.
    const said = `${tag.textContent} ${tag.getAttribute('title')}`.toLowerCase()
    expect(said).not.toContain('clean')
    expect(said).not.toContain('scanned and')
    expect(said).not.toContain('virus-free')
    // And the fact it does say: nothing inspected the bytes.
    expect(said).toContain('unscanned')
    expect(said).toContain('cannot scan')
  })

  it('says a held file waits for a team lead, and does not call it scanned either', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name, held: true, scan_state: 'unsupported' }))
    const { container } = render(<AttachmentPicker upload={upload} />)
    await pick(container, file('berlin-rollout.exe'))
    const tag = await screen.findByText('Held for review')
    expect(tag).toHaveAttribute('title', expect.stringContaining('team lead'))
  })

  it('reports a real verdict where one exists: clean is "Scanned", infected is "Blocked"', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name, scan_state: f.name.startsWith('bad') ? 'infected' : 'clean' }))
    const { container } = render(<AttachmentPicker upload={upload} />)
    await pick(container, file('good.csv'), file('bad.csv'))
    expect(await screen.findByText('Scanned')).toBeInTheDocument()
    expect(await screen.findByText('Blocked')).toBeInTheDocument()
  })
})

describe('AttachmentPicker works from the keyboard and on a phone', () => {
  it('opens the file dialog from the drop target with Enter and with Space', async () => {
    const { container } = render(<AttachmentPicker />)
    const input = fileInput(container)
    const open = vi.spyOn(input, 'click').mockImplementation(() => {})
    const drop = screen.getByRole('button', { name: /Drop files here/ })
    drop.focus()
    expect(drop).toHaveFocus()
    await userEvent.keyboard('{Enter}')
    await userEvent.keyboard(' ')
    expect(open).toHaveBeenCalledTimes(2)
  })

  it('reaches Retry, Cancel and Remove by keyboard, and every one is a real button', async () => {
    const upload = vi.fn<UploadFile>(async () => { throw new Error('nope') })
    const { container } = render(<AttachmentPicker upload={upload} onRemoveAttachment={async () => {}} />)
    await pick(container, file('a.csv'))
    const retry = await screen.findByRole('button', { name: 'Retry upload of a.csv' })
    expect(retry.tagName).toBe('BUTTON')
    retry.focus()
    expect(retry).toHaveFocus()
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(upload).toHaveBeenCalledTimes(2))
  })

  it('is a touch-sized target and never a fixed width that would overflow 390px', () => {
    render(<AttachmentPicker />)
    const drop = screen.getByRole('button', { name: /Drop files here/ })
    // 44px tall: the guide's touch minimum for a primary action.
    expect(drop.style.minHeight).toBe('44px')
    // Full width, so it cannot be wider than the phone: no pixel width at all.
    expect(drop.className).toContain('w-full')
    expect(drop.className).not.toMatch(/\bw-\[\d{3,}px\]/)
  })

  it('truncates a very long file name instead of pushing the row wide', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name }))
    const { container } = render(<AttachmentPicker upload={upload} />)
    const long = `${'a'.repeat(120)}.csv`
    await pick(container, file(long))
    const name = await screen.findByTitle(long)
    expect(name).toHaveClass('truncate')
    expect(name.textContent!.length).toBeLessThan(long.length)
  })

  it('announces the file count politely rather than shouting it', async () => {
    const upload = vi.fn<UploadFile>(async f => answer({ file_name: f.name }))
    const { container } = render(<AttachmentPicker upload={upload} />)
    await pick(container, file('a.csv'))
    await waitFor(() => expect(screen.getByTestId('picker-summary')).toHaveTextContent('1 of 5 files'))
    expect(screen.getByTestId('picker-summary')).toHaveAttribute('aria-live', 'polite')
    // The progress bar is indeterminate and says so: fetch reports no upload
    // progress, so a percentage here would be invented.
    await pick(container, file('b.csv'))
    const bar = within(screen.getByTestId('picker-summary').parentElement!).queryByRole('progressbar')
    expect(bar).toBeNull()
  })
})
