/**
 * The words the team-inbox request panels use (AMP #3979 — the discussion,
 * history and attachment panels of build slice J2 in
 * `discoveries/team-inbox-approved-build-slices.md`; approved mockups 37 v1 for
 * the team surface, 38 v1 for the requester's).
 *
 * Every string here is a *word for a value the server already decided*: a
 * lifecycle state, a comment kind, a history action, a scan verdict, a download
 * refusal. None of them may invent a state, soften a refusal, or call a file
 * "clean" that was never scanned. Keeping them in one module is what stops the
 * three panels disagreeing about what a value means.
 *
 * Words only. Whether an act is offered, and whether a write is allowed, is the
 * component's decision; whether it is *permitted* is always the server's.
 */
import { REQUEST_STATE_LABEL } from './requestState'
import type {
  RequestAttachmentRefusal,
  RequestCommentKind,
  RequestEvent,
  RequestEventAction,
  RequestScanState,
  RequestStatus,
} from '../types'

/**
 * The lifecycle state in words.
 *
 * There is exactly one table of these words on the surface — J1's
 * `REQUEST_STATE_LABEL` in `lib/requestState.ts` — and this is a re-export
 * rather than a second copy. A history row names its from/to states, and the
 * same request's queue pill sits three inches above it; two tables would let
 * one say "Needs info" and the other "Needs information" about the same row.
 */
export const REQUEST_STATE_WORD: Readonly<Record<RequestStatus, string>> = REQUEST_STATE_LABEL

/**
 * What one discussion row is, in words.
 *
 * The distinction is the point (contract I5): a plain `comment` is the desk
 * talking among itself and is never shown to a requester, a `question` is the
 * one thing a requester is asked, and an `answer` is the only kind that returns
 * them to the queue. `system` is the record's own voice — a transition's note —
 * and is never requester-visible either.
 */
export const REQUEST_COMMENT_LABEL: Record<RequestCommentKind, string> = {
  comment: 'Internal comment',
  question: 'Question to the requester',
  answer: 'Answer from the requester',
  system: 'System note',
}

/** Whether a discussion row is part of the desk↔requester exchange rather than internal notes. */
export const REQUEST_COMMENT_SHARED: Record<RequestCommentKind, boolean> = {
  comment: false,
  question: true,
  answer: true,
  system: false,
}

/** The history verb for an act that names no destination state. */
export const REQUEST_ACTION_LABEL: Record<RequestEventAction, string> = {
  submit: 'Submitted',
  transition: 'Moved the request',
  claim: 'Claimed the request',
  release: 'Released the claim',
  assign: 'Changed the assignee',
  link: 'Linked work',
  unlink: 'Removed a link',
  edit: 'Edited the request',
}

/** The field names an `edit` history row may carry, in words. Ids stay in the row. */
const EDIT_FIELD_WORD: Record<string, string> = {
  subject: 'the subject',
  input_brief: 'the brief',
  outcome_brief: 'the delivery note',
  metadata: 'the metadata',
}

/** What a link's target is, in words. A kind this UI does not know says "work". */
const TARGET_WORD: Record<string, string> = {
  project: 'project',
  epic: 'epic',
  story: 'story',
  task: 'ticket',
  artifact: 'artifact',
  receipt: 'receipt',
  request: 'request',
}

const targetWord = (kind: unknown) => (typeof kind === 'string' ? TARGET_WORD[kind] ?? 'work' : 'work')

/** `#7`: a target's id when the row named one, empty otherwise. */
const targetRef = (id: unknown) => (typeof id === 'number' && Number.isFinite(id) && id > 0 ? ` #${id}` : '')

/**
 * The sentence a transition row reads as, so history says what happened rather
 * than "from triaging to needs_information". A destination the vocabulary does
 * not name (impossible through the schema, worth failing visibly) falls back to
 * the two state words.
 */
export function requestTransitionSentence(from: RequestStatus | undefined, to: RequestStatus | undefined): string {
  switch (to) {
    case 'triaging': return from === 'duplicate' ? 'reopened it out of Duplicate' : 'started triage'
    case 'needs_information': return 'asked the requester a question'
    case 'accepted': return 'accepted the request'
    case 'in_progress': return 'started work'
    case 'delivered': return 'delivered the outcome'
    case 'requester_accepted': return 'the requester accepted the outcome'
    case 'rejected': return 'rejected the request'
    case 'duplicate': return 'marked it a duplicate'
    case 'new': return 'put it back to New'
    default: break
  }
  const a = from ? REQUEST_STATE_WORD[from] : ''
  const b = to ? REQUEST_STATE_WORD[to] : ''
  if (a && b) return `moved it from ${a} to ${b}`
  return 'moved the request'
}

/** The `edit` row's sentence: the fields that changed, in words. */
export function requestEditSentence(fields: unknown): string {
  const names = Array.isArray(fields) ? fields.filter((f): f is string => typeof f === 'string') : []
  const words = names.map(f => EDIT_FIELD_WORD[f]).filter((w): w is string => !!w)
  if (words.length === 0) return 'edited the request'
  if (words.length === 1) return `edited ${words[0]}`
  return `edited ${words.slice(0, -1).join(', ')} and ${words[words.length - 1]}`
}

/**
 * The link/unlink row's sentence, from the row's own detail (ids and kinds
 * only — the history deliberately carries no names, because it is the same row
 * a requester's timeline is built from).
 */
export function requestLinkSentence(detail: { link_kind?: unknown; target_kind?: unknown; target_id?: unknown }, unlink: boolean): string {
  const target = targetWord(detail.target_kind)
  switch (detail.link_kind) {
    case 'routes_to':
      return unlink ? 'removed the routing to its project' : 'routed it to a project'
    case 'planned_as':
      return `${unlink ? 'removed a planned' : 'linked a planned'} ${target === 'project' ? 'item' : target}${targetRef(detail.target_id)}`
    case 'delivered_by':
      return `${unlink ? 'removed' : 'recorded'} ${target === 'work' ? 'delivery evidence' : `a delivered ${target}`}${targetRef(detail.target_id)}`
    case 'duplicate_of':
      return unlink ? 'detached it from its canonical request' : 'linked it to its canonical request'
    case 'relates_to':
      return `${unlink ? 'removed the link to' : 'linked'} a related request${targetRef(detail.target_id)}`
    default:
      return `${unlink ? 'removed a link to' : 'linked'} a ${target}${targetRef(detail.target_id)}`
  }
}

/**
 * What a scan verdict may be called.
 *
 * `unsupported` is *unknown*, not clean: AMP ships no malware scanner in v1, so
 * a file with that verdict reads "Not scanned" and the panel says so out loud.
 * Calling it clean would be the one lie a private-file panel must never tell.
 */
export function requestScanText(scan: RequestScanState | undefined): { label: string; title: string; bad: boolean } {
  switch (scan) {
    case 'clean':
      return { label: 'Scanned', title: 'A scan ran and found nothing.', bad: false }
    case 'pending':
      return { label: 'Scanning', title: 'The scan has not finished, so the file cannot be downloaded yet.', bad: false }
    case 'infected':
      return { label: 'Blocked', title: 'The scan found something. AMP will not serve this file.', bad: true }
    default:
      return {
        label: 'Not scanned',
        title: 'AMP does not run a malware scanner in v1, so this file was never scanned. It is not being called clean.',
        bad: false,
      }
  }
}

/**
 * Why a file cannot be downloaded, in a sentence a person can act on. The empty
 * refusal means it can be. An unknown word falls back to "not available right
 * now" rather than an invented reason — the server's decision stays the
 * server's, and a panel only explains it.
 */
export function requestRefusalText(refusal: RequestAttachmentRefusal | undefined): string {
  if (!refusal) return ''
  switch (refusal) {
    case 'scan_pending': return 'Waiting for the scan to finish.'
    case 'scan_infected': return 'The scan found something, so AMP will not serve this file.'
    case 'held_for_release': return 'Held for a team lead to release.'
    case 'purged': return 'Purged. AMP removes files 90 days after the request closes unless the team kept them.'
    default: return 'This file is not available right now.'
  }
}

/** `41 KB`, `2.1 MB`: the size a file row shows. */
export function requestFileSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '0 B'
  if (bytes < 1024) return `${Math.round(bytes)} B`
  if (bytes < 1048576) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / 1048576).toFixed(1)} MB`
}

/** The sniffed subtypes a file row names exactly, where the raw value reads badly. */
const FILE_TYPE_WORD: Record<string, string> = {
  'application/octet-stream': 'file',
  'application/x-msdownload': 'EXE',
  'application/x-msi': 'MSI',
  'application/x-msdos-program': 'EXE',
  'application/x-sh': 'SH',
  'application/x-bat': 'BAT',
  'application/x-httpd-php': 'PHP',
  'application/x-tar': 'TAR',
  'application/gzip': 'GZ',
  'application/x-gzip': 'GZ',
  'application/x-7z-compressed': '7Z',
  'application/x-rar-compressed': 'RAR',
  'application/vnd.rar': 'RAR',
  'application/msword': 'DOC',
  'application/vnd.ms-excel': 'XLS',
  'application/vnd.ms-powerpoint': 'PPT',
  'application/pdf': 'PDF',
  'application/json': 'JSON',
  'application/zip': 'ZIP',
  'application/xml': 'XML',
}

/**
 * The short type a file row shows: `XML`, `PNG`, `EXE`, `OFFICE`.
 *
 * Display only. The row's `content_type` is what AMP sniffed from the bytes —
 * never the client's header — and this is a label for it, not a second opinion
 * about what the file is or whether it may be served. An unknown type shows its
 * subtype upper-cased rather than a guess, because "we don't know" is a better
 * label than a wrong one.
 */
export function requestFileType(contentType: string): string {
  const type = (contentType || '').trim().toLowerCase()
  if (!type) return 'file'
  const known = FILE_TYPE_WORD[type]
  if (known) return known
  const [top, sub = ''] = type.split('/')
  if (top === 'image') return sub.replace(/^x-/, '').toUpperCase()
  if (top === 'text') return sub === 'plain' ? 'TXT' : sub.replace(/^x-/, '').toUpperCase()
  if (sub.startsWith('vnd.openxmlformats')) return 'OFFICE'
  if (sub.includes('zip')) return 'ZIP'
  if (sub.includes('json')) return 'JSON'
  if (sub.includes('xml')) return 'XML'
  if (sub.includes('csv')) return 'CSV'
  if (sub.startsWith('x-')) return sub.slice(2).toUpperCase()
  if (sub.startsWith('vnd.')) return (sub.split('.')[1] ?? 'file').toUpperCase()
  return sub ? sub.toUpperCase() : top.toUpperCase()
}

/**
 * When a history row happened: "11:02" today, "Nov 8, 09:41" otherwise. The
 * exact moment travels as the element's `title`, so one hover answers "when
 * exactly" without the list reading like a log file.
 */
export function requestEventTime(iso: string, now = new Date()): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
  }
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

/**
 * The detail JSON, parsed once. Every history `detail` is written by the server
 * from ids and kinds only (`request_claims.go`, `request_links.go`,
 * `request_edits.go` annotate their own shapes), so this reads the documented
 * fields and nothing else. A malformed value is `{}` rather than raw text: a
 * history row must never render a fragment of JSON as if it were a sentence.
 */
export function requestEventDetail(raw: string | undefined): Record<string, unknown> {
  if (!raw) return {}
  try {
    const parsed: unknown = JSON.parse(raw)
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

/**
 * One history row's sentence — and it never claims more than the row does. A
 * transition names its two states, a link names the kind and target the row
 * recorded, an edit names the fields its detail lists, and every other act uses
 * its verb. Nothing is inferred: a linked ticket closing is not a history row
 * here, because a closing ticket never flips a request (I8).
 */
export function requestEventSentence(e: Pick<RequestEvent, 'action' | 'from_status' | 'to_status' | 'detail'>): string {
  const detail = requestEventDetail(e.detail)
  switch (e.action) {
    case 'transition':
      return requestTransitionSentence(e.from_status, e.to_status)
    case 'edit':
      return requestEditSentence(detail.fields)
    case 'link':
      return requestLinkSentence(detail, false)
    case 'unlink':
      return requestLinkSentence(detail, true)
    default:
      return (REQUEST_ACTION_LABEL[e.action] ?? 'changed the request').toLowerCase()
  }
}
