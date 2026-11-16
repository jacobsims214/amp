import type { RequestAttachmentLimits, RequestScanState, RequestUploadAnswer, RequestUploadInput } from '../types'

// The requester's side of a private upload: the bounds, the words, and the one
// place bytes are read. Nothing here decides anything the server decides — who
// may upload, what a file's stored type is, what its scan state is, whether it
// is held, and whether anyone may download it are all the API's and the
// domain's (v2/amp-api/internal/domain/request_attachment.go). What this file
// owns is the half a browser has to own:
//
//  1. **The bounds, said before a round trip.** A person who picks a 96 MB file
//     should be told "25 MB max" in the form, not after a failed upload. The
//     numbers are copied from the domain's constants and are *the server's*
//     numbers, not a second policy: the server still refuses anything over
//     them, and a refusal it sends is shown as it sent it.
//  2. **Honest words about a scan that never happened.** AMP ships no malware
//     scanner in v1, so every upload lands on `unsupported`. The wording below
//     is the one place that says so, and it can never say "clean" or "scanned"
//     about `unsupported` — that would be a security claim the product cannot
//     back (see the domain doc's "Clean is a claim; unsupported is the absence
//     of one").
//  3. **Bytes read locally, always.** `fileToUploadInput` is the only function
//     that turns a picked file into a body, and it reads the file with a
//     FileReader. Nothing here fetches a URL, and the upload route takes no
//     location for the same reason: AMP never fetches an address a caller names.

/**
 * AMP's upload bounds, as a client needs them before the first call.
 *
 * Mirrors `MaxRequestAttachmentBytes` / `MaxRequestAttachments` /
 * `MaxRequestAttachmentsTotalBytes` / `RequestAttachmentRetentionDays` in
 * `v2/amp-api/internal/domain/request_attachment.go` (25 MiB, 5, 60 MiB, 90
 * days — the numbers the approved mockups show as "25 MB each, 5 files"). A
 * listing from the API carries the live values in `limits`, and a component
 * prefers those; this constant is what a page uses when it has no listing yet,
 * which is exactly the submit form's case (no request exists to list).
 */
export const REQUEST_UPLOAD_LIMITS: RequestAttachmentLimits = {
  max_file_bytes: 25 << 20,
  max_files: 5,
  max_total_bytes: 60 << 20,
  retention_days: 90,
}

/**
 * The `accept` attribute for the picker: the formats the mockup names, plus the
 * two container types the server stores but holds.
 *
 * It is a hint for the file dialog and nothing else. A browser's accept filter
 * is advisory (a drag-and-drop bypasses it, and an OS dialog can be switched to
 * "all files"), and the type that counts is the one AMP sniffs from the bytes —
 * so this list never refuses anything and never grants anything.
 */
export const REQUEST_UPLOAD_ACCEPT =
  '.pdf,.png,.jpg,.jpeg,.gif,.webp,.bmp,.csv,.txt,.log,.json,.xml,.html,.zip,.gz,application/pdf,image/*,text/*'

/** "512 B", "182 KB", "1.2 MB", "25 MB" — the mockup's own vocabulary. */
export function formatBytes(bytes: number): string {
  const n = Number.isFinite(bytes) ? Math.max(0, bytes) : 0
  if (n < 1024) return `${Math.round(n)} B`
  const kb = n / 1024
  if (kb < 1024) return `${kb < 10 ? round1(kb) : Math.round(kb)} KB`
  const mb = kb / 1024
  if (mb < 1024) return `${mb < 10 ? round1(mb) : Math.round(mb)} MB`
  return `${round1(mb / 1024)} GB`
}

const round1 = (n: number) => Math.round(n * 10) / 10

/** Why a picked file was refused before any upload was attempted. */
export type PickRefusal = 'empty' | 'too_large' | 'too_many' | 'total_too_large'

/** What a selection of files came to: what may be uploaded, and what may not. */
export interface PickPlan {
  accepted: File[]
  refused: { file: File; reason: PickRefusal }[]
}

/**
 * Apply the bounds to a selection, in the order a person can act on.
 *
 * `kept` is the files the request already counts — uploaded, uploading and
 * queued ones. A refused or cancelled row is not counted, because it is not on
 * the request: refusing a sixth file while a refused row sits in the list would
 * tell the person to remove a file that was never sent.
 *
 * The count is applied first (a sixth file is over the limit whatever its size),
 * then the per-file bound, then the running total — so a selection of six huge
 * files reports the reason that is actually true of each, rather than blaming
 * the total for the first five.
 */
export function planPicks(
  kept: File[],
  incoming: File[],
  limits: RequestAttachmentLimits = REQUEST_UPLOAD_LIMITS,
): PickPlan {
  const plan: PickPlan = { accepted: [], refused: [] }
  let count = kept.length
  let total = kept.reduce((n, f) => n + f.size, 0)

  for (const file of incoming) {
    if (file.size <= 0) {
      plan.refused.push({ file, reason: 'empty' })
      continue
    }
    if (count >= limits.max_files) {
      plan.refused.push({ file, reason: 'too_many' })
      continue
    }
    if (file.size > limits.max_file_bytes) {
      plan.refused.push({ file, reason: 'too_large' })
      continue
    }
    if (total + file.size > limits.max_total_bytes) {
      plan.refused.push({ file, reason: 'total_too_large' })
      continue
    }
    plan.accepted.push(file)
    count += 1
    total += file.size
  }
  return plan
}

/** The bound, named: a refusal a person can act on rather than "invalid file". */
export function pickRefusalMessage(
  reason: PickRefusal,
  limits: RequestAttachmentLimits = REQUEST_UPLOAD_LIMITS,
): string {
  switch (reason) {
    case 'empty':
      return 'Empty file — there is nothing to send.'
    case 'too_large':
      return `Too large — ${formatBytes(limits.max_file_bytes)} max`
    case 'too_many':
      return `${limits.max_files} files max per request`
    case 'total_too_large':
      return `${formatBytes(limits.max_total_bytes)} total max`
  }
}

/** How a file's scan verdict is said out loud, and which colour it is. */
export interface ScanWording {
  label: string
  hint: string
  tone: 'ok' | 'wait' | 'flag'
}

/**
 * The words for one file's scan state and hold.
 *
 * The rule this function exists for: `unsupported` is **not** clean, so it may
 * never be labelled "Scanned" or "Clean". AMP has no scanner in v1 and every
 * upload lands on `unsupported`; saying otherwise would tell a requester their
 * file was checked when nothing checked it. `pending` is a real queue waiting
 * for a verdict, and `clean` is the only state that may claim a scan happened.
 *
 * The hold is reported first because it is the actionable fact ("a team lead
 * must release it"), and a held file that is also unscanned says both.
 */
export function scanWording(scan: RequestScanState, held: boolean): ScanWording {
  const unscanned = scan === 'unsupported'
  if (held) {
    return {
      label: 'Held for review',
      hint: 'An archive, an executable or a script: it is stored, and a team lead has to release it before anyone can download it.'
        + (unscanned ? ' It is also unscanned.' : ''),
      tone: 'flag',
    }
  }
  switch (scan) {
    case 'clean':
      return { label: 'Scanned', hint: 'Scanned and nothing was found.', tone: 'ok' }
    case 'infected':
      return { label: 'Blocked', hint: 'A scan found a problem, so nobody can download this file.', tone: 'flag' }
    case 'pending':
      return { label: 'Scanning', hint: 'Waiting for the scan to finish before anyone can open it.', tone: 'wait' }
    case 'unsupported':
      return {
        label: 'Not scanned',
        hint: 'AMP cannot scan this kind of file, so it is stored unscanned — nothing has inspected its contents.',
        tone: 'wait',
      }
  }
}

/**
 * One picked file as the upload route's body: its name and its bytes.
 *
 * The bytes are read here, locally, with a FileReader — there is no URL, no
 * path and no place to fetch in the result, which is the same reason the route
 * refuses a location-shaped field by name. The data-URL prefix is dropped: it
 * carries the *browser's* guess at the type, and the stored type is what AMP
 * sniffs from the bytes.
 */
export function fileToUploadInput(file: File): Promise<RequestUploadInput> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(new Error('AMP could not read that file. Try choosing it again.'))
    reader.onload = () => {
      const result = typeof reader.result === 'string' ? reader.result : ''
      const comma = result.indexOf(',')
      if (comma < 0) {
        reject(new Error('AMP could not read that file. Try choosing it again.'))
        return
      }
      resolve({ file_name: file.name, content_base64: result.slice(comma + 1) })
    }
    reader.readAsDataURL(file)
  })
}

/** A file name short enough to render, with the full name kept for the tooltip. */
export function fileNameForDisplay(name: string, max = 64): string {
  const n = name.trim()
  if (n.length <= max) return n
  const dot = n.lastIndexOf('.')
  const ext = dot > 0 && n.length - dot <= 12 ? n.slice(dot) : ''
  return `${n.slice(0, Math.max(1, max - ext.length - 1))}…${ext}`
}

/**
 * Upload one file, as the picker awaits it. `signal` aborts the upload when the
 * person cancels; the picker treats a rejection whose signal is aborted as a
 * cancellation, not a failure, so a page's implementation must reject on abort
 * (or simply let `abortableUpload` below do it).
 */
export type UploadFile = (file: File, signal: AbortSignal) => Promise<RequestUploadAnswer>

/**
 * Bind a one-argument upload call (a page's `requestApi.uploadAttachment(team,
 * request, input)` with its ids filled in) to the picker's shape, reading the
 * bytes locally and settling on abort.
 *
 * The settle-on-abort half is not cosmetic. The API client does not take an
 * AbortSignal (its transport is one `fetch` per call), so a cancelled upload's
 * request may still complete on the server. What matters is that the *person*
 * is not left waiting: the wrapper rejects at once, the picker drops the row,
 * and a later answer patches nothing because the row is gone. That is also why
 * a cancelled file is honest to call "never sent" only once the request cannot
 * be resumed — the row is removed, and a retry of the same file is a new upload.
 */
export function abortableUpload(
  call: (input: RequestUploadInput) => Promise<RequestUploadAnswer>,
): UploadFile {
  return async (file, signal) => {
    if (signal.aborted) throw new DOMException('The upload was cancelled.', 'AbortError')
    const input = await fileToUploadInput(file)
    if (signal.aborted) throw new DOMException('The upload was cancelled.', 'AbortError')
    return new Promise<RequestUploadAnswer>((resolve, reject) => {
      const onAbort = () => reject(new DOMException('The upload was cancelled.', 'AbortError'))
      signal.addEventListener('abort', onAbort, { once: true })
      call(input).then(
        answer => { signal.removeEventListener('abort', onAbort); resolve(answer) },
        (e: unknown) => { signal.removeEventListener('abort', onAbort); reject(e) },
      )
    })
  }
}
