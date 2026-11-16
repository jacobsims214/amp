import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { AlertTriangle, FileText, Image as ImageIcon, Loader2, Paperclip } from 'lucide-react'
import type {
  RequestAttachmentFacts, RequestAttachmentLimits, RequestUploadAnswer,
} from '../../types'
import { errMsg } from '../../lib/adminStyles'
import {
  REQUEST_UPLOAD_ACCEPT, REQUEST_UPLOAD_LIMITS, fileNameForDisplay, formatBytes,
  pickRefusalMessage, planPicks, scanWording, type PickRefusal, type UploadFile,
} from '../../lib/requesterUpload'
// The requester's file picker (approved mockup artifact 38 v1, "Attachments"
// section of the submit form; build slice J4 of
// discoveries/team-inbox-approved-build-slices.md, ticket AMP #3981).
//
// What it is: the one place a requester adds private files to a request. It is
// shared rather than a page's private part, because the submit form and the
// request status screen both show it and both must apply the same bounds and
// say the same words about a scan.
//
// Three rules this component keeps, each with a test:
//
//  1. **The bounds are the server's, and they are applied before the round
//     trip.** 5 files, 25 MiB each, 60 MiB total (`domain/request_attachment.go`;
//     `limits` carries the live values from a listing when a page has one).
//     A sixth file, or a 96 MB one, is refused *in the list* with the bound
//     named — and refused rows are never sent and never count, so a person is
//     never told to remove a file that was never attached.
//  2. **Bytes are read locally, never fetched.** This component calls no API of
//     its own: the page hands it an `upload` function, and the body that
//     function sends is built by `fileToUploadInput` (lib/requesterUpload.ts),
//     which reads the picked File with a FileReader. There is no URL, path or
//     location in that body — and the upload route refuses a location-shaped
//     field by name — so "content is never remote-fetched" is a property of the
//     code rather than a promise in a comment.
//  3. **An unscanned file is never called clean.** Every upload lands on
//     `unsupported` (AMP ships no scanner in v1), so the row says "Not scanned"
//     — never "Scanned" or "Clean" — and a held file says a team lead must
//     release it. `scanWording` is the only place those words exist.
//
// A cancelled or failed upload is not part of the request: the mockup's hint
// says so ("Failed or refused files are never sent") and the counting rule
// agrees with it.

/** One file the picker is tracking. */
type RowStatus = 'queued' | 'uploading' | 'attached' | 'failed' | 'refused'

interface Row {
  key: string
  file: File
  status: RowStatus
  /** Why a refused row was refused; only for `refused`. */
  reason?: PickRefusal
  /**
   * The bound that refused it, as a sentence, captured at pick time. It is
   * stored rather than recomputed because the bound that applied is the one the
   * person must act on: with a listing's live limits, re-deriving it later from
   * the default constants would name the wrong number.
   */
  refusal?: string
  /** The server's answer for an attached row — the facts, never the bytes. */
  answer?: RequestUploadAnswer
  /** What went wrong for a failed row, in the server's own words where it gave them. */
  error?: string
}

/**
 * Upload one file. The picker awaits it, so the page can do the call its own way
 * (an attachment route on an existing request) without the picker knowing a path.
 * `signal` aborts the upload when the person cancels — `abortableUpload` in
 * lib/requesterUpload.ts binds a page's call to this shape.
 */
export type { UploadFile }

export interface AttachmentPickerProps {
  /**
   * Upload one file onto the request. Omit it before the request exists: the
   * picker then holds the files and reports them through `onChange`, and the
   * page uploads them once it has an id (the submit form's order).
   */
  upload?: UploadFile
  /** The files the picker holds that will be, or were, sent — in pick order. */
  onChange?: (files: File[]) => void
  /** Remove an already-uploaded file. Omit it and an attached row shows no Remove. */
  onRemoveAttachment?: (attachmentId: number) => void | Promise<void>
  /** AMP's live bounds from a listing; the domain's constants when a page has none. */
  limits?: RequestAttachmentLimits
  /** The whole picker is unavailable (the form is submitting, say). */
  disabled?: boolean
  /** Said above the drop area; the mockup's own sentence by default. */
  hint?: string
  /** An error the page wants shown under the list (a 400 from the upload route). */
  error?: string | null
}

const isSent = (r: Row) => r.status === 'queued' || r.status === 'uploading' || r.status === 'attached'

/** The tag a row shows, and its tone. One function so no row invents a word. */
function rowTag(row: Row): { label: string; hint: string; tone: 'ok' | 'wait' | 'flag' | 'quiet' } {
  switch (row.status) {
    case 'refused':
      return { label: row.refusal ?? pickRefusalMessage(row.reason ?? 'too_large'), hint: 'This file is not sent. Remove it, or pick one within the bound.', tone: 'flag' }
    case 'failed':
      return { label: 'Upload failed', hint: row.error ?? 'The upload did not finish. Retry it, or remove it — the rest of your request is unaffected.', tone: 'flag' }
    case 'uploading':
      return { label: 'Uploading', hint: 'Sending the file to the desk now.', tone: 'wait' }
    case 'attached': {
      const facts = row.answer?.attachment
      const w = scanWording(facts?.scan_state ?? 'unsupported', !!facts?.held)
      return { label: w.label, hint: w.hint, tone: w.tone }
    }
    case 'queued':
      return { label: 'Ready to send', hint: 'It goes with your request when you send it.', tone: 'quiet' }
  }
}

export function AttachmentPicker({
  upload, onChange, onRemoveAttachment, limits = REQUEST_UPLOAD_LIMITS, disabled = false, hint, error,
}: AttachmentPickerProps) {
  const [rows, setRows] = useState<Row[]>([])
  const inputRef = useRef<HTMLInputElement>(null)
  const nextKey = useRef(0)
  // One abort handle per upload in flight, so Cancel and unmount both stop the
  // request itself rather than only forgetting about it.
  const inflight = useRef(new Map<string, AbortController>())
  const ids = useId()
  // The latest props, read from callbacks that must not re-create themselves on
  // every render (an upload in flight would otherwise see a stale `upload`).
  const uploadRef = useRef(upload)
  const onChangeRef = useRef(onChange)
  const limitsRef = useRef(limits)
  useEffect(() => { uploadRef.current = upload; onChangeRef.current = onChange; limitsRef.current = limits })

  const uploading = rows.some(r => r.status === 'uploading')
  const paused = uploading || disabled

  // The files that will be, or were, sent — reported to the page whenever that
  // set changes, so a submit form can carry them through the create call.
  const sent = rows.filter(isSent).map(r => r.file)
  const sentSignature = rows.filter(isSent).map(r => r.key).join(',')
  useEffect(() => {
    onChangeRef.current?.(rows.filter(isSent).map(r => r.file))
    // `sentSignature` is the set's identity; `rows` itself changes on every
    // progress tick and must not re-notify the page.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sentSignature])

  const patch = useCallback((key: string, next: Partial<Row>) => {
    setRows(prev => prev.map(r => (r.key === key ? { ...r, ...next } : r)))
  }, [])

  const startUpload = useCallback(async (key: string, file: File) => {
    const send = uploadRef.current
    if (!send) return
    const controller = new AbortController()
    inflight.current.set(key, controller)
    patch(key, { status: 'uploading', error: undefined })
    try {
      const answer = await send(file, controller.signal)
      patch(key, { status: 'attached', answer, error: undefined })
    } catch (e) {
      if (controller.signal.aborted) {
        // Cancelled: the file was never on the request, so it leaves the list
        // rather than sitting there in a state nothing can act on.
        setRows(prev => prev.filter(r => r.key !== key))
        return
      }
      patch(key, { status: 'failed', error: errMsg(e, 'The upload did not finish.') })
    } finally {
      inflight.current.delete(key)
    }
  }, [patch])

  // One upload at a time. The server's count and total bounds are properties of
  // the request, and a person who drops six files should see them arrive in
  // order rather than watch five refuse each other.
  useEffect(() => {
    if (!upload || uploading) return
    const next = rows.find(r => r.status === 'queued')
    if (next) void startUpload(next.key, next.file)
  }, [rows, uploading, upload, startUpload])

  // Nothing keeps uploading after the picker goes away.
  const inflightRef = inflight
  useEffect(() => {
    const running = inflightRef.current
    return () => { running.forEach(c => c.abort()) }
  }, [inflightRef])

  const addFiles = (incoming: File[]) => {
    if (paused || incoming.length === 0) return
    const kept = rows.filter(isSent).map(r => r.file)
    const live = limitsRef.current
    const plan = planPicks(kept, incoming, live)
    const added: Row[] = [
      ...plan.accepted.map(file => ({ key: `f${nextKey.current++}`, file, status: 'queued' as RowStatus })),
      ...plan.refused.map(({ file, reason }) => ({
        key: `f${nextKey.current++}`, file, status: 'refused' as RowStatus, reason,
        refusal: pickRefusalMessage(reason, live),
      })),
    ]
    setRows(prev => [...prev, ...added])
  }

  const openPicker = () => { if (!paused) inputRef.current?.click() }

  const retry = (key: string) => {
    const row = rows.find(r => r.key === key)
    if (!row || !uploadRef.current) return
    void startUpload(key, row.file)
  }

  const cancel = (key: string) => {
    // Aborting makes `startUpload` drop the row on its own rejection, so the
    // cancel and the failure path cannot disagree about what happened.
    inflight.current.get(key)?.abort()
    setRows(prev => prev.filter(r => r.key !== key))
  }

  const remove = async (row: Row) => {
    if (row.status === 'attached' && row.answer && onRemoveAttachment) {
      try {
        await onRemoveAttachment(row.answer.attachment.id)
      } catch (e) {
        patch(row.key, { error: errMsg(e, 'Could not remove that file.'), status: 'failed' })
        return
      }
    }
    setRows(prev => prev.filter(r => r.key !== row.key))
  }

  const totalBytes = sent.reduce((n, f) => n + f.size, 0)
  const failed = rows.filter(r => r.status === 'failed').length
  const hintText = hint
    ?? `Screenshots, exports or logs that help explain it. Up to ${limits.max_files} files, ${formatBytes(limits.max_file_bytes)} each.`
  const summary = `${sent.length} of ${limits.max_files} files · ${formatBytes(totalBytes)} of ${formatBytes(limits.max_total_bytes)}`

  return (
    <div className="grid gap-2.5">
      <div>
        <p className="m-0 mb-2 text-[12.5px] leading-relaxed text-ink-2">{hintText}</p>

        {/* The drop target is a real button: Enter and Space open the picker
            without a key handler, and it stays focusable while paused so the
            reason is reachable rather than invisible. */}
        <button
          type="button"
          onClick={openPicker}
          onKeyDown={e => {
            if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openPicker() }
          }}
          onDragOver={e => { if (!paused) { e.preventDefault(); e.dataTransfer.dropEffect = 'copy' } }}
          onDrop={e => {
            e.preventDefault()
            if (paused) return
            addFiles(Array.from(e.dataTransfer.files ?? []))
          }}
          aria-disabled={paused}
          aria-describedby={`${ids}-hint`}
          className="w-full cursor-pointer border border-dashed border-line-str bg-surface-2 px-4 py-5 text-center text-[13px] leading-relaxed text-ink-2 transition-colors hover:border-ink hover:text-ink aria-disabled:cursor-not-allowed aria-disabled:opacity-60 aria-disabled:hover:border-line-str aria-disabled:hover:text-ink-2"
          style={{ borderRadius: 'var(--radius)', minHeight: 44 }}
        >
          {paused ? (
            <span className="inline-flex items-start justify-center gap-2">
              <Loader2 size={13} className="mt-0.5 flex-shrink-0 animate-spin" aria-hidden="true" />
              <span>Adding more files is paused while uploads finish…</span>
            </span>
          ) : (
            <span className="inline-flex flex-wrap items-center justify-center gap-x-1.5">
              <Paperclip size={13} aria-hidden="true" />
              <span>Drop files here, or <span className="font-medium text-link underline">browse</span></span>
              <span className="text-ink-3">— PDF, PNG, CSV, LOG, ZIP</span>
            </span>
          )}
        </button>
        <input
          ref={inputRef}
          id={`${ids}-input`}
          type="file"
          multiple
          accept={REQUEST_UPLOAD_ACCEPT}
          className="hidden"
          tabIndex={-1}
          aria-hidden="true"
          onChange={e => {
            addFiles(Array.from(e.target.files ?? []))
            // Let the same file be picked again after a remove: an unchanged
            // value fires no change event.
            e.target.value = ''
          }}
        />
        <p id={`${ids}-hint`} className="m-0 mt-1.5 text-[11.5px] leading-relaxed text-ink-3">
          Files are private to you and the desk. They are scanned before anyone can open them; AMP cannot scan every
          kind of file, and a file it cannot scan says so.
        </p>
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className="amp-label" aria-live="polite" data-testid="picker-summary">{summary}</span>
        {failed > 0 && (
          <span className="text-[11.5px] text-danger-ink" role="alert">
            {failed === 1 ? '1 file failed — retry it or remove it.' : `${failed} files failed — retry or remove them.`}
          </span>
        )}
      </div>

      {rows.length > 0 && (
        <ul className="m-0 grid list-none gap-1.5 p-0">
          {rows.map(row => (
            <FileRow
              key={row.key}
              row={row}
              canUpload={!!upload}
              canRemoveAttached={!!onRemoveAttachment}
              onRetry={() => retry(row.key)}
              onCancel={() => cancel(row.key)}
              onRemove={() => void remove(row)}
            />
          ))}
        </ul>
      )}

      {error && <p className="m-0 text-[12px] leading-snug text-danger-ink" role="alert">{error}</p>}
      {rows.some(r => r.status === 'failed' || r.status === 'refused') && (
        <p className="m-0 text-[11.5px] leading-relaxed text-ink-3">
          Failed or refused files are never sent — removing them doesn't affect the rest of your request.
        </p>
      )}
    </div>
  )
}

/** One file's row: what it is, how it went, and the one action it needs. */
function FileRow({ row, canUpload, canRemoveAttached, onRetry, onCancel, onRemove }: {
  row: Row
  canUpload: boolean
  canRemoveAttached: boolean
  onRetry: () => void
  onCancel: () => void
  onRemove: () => void
}) {
  const tag = rowTag(row)
  const facts: RequestAttachmentFacts | undefined = row.answer?.attachment
  const name = facts?.file_name ?? row.file.name
  const size = facts?.size_bytes ?? row.file.size
  const showRemove = row.status !== 'attached' || canRemoveAttached
  const tone = tag.tone

  return (
    <li
      className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5 border bg-surface px-2.5 py-2 text-[12.5px]"
      style={{
        borderRadius: 'var(--radius)',
        borderColor: tone === 'flag' ? 'color-mix(in srgb, var(--danger) 45%, var(--surface))' : 'var(--line)',
      }}
      data-status={row.status}
    >
      {row.status === 'uploading' ? (
        <Loader2 size={14} className="flex-shrink-0 animate-spin text-ink-3" aria-hidden="true" />
      ) : tone === 'flag' ? (
        <AlertTriangle size={14} className="flex-shrink-0" style={{ color: 'var(--danger)' }} aria-hidden="true" />
      ) : isImageName(name) ? (
        <ImageIcon size={14} className="flex-shrink-0 text-ink-3" aria-hidden="true" />
      ) : (
        <FileText size={14} className="flex-shrink-0 text-ink-3" aria-hidden="true" />
      )}

      {/* The name takes the first line and truncates; the verdict and the
          actions wrap to their own line on a phone, so a row never squeezes a
          name down to three letters to fit four controls beside it. */}
      <span className="min-w-0 flex-1 truncate font-medium text-ink" title={name}>{fileNameForDisplay(name)}</span>
      <span className="amp-num flex-shrink-0 text-[10.5px] text-ink-3">{formatBytes(size)}</span>

      <span className="ml-auto flex w-full flex-wrap items-center justify-end gap-2 sm:w-auto">
        <Tag tone={tone} title={tag.hint}>{tag.label}</Tag>
        {row.status === 'uploading' && (
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={onCancel}
            aria-label={`Cancel upload of ${name}`}>Cancel</button>
        )}
        {row.status === 'failed' && canUpload && (
          <button type="button" className="amp-btn amp-btn-sm" onClick={onRetry}
            aria-label={`Retry upload of ${name}`}>Retry</button>
        )}
        {showRemove && row.status !== 'uploading' && (
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={onRemove}
            aria-label={`Remove ${name}`}>Remove</button>
        )}
      </span>

      {row.status === 'uploading' && (
        // The transport is fetch, which reports no upload progress; a percentage
        // here would be invented. The bar is therefore indeterminate, and says so
        // to a screen reader instead of claiming a number.
        <span
          className="mt-0.5 block h-1 w-full overflow-hidden bg-surface-3"
          style={{ borderRadius: 2 }}
          role="progressbar"
          aria-label={`Uploading ${name}`}
          aria-valuetext="Uploading"
          data-indeterminate="true"
        >
          <i className="block h-full w-2/5" style={{ background: 'var(--st-prog)' }} />
        </span>
      )}
      {row.status === 'failed' && row.error && (
        <span className="w-full text-[11.5px] leading-snug text-danger-ink">{row.error}</span>
      )}
    </li>
  )
}

function Tag({ tone, title, children }: { tone: 'ok' | 'wait' | 'flag' | 'quiet'; title: string; children: React.ReactNode }) {
  const color = tone === 'flag' ? 'var(--danger-ink)' : tone === 'ok' ? 'var(--success-ink)' : 'var(--ink-2)'
  const rule = tone === 'flag' ? 'var(--danger)' : tone === 'ok' ? 'var(--success)' : 'var(--line-str)'
  return (
    <span
      title={title}
      className="inline-flex flex-shrink-0 items-center gap-1 whitespace-nowrap border px-1.5 py-[4px] font-label text-[10px] font-semibold uppercase leading-none tracking-[0.14em]"
      style={{ borderRadius: 2, color, borderColor: `color-mix(in srgb, ${rule} 45%, var(--surface))`, background: 'var(--surface)' }}
    >
      <i className="block h-1.5 w-1.5" style={{ borderRadius: 999, background: rule }} aria-hidden="true" />
      {children}
    </span>
  )
}

const isImageName = (name: string) => /\.(png|jpe?g|gif|webp|bmp)$/i.test(name)
