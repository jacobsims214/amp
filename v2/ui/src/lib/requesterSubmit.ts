/**
 * The requester submission form's own logic (AMP #3985, build slice J4 of
 * `discoveries/team-inbox-approved-build-slices.md`; approved mockup artifact
 * 38 v1, "New request" screen; approved product contract artifact 36 v2 §2/§5).
 *
 * `pages/RequesterSubmit.tsx` renders the form; this file decides the four
 * things a page should not decide inline, each with its own test:
 *
 *  1. **What the form sends, and what it refuses to send.** The submit body is
 *     `{subject, input_brief, metadata?}` and nothing else — the route refuses
 *     an unknown field by name, so a form that sent a contact address, an
 *     actor, a project or a state would be refused rather than quietly
 *     stripped. The requester's contact is captured server-side from the
 *     sign-in, which is why the two "Getting back to you" fields are read-only
 *     and are never part of the body.
 *  2. **One retry token per draft, kept across a reload.** A submission is
 *     idempotent by the `Idempotency-Key` header: the same key with the same
 *     payload answers with the request the first attempt wrote. The key and the
 *     payload it belongs to live in this tab's `sessionStorage`, so a retry
 *     after an uncertain answer (the request may have been written) replays
 *     instead of creating a second request — and a *changed* draft mints a new
 *     key, because the same key with a different payload is a 409 by design.
 *  3. **Validation that names the field and never loses a word.** The bounds
 *     are the server's (`domain.MaxRequestSubjectLen`, `MaxRequestBriefLen`),
 *     applied before the round trip; the messages say what to do.
 *  4. **Errors said in the requester's own terms.** A 404 means "this desk has
 *     not invited your sign-in" (never "not found", never "not yours" — the
 *     surface refuses to distinguish them), a 403 is a permission sentence, a
 *     429 carries its wait, and a dropped connection says plainly that nothing
 *     was lost and that retrying cannot duplicate the request.
 *
 * Two product facts this file takes seriously, because a form is where they
 * would otherwise be invented:
 *
 *  - **AMP sends no email.** The mockup's success screen says "We emailed a
 *    confirmation to …"; nothing in AMP can, so nothing here says it. Every
 *    sentence a requester reads is about this page.
 *  - **Two mockup fields have no field on the wire.** "What done looks like"
 *    is folded into `input_brief` under its own heading (the `outcome_brief`
 *    column is the *team's* delivery statement — contract §4 — and a submitter
 *    may not set it), and "Kind of request"/urgency ride in `metadata` as
 *    triage routing hints (the submit body's own comment allows "connector
 *    labels and routing hints only"). Neither is an authorization decision, a
 *    typed relation or a state: nothing in AMP reads either to decide access.
 */
import type { Metadata, SubmitRequestInput } from '../types'
import { NO_PERMISSION, isBareStatus } from './apiError'

/**
 * The bounds the form applies before the round trip.
 *
 * Copied from `v2/amp-api/internal/domain/request.go` — `MaxRequestSubjectLen`
 * (200) and `MaxRequestBriefLen` (20000). The server still enforces its own;
 * these exist so a person is told in the form instead of after a failed send,
 * and so the composed brief (see `composeInputBrief`) can never be over the
 * bound the server would refuse.
 */
export const MAX_SUBJECT_LEN = 200
export const MAX_BRIEF_LEN = 20000

/** The heading the desired outcome is folded under inside `input_brief`. */
export const OUTCOME_HEADING = 'What "done" looks like:'

/**
 * The kinds of request the approved mockup offers, as stable slugs.
 *
 * The value is what is stored (`metadata.request_kind`); the label is what the
 * person reads. Slugs are not the label because a label is copy and copy
 * changes: a stored hint that renamed itself would make last month's requests
 * unsearchable.
 */
export const REQUEST_KINDS = [
  { value: 'not_working', label: "Something isn't working" },
  { value: 'new_work', label: 'New work or a change' },
  { value: 'agreement', label: 'A question about our agreement' },
  { value: 'access', label: 'Access or an account' },
] as const

export type RequestKind = (typeof REQUEST_KINDS)[number]['value']

/** How much it is blocking the requester: the mockup's three-step scale. */
export const REQUEST_URGENCIES = [
  { value: 'not_urgent', label: 'Not urgent' },
  { value: 'slowing_us_down', label: 'Slowing us down' },
  { value: 'work_stopped', label: 'Work stopped' },
] as const

export type RequestUrgency = (typeof REQUEST_URGENCIES)[number]['value']

/** The metadata keys the routing hints are stored under. */
export const REQUEST_KIND_METADATA_KEY = 'request_kind'
export const REQUEST_URGENCY_METADATA_KEY = 'urgency'

/** Everything the form holds. Nothing here is sent verbatim — see buildSubmitBody. */
export interface SubmitDraft {
  subject: string
  brief: string
  /** "What done looks like for you" — optional, folded into the brief on send. */
  outcome: string
  kind: RequestKind | ''
  urgency: RequestUrgency
}

/**
 * A blank form. `urgency` starts where the mockup's segmented control starts
 * ("Slowing us down" pressed): a scale with no default makes a person answer a
 * question they may not have an opinion about before they can send anything.
 */
export const EMPTY_DRAFT: SubmitDraft = {
  subject: '',
  brief: '',
  outcome: '',
  kind: '',
  urgency: 'slowing_us_down',
}

/** Which field an error belongs to. The order is the form's order (and the summary's). */
export type DraftField = 'subject' | 'kind' | 'brief'

/** The form's field order, for the error summary and for focusing the first bad one. */
export const DRAFT_FIELD_ORDER: readonly DraftField[] = ['subject', 'kind', 'brief']

export type DraftErrors = Partial<Record<DraftField, string>>

/** How many characters (runes, not UTF-16 units) a string has — the server's measure. */
export function charCount(s: string): number {
  return [...s].length
}

/**
 * The brief as it is stored: the requester's words, plus their desired outcome
 * under its own heading when they wrote one.
 *
 * Folding rather than a second field is deliberate. The submit body has exactly
 * one content field (`input_brief`), and the record's `outcome_brief` column is
 * the *team's* statement of what it delivered (contract §4) — a submitter's
 * desired outcome must not be written there, and is refused by name on the edit
 * route for the same reason. So the one place both halves can live and be read
 * back by both sides is the brief. The heading is fixed text, so the requester's
 * own words are never rewritten.
 */
export function composeInputBrief(brief: string, outcome: string): string {
  const b = brief.trim()
  const o = outcome.trim()
  if (!o) return b
  return b ? `${b}\n\n${OUTCOME_HEADING}\n${o}` : `${OUTCOME_HEADING}\n${o}`
}

/**
 * What is wrong with the draft, per field. Empty when it may be sent.
 *
 * Being stricter than the server on two points is intentional and matches the
 * approved mockup's own error state: a subject and a description are both
 * required. The server accepts an empty brief (`NormalizeRequestBrief` allows
 * one until it is needed), but a request with no description is the one thing
 * the desk cannot triage from, and the mockup says so in its own words.
 */
export function validateDraft(draft: SubmitDraft): DraftErrors {
  const errors: DraftErrors = {}

  const subject = draft.subject.trim()
  const subjectLen = charCount(subject)
  if (!subject) {
    errors.subject = 'Give your request a subject, like "Export of invoice history missing October".'
  } else if (subjectLen > MAX_SUBJECT_LEN) {
    errors.subject = `That subject is ${subjectLen} characters — ${MAX_SUBJECT_LEN} is the most the desk can list. Shorten it.`
  }

  if (!draft.kind) {
    errors.kind = 'Choose the closest kind — it tells the desk who should see it first.'
  }

  const brief = draft.brief.trim()
  if (!brief) {
    errors.brief = "A sentence or two is enough — but the desk can't start from an empty description."
  } else {
    const composed = charCount(composeInputBrief(brief, draft.outcome))
    if (composed > MAX_BRIEF_LEN) {
      errors.brief =
        `Your description and what done looks like come to ${composed} characters — ${MAX_BRIEF_LEN} is the limit. ` +
        'Trim it down; a long log or screenshot belongs in a file.'
    }
  }

  return errors
}

/** Whether the draft may be sent. */
export function isValidDraft(draft: SubmitDraft): boolean {
  return Object.keys(validateDraft(draft)).length === 0
}

/** The first field with an error, in form order — what a submit focuses. */
export function firstInvalidField(errors: DraftErrors): DraftField | null {
  return DRAFT_FIELD_ORDER.find(f => errors[f] != null) ?? null
}

/** The DOM id of a field's control, used for the summary's links and for focus. */
export function fieldControlId(field: DraftField): string {
  return `in-${field}`
}

/**
 * The body a submission sends.
 *
 * Three fields, in the route's own vocabulary, and nothing else: no contact, no
 * actor, no org or team (the path names the team), no project (intake routes
 * nothing — contract §3), no state (intake lands in `new`) and no
 * idempotency key (that is the `Idempotency-Key` header, because a body field
 * could be read as content). The submission route refuses an unknown field by
 * name, so a payload that grew one would be a 400 rather than a silent drop.
 *
 * `metadata` carries the two routing hints the mockup collects. Metadata is the
 * contract's extension space for labels and routing hints and is never an
 * authorization decision, never a typed relation and never a lifecycle input —
 * so a hint here cannot move a request, grant anyone anything, or change who
 * sees it.
 */
export function buildSubmitBody(draft: SubmitDraft): SubmitRequestInput {
  const metadata: Metadata = {}
  if (draft.kind) metadata[REQUEST_KIND_METADATA_KEY] = draft.kind
  if (draft.urgency) metadata[REQUEST_URGENCY_METADATA_KEY] = draft.urgency

  return {
    subject: draft.subject.trim(),
    input_brief: composeInputBrief(draft.brief, draft.outcome),
    ...(Object.keys(metadata).length ? { metadata } : {}),
  }
}

/**
 * The exact text a retry token is bound to: the team and the body, and nothing
 * that is not sent.
 *
 * Two drafts with the same payload are the same submission and must share a
 * key; one character different is a different submission, and reusing the key
 * would make the server answer 409 ("same key, different payload") — a dead end
 * the person could only escape by reloading.
 */
export function submissionPayload(teamId: number, body: SubmitRequestInput): string {
  // Key order is fixed by buildSubmitBody, so JSON is stable for a given draft.
  return JSON.stringify([teamId, body])
}

// ---- The in-flight retry token ----

/** What this tab remembers about the submission it has in flight. */
export interface InflightSubmission {
  /** The `Idempotency-Key` header value. */
  key: string
  /** The `submissionPayload` it was minted for; a different payload means a new key. */
  payload: string
}

/**
 * The slice of `Storage` this file uses, injectable so the rules can be tested
 * without a browser (and so a page could pass its own store in future).
 */
export interface KeyStorage {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

/** Where one team's in-flight submission lives. */
export function inflightPath(teamId: number): string {
  return `amp.requester-submit.${teamId}`
}

/**
 * This tab's `sessionStorage`, or null when it is unavailable (private mode, a
 * blocked origin, or a test environment).
 *
 * `sessionStorage` and not `localStorage`: the token belongs to one draft in one
 * tab. Two tabs are two drafts, and sharing one key between them would let the
 * second tab's submission replay the first's — answering with a request whose
 * subject the person never sent from that tab. Session storage also ends with
 * the tab, which is the right lifetime for a token nobody should have to think
 * about again.
 */
export function browserDraftStorage(): KeyStorage | null {
  try {
    const s = window.sessionStorage
    const probe = 'amp.requester-submit.probe'
    s.setItem(probe, '1')
    s.removeItem(probe)
    return s
  } catch {
    return null
  }
}

/** A fresh, opaque retry token. 36 hex characters, well inside 061's 255-byte bound. */
export function newSubmissionKey(): string {
  const c = globalThis.crypto
  if (c && typeof c.getRandomValues === 'function') {
    const bytes = new Uint8Array(18)
    c.getRandomValues(bytes)
    return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('')
  }
  // No Web Crypto (an ancient or stripped runtime). A retry token is not a
  // secret — it only has to be unique to this draft — so this is acceptable,
  // and it is still 36-ish characters of entropy rather than a counter.
  return `k${Date.now().toString(36)}${Math.random().toString(36).slice(2, 12)}`
}

/** The submission this tab already has in flight for a team, if any. */
export function loadInflightSubmission(
  teamId: number,
  storage: KeyStorage | null = browserDraftStorage(),
): InflightSubmission | null {
  if (!storage) return null
  try {
    const raw = storage.getItem(inflightPath(teamId))
    if (!raw) return null
    const held = JSON.parse(raw) as Partial<InflightSubmission>
    if (typeof held?.key !== 'string' || !held.key || typeof held.payload !== 'string') return null
    return { key: held.key, payload: held.payload }
  } catch {
    // An unreadable record is a missing one: a new key is always safe (the
    // worst case is the server's "same key, different payload" never arising),
    // while throwing here would take the form down.
    return null
  }
}

/**
 * The key this submission must use: the one already in flight for the same
 * payload, or a fresh one — recorded before the call, so a tab that dies
 * mid-request still retries with it afterwards.
 */
export function idempotencyKeyFor(
  teamId: number,
  payload: string,
  storage: KeyStorage | null = browserDraftStorage(),
): string {
  const held = loadInflightSubmission(teamId, storage)
  if (held && held.payload === payload) return held.key

  const key = newSubmissionKey()
  if (storage) {
    try {
      storage.setItem(inflightPath(teamId), JSON.stringify({ key, payload } satisfies InflightSubmission))
    } catch {
      // Storage is full or blocked. The submission still goes out with this
      // key; it is simply not remembered, so a reload mints another one.
    }
  }
  return key
}

/**
 * Forget a team's in-flight submission. Called once the server has answered
 * with a request — from then on the draft is over and the next send is a new
 * request that needs its own key.
 */
export function clearInflightSubmission(
  teamId: number,
  storage: KeyStorage | null = browserDraftStorage(),
): void {
  if (!storage) return
  try {
    storage.removeItem(inflightPath(teamId))
  } catch {
    // Nothing to do: a key that lingers only costs the next send a replay it
    // would have matched anyway (the payload differs, so a new key is minted).
  }
}

// ---- What a failure says ----

/** One failed submission, as the form shows it. */
export interface SubmissionNotice {
  title: string
  body: string
  /** A 429's wait, when the server said one. */
  retryInS?: number
}

/** The status and code facts an `HttpError` carries, read without importing it. */
function errorFacts(err: unknown): { status?: number; code?: string; message: string; retryAfterS?: number } {
  const e = err as { status?: number; code?: string; message?: string; retryAfterS?: number } | null | undefined
  return {
    status: typeof e?.status === 'number' ? e.status : undefined,
    code: typeof e?.code === 'string' ? e.code : undefined,
    message: typeof e?.message === 'string' ? e.message.trim() : '',
    retryAfterS: typeof e?.retryAfterS === 'number' ? e.retryAfterS : undefined,
  }
}

/**
 * What to tell a requester whose submission failed.
 *
 * Every branch says whether anything was written, because that is the question a
 * person actually has when a form fails. A 4xx refusal wrote nothing (the
 * service refuses before it creates, and a 404 is the surface's single
 * not-found). A dropped connection or a 5xx is the one case where the answer is
 * unknown — and there the sentence is that retrying is safe, which is true
 * because the retry reuses this draft's key.
 *
 * The 404 wording never suggests the request exists and is hidden: on this
 * surface a team you may not submit to, a team that does not exist and a revoked
 * grant are one answer by design (B4), so the form must not offer a distinction
 * the API refuses to make.
 */
export function submissionNotice(err: unknown, deskName: string): SubmissionNotice {
  const { status, code, message, retryAfterS } = errorFacts(err)
  const desk = deskName.trim() || 'this desk'
  const uncertain =
    'Nothing you wrote was lost — it is all still in the form. Sending again reuses this submission, ' +
    'so a request that did get through cannot be sent twice.'

  if (status === 429 || code === 'request.rate_limited') {
    const wait = retryAfterS && retryAfterS > 0 ? ` in ${retryAfterS} second${retryAfterS === 1 ? '' : 's'}` : ' in a moment'
    return {
      title: 'That was a lot of requests at once',
      body: `Nothing was sent. Try again${wait} — the desk limits how many requests arrive at once.`,
      retryInS: retryAfterS,
    }
  }

  if (status === 404 || code === 'request.not_found') {
    return {
      title: `Your sign-in can't send to ${desk}`,
      body:
        `Nothing was sent. Requests are accepted only from sign-ins the desk has invited — ask ${desk} to invite ` +
        'this address, then come back and send this again.',
    }
  }

  if (status === 403) {
    return {
      title: `You can't send requests to ${desk}`,
      // The server's own sentence when it gave one. A bare status word is not a
      // reason a person can act on, so it becomes the permission sentence — the
      // same rule the API client applies to every failure (lib/apiError.ts),
      // restated here because this notice is built from the error, not from a
      // response body the client already cleaned.
      body: isBareStatus(message) ? NO_PERMISSION : message,
    }
  }

  if (status === 409 || code === 'request.conflict') {
    return {
      title: 'This may already have been sent',
      body:
        (message ? `${message} ` : '') +
        'Check your requests before sending again: if this request is already there, the desk has it.',
    }
  }

  if (status === 400 || code === 'request.invalid') {
    return {
      title: "The desk couldn't accept this as it stands",
      body: `${message || 'Something about the form was refused.'} Nothing was sent.`,
    }
  }

  if (status !== undefined && status < 500) {
    return {
      title: "The request wasn't sent",
      body: `${message || `The desk's system refused it (HTTP ${status}).`} Nothing was sent.`,
    }
  }

  return {
    title: "Sending didn't finish",
    body: status === undefined
      ? uncertain
      : `${message || `The desk's system did not answer (HTTP ${status}).`} ${uncertain}`,
  }
}
