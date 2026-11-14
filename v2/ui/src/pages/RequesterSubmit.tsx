/**
 * The requester's "New request" screen (AMP #3985, build slice J4 of
 * `discoveries/team-inbox-approved-build-slices.md`): the form a customer fills
 * in to send a request to a team's inbox, and the success screen that answers
 * with the reference they will quote from then on.
 *
 * Built to the approved requester mockup (artifact 38 v1, the "New request"
 * screen and its four states: filled, validation errors, uploading/failed file,
 * submitted). It composes the two components #3981 built —
 * `components/requests/RequesterChrome` (a customer frame with no room for
 * project internals) — and owns no routing: the page takes the team as props and
 * its links as callbacks, so #3988 can mount it at whatever URL it wires.
 *
 * ## The four things this page is careful about
 *
 * 1. **One retry token per draft.** The submission route is idempotent by its
 *    `Idempotency-Key` header, which the browser mints. The key is minted before
 *    the call and kept in this tab's `sessionStorage` (lib/requesterSubmit.ts),
 *    so an uncertain answer — a dropped connection, a 5xx, a reload — is retried
 *    as a *replay* of the same submission rather than a second request. The
 *    screen says that out loud, because it is the one fact that makes a person
 *    willing to press Retry.
 *
 * 2. **A failure never costs the words.** The draft lives in component state and
 *    nothing on the failure path touches it: no field is cleared, no value is
 *    replaced by the server's echo, and the form is still there with the error
 *    above it. The only thing a failed send changes is the notice — and its
 *    Retry only re-sends.
 *
 * 3. **The contact is not a field.** A request's requester is captured
 *    server-side from the sign-in (`requester_contact` is written from the
 *    principal, and the submit body has no such field — it would be refused by
 *    name). The two "Getting back to you" inputs are therefore read-only and are
 *    never part of the body; there is deliberately nowhere on this page to type a
 *    different address.
 *
 * 4. **No promise the product cannot keep.** The mockup's success screen says
 *    "we emailed a confirmation" and its file rows show an upload percentage.
 *    AMP sends no email at all (the API imports no mailer) and `fetch` reports no
 *    upload progress, so neither sentence appears here. The success screen says
 *    what is actually true: the reference is the thing to keep, and the request
 *    is checked from *Your requests*.
 *
 * ## Two deliberate differences from the approved mockup, both stated in place
 *
 * - **No attachment picker on this form.** The picker #3981 built is bounded,
 *   retryable and honest, but there is no route for a *requester* to upload to a
 *   request: the upload route is team-addressed and takes the request's serial,
 *   and the requester projection carries neither the serial nor the team. A
 *   picker here would therefore accept files it could never send — a person would
 *   watch a request go out believing their log went with it. So the section says
 *   plainly that files cannot be attached from this form yet and what to do
 *   instead. When a requester-addressed upload exists, the section becomes
 *   `<AttachmentPicker upload={...} />` with no other change to this page.
 * - **No "Save draft" button.** A draft that survives a reload would have to be
 *   stored somewhere, and this page stores exactly one thing (the retry token).
 *   The form keeps everything in memory, so a reload starts again — which is
 *   what "send this in one sitting" means, said by the absence of a button
 *   rather than by a button that does nothing.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, Check, Copy, Loader2, Lock, RotateCw } from 'lucide-react'
import { requestApi } from '../api/client'
import type { RequestSubmissionAnswer } from '../types'
import { RequesterChrome } from '../components/requests/RequesterChrome'
import { requesterNavItemClass, type RequesterIdentity } from '../lib/requesterChrome'
import {
  DRAFT_FIELD_ORDER, EMPTY_DRAFT, MAX_BRIEF_LEN, MAX_SUBJECT_LEN, REQUEST_KINDS, REQUEST_URGENCIES,
  buildSubmitBody, charCount, clearInflightSubmission, fieldControlId, idempotencyKeyFor,
  isValidDraft, submissionNotice, submissionPayload, validateDraft,
  type DraftErrors, type DraftField, type SubmissionNotice, type SubmitDraft,
} from '../lib/requesterSubmit'
import { requestRef } from '../lib/requestState'
import { withTimeout } from '../lib/timeout'

export interface RequesterSubmitProps {
  /** The inbox this request goes to. The path names it; nothing else on the page can change it. */
  teamId: number
  /**
   * The team's public display name ("Northwind support desk"). Passed in, not
   * fetched: a customer has no org standing, so `GET /api/teams/:id` answers
   * them the team surface's 404. The routing ticket supplies it (an invite link
   * knows which desk it is for).
   */
  teamName: string
  /** The signed-in requester, as `/api/me` answers. The contact snapshot is theirs and read-only. */
  me: RequesterIdentity
  /** The requester's own two tabs, as the page's own links (#3988 owns routing). */
  nav?: { label: string; current: boolean; onClick?: () => void }[]
  /** An access notice to sit under the frame's rule ("your grant needs renewing", say). */
  banner?: React.ReactNode
  onSignOut?: () => void
  /** Open a request the person just sent. Without it, the success screen offers no such button. */
  onOpenRequest?: (publicRef: string) => void
  /** Go to the person's own request list. Without it, no such button. */
  onViewRequests?: () => void
  /** Go to a blank form (the success screen's "send another"). Without it, no such button. */
  onSendAnother?: () => void
}

/** Which screen the page is showing. One variable, so two states cannot both be true. */
type Screen =
  | { kind: 'form' }
  | { kind: 'sending' }
  | { kind: 'failed'; notice: SubmissionNotice }
  | { kind: 'sent'; answer: RequestSubmissionAnswer }

export function RequesterSubmit({
  teamId, teamName, me, nav, banner, onSignOut, onOpenRequest, onViewRequests, onSendAnother,
}: RequesterSubmitProps) {
  const [draft, setDraft] = useState<SubmitDraft>(EMPTY_DRAFT)
  const [errors, setErrors] = useState<DraftErrors>({})
  const [screen, setScreen] = useState<Screen>({ kind: 'form' })
  // The summary is only rendered after a refused submit, never while someone is
  // still filling the form in: an error list that appears before a person has
  // had a chance to type is nagging, not help.
  const [showSummary, setShowSummary] = useState(false)
  const summaryRef = useRef<HTMLDivElement>(null)
  const subjectRef = useRef<HTMLInputElement>(null)
  const kindRef = useRef<HTMLSelectElement>(null)
  const briefRef = useRef<HTMLTextAreaElement>(null)

  const set = <K extends keyof SubmitDraft>(field: K, value: SubmitDraft[K]) => {
    setDraft(d => ({ ...d, [field]: value }))
    // A field's error clears as soon as the person changes it: leaving a red
    // sentence under a field they have just fixed is the same lie as a stale
    // count. The summary keeps its own copy and re-appears on the next submit.
    if (errors[field as DraftField]) {
      setErrors(e => {
        const next = { ...e }
        delete next[field as DraftField]
        return next
      })
    }
  }

  const controlFor = (field: DraftField) =>
    field === 'subject' ? subjectRef : field === 'kind' ? kindRef : briefRef

  const submit = useCallback(async () => {
    const found = validateDraft(draft)
    setErrors(found)
    if (!isValidDraft(draft)) {
      setShowSummary(true)
      setScreen({ kind: 'form' })
      // Focus the summary, not the first field: a screen reader user hears what
      // is wrong before being dropped into one input, and the summary's links go
      // to each field. The DOM update must land first, hence the microtask.
      queueMicrotask(() => summaryRef.current?.focus())
      return
    }
    setShowSummary(false)

    const body = buildSubmitBody(draft)
    // The token is minted *before* the call and remembered, so a failure — or a
    // tab that dies mid-request — retries the same submission instead of
    // creating a second request.
    const key = idempotencyKeyFor(teamId, submissionPayload(teamId, body))
    setScreen({ kind: 'sending' })
    try {
      const answer = await withTimeout(
        requestApi.submit(teamId, body, key),
        'Sending your request',
      )
      // Answered: the draft is over, so the next send is a new request and needs
      // its own key.
      clearInflightSubmission(teamId)
      setScreen({ kind: 'sent', answer })
      window.scrollTo?.(0, 0)
    } catch (e) {
      // The draft is untouched: every word is still in the form, and the token
      // is still in storage for the retry.
      setScreen({ kind: 'failed', notice: submissionNotice(e, teamName) })
    }
  }, [draft, teamId, teamName])

  // A plain function rather than a useCallback: React Compiler owns memoization
  // in this repo and refuses to preserve a manually memoized handler that closes
  // over a ref (`preserve-manual-memoization`).
  const reset = () => {
    setDraft(EMPTY_DRAFT)
    setErrors({})
    setShowSummary(false)
    setScreen({ kind: 'form' })
    onSendAnother?.()
    queueMicrotask(() => subjectRef.current?.focus())
  }

  const navNode = useMemo(() => (nav && nav.length > 0
    ? nav.map(item => (
      <button
        key={item.label}
        type="button"
        onClick={item.onClick}
        aria-current={item.current ? 'page' : undefined}
        className={requesterNavItemClass(item.current)}
      >
        {item.label}
      </button>
    ))
    : undefined), [nav])

  return (
    <RequesterChrome
      teamName={teamName}
      me={me}
      nav={navNode}
      banner={banner}
      onSignOut={onSignOut}
    >
      {screen.kind === 'sent'
        ? (
          <SuccessScreen
            answer={screen.answer}
            teamName={teamName}
            me={me}
            onOpenRequest={onOpenRequest}
            onViewRequests={onViewRequests}
            onSendAnother={onSendAnother ? reset : undefined}
          />
        )
        : (
          <div className="mx-auto w-full" style={{ maxWidth: 760 }}>
            <h1 className="m-0 text-[24px] font-semibold tracking-[-0.01em] text-ink">New request</h1>
            <p className="m-0 mb-6 mt-1.5 max-w-[62ch] text-[13.5px] leading-relaxed text-ink-2">
              Tell {teamName} what you need. They'll reply on this page — usually within one business day. You don't
              need an account on their systems, and they can't see anything of yours but this request.
            </p>

            {/* The failed send. Above the form, and never instead of it. */}
            {screen.kind === 'failed' && (
              <FailureNotice
                notice={screen.notice}
                onRetry={() => void submit()}
                onDismiss={() => setScreen({ kind: 'form' })}
              />
            )}

            {/* The refused submit: every bad field, in form order, each a link to it. */}
            {showSummary && Object.keys(errors).length > 0 && (
              <div
                ref={summaryRef}
                role="alert"
                tabIndex={-1}
                data-testid="submit-error-summary"
                className="mb-4 border px-3.5 py-3"
                style={{
                  borderRadius: 'var(--radius)',
                  borderColor: 'var(--danger)',
                  borderLeft: '5px solid var(--danger)',
                  background: 'var(--surface)',
                }}
              >
                <b className="block text-[13px] font-semibold text-ink">
                  {Object.keys(errors).length === 1 ? '1 thing to fix before sending' : `${Object.keys(errors).length} things to fix before sending`}
                </b>
                <ul className="mb-0 mt-1.5 pl-[18px] text-[12.5px] text-ink-2">
                  {DRAFT_FIELD_ORDER.filter(f => errors[f]).map(field => (
                    <li key={field}>
                      <button
                        type="button"
                        onClick={() => controlFor(field).current?.focus()}
                        className="cursor-pointer text-left underline"
                        style={{ color: 'var(--danger-ink)' }}
                      >
                        {errors[field]}
                      </button>
                    </li>
                  ))}
                </ul>
              </div>
            )}

            <form
              noValidate
              onSubmit={e => { e.preventDefault(); void submit() }}
              aria-busy={screen.kind === 'sending'}
            >
              {/* ---- What you need ---- */}
              <section
                aria-labelledby="h-what"
                className="mb-4 border bg-surface px-4 pb-1.5 pt-4"
                style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
              >
                <h2 id="h-what" className="amp-label m-0 mb-1">What you need</h2>
                <p className="m-0 mb-3.5 text-[12.5px] leading-relaxed text-ink-3">
                  The short version first — the desk reads this to route your request.
                </p>

                <Field
                  label="Subject"
                  htmlFor={fieldControlId('subject')}
                  error={errors.subject}
                >
                  <input
                    ref={subjectRef}
                    id={fieldControlId('subject')}
                    type="text"
                    value={draft.subject}
                    onChange={e => set('subject', e.target.value)}
                    maxLength={MAX_SUBJECT_LEN + 400}
                    placeholder='e.g. Export of invoice history missing October'
                    aria-invalid={!!errors.subject}
                    aria-describedby={errors.subject ? `${fieldControlId('subject')}-err` : undefined}
                    className="amp-input text-base sm:text-[13px]"
                  />
                  <Counter value={draft.subject} max={MAX_SUBJECT_LEN} />
                </Field>

                <div className="grid gap-3.5 sm:grid-cols-2">
                  <Field
                    label="Kind of request"
                    htmlFor={fieldControlId('kind')}
                    error={errors.kind}
                  >
                    <select
                      ref={kindRef}
                      id={fieldControlId('kind')}
                      value={draft.kind}
                      onChange={e => set('kind', e.target.value as SubmitDraft['kind'])}
                      aria-invalid={!!errors.kind}
                      aria-describedby={errors.kind ? `${fieldControlId('kind')}-err` : undefined}
                      className="amp-input text-base sm:text-[13px]"
                    >
                      <option value="">Choose…</option>
                      {REQUEST_KINDS.map(k => <option key={k.value} value={k.value}>{k.label}</option>)}
                    </select>
                  </Field>

                  <div className="mb-4">
                    <span id="lbl-urgency" className="mb-1.5 block font-label text-[11px] font-semibold uppercase tracking-[0.14em] text-ink-2">
                      How much is this blocking you?
                    </span>
                    <div
                      role="group"
                      aria-labelledby="lbl-urgency"
                      className="flex w-max max-w-full overflow-hidden border"
                      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
                    >
                      {REQUEST_URGENCIES.map(u => {
                        const active = draft.urgency === u.value
                        return (
                          <button
                            key={u.value}
                            type="button"
                            onClick={() => set('urgency', u.value)}
                            aria-pressed={active}
                            className={`flex-1 cursor-pointer whitespace-nowrap border-r px-3 py-2 text-[12.5px] last:border-r-0 ${
                              active ? 'bg-ink font-medium text-on-ink' : 'bg-surface text-ink-2 hover:text-ink'
                            }`}
                            style={{ borderColor: 'var(--line)', minHeight: 36 }}
                          >
                            {u.label}
                          </button>
                        )
                      })}
                    </div>
                    <p className="m-0 mt-1.5 text-[12px] leading-relaxed text-ink-3">
                      The desk triages on this, so be honest — "work stopped" pages someone.
                    </p>
                  </div>
                </div>

                <Field
                  label="What happened / what you need"
                  htmlFor={fieldControlId('brief')}
                  error={errors.brief}
                >
                  <textarea
                    ref={briefRef}
                    id={fieldControlId('brief')}
                    value={draft.brief}
                    onChange={e => set('brief', e.target.value)}
                    rows={5}
                    placeholder="What were you trying to do, what did you expect, and what did you get instead? Include dates, order or invoice numbers, and the names of anyone else affected."
                    aria-invalid={!!errors.brief}
                    aria-describedby={`${fieldControlId('brief')}-hint${errors.brief ? ` ${fieldControlId('brief')}-err` : ''}`}
                    className="amp-input text-base leading-relaxed sm:text-[13px]"
                    style={{ minHeight: 120 }}
                  />
                  <Counter value={draft.brief} max={MAX_BRIEF_LEN} />
                  <p id={`${fieldControlId('brief')}-hint`} className="m-0 mt-1.5 text-[12px] leading-relaxed text-ink-3">
                    Don't paste passwords, API keys or full card numbers. The desk will never ask for them here.
                  </p>
                </Field>

                <Field
                  label='What "done" looks like for you'
                  htmlFor="in-outcome"
                  optional
                  hint='This becomes the bar the desk delivers against — and what you are asked to confirm at the end.'
                >
                  <textarea
                    id="in-outcome"
                    value={draft.outcome}
                    onChange={e => set('outcome', e.target.value)}
                    rows={3}
                    placeholder="e.g. A corrected October CSV we can re-import, and a note on why the totals shifted."
                    aria-describedby="in-outcome-hint"
                    className="amp-input text-base leading-relaxed sm:text-[13px]"
                    style={{ minHeight: 80 }}
                  />
                </Field>
              </section>

              {/* ---- Attachments ---- */}
              <section
                aria-labelledby="h-files"
                className="mb-4 border bg-surface px-4 py-4"
                style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
              >
                <h2 id="h-files" className="amp-label m-0 mb-1">Attachments</h2>
                <p className="m-0 max-w-[62ch] text-[12.5px] leading-relaxed text-ink-2">
                  Files can’t be sent with a new request from this page yet. Send it, then the desk can ask you for
                  anything it needs and you can reply on the request’s page. If the detail already exists somewhere
                  you can share — a link, an order id, a reference from the desk’s own systems — put it in the
                  description above.
                </p>
              </section>

              {/* ---- Getting back to you ---- */}
              <section
                aria-labelledby="h-contact"
                className="mb-4 border bg-surface px-4 pb-1.5 pt-4"
                style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
              >
                <h2 id="h-contact" className="amp-label m-0 mb-1">Getting back to you</h2>
                <p className="m-0 mb-3.5 text-[12.5px] leading-relaxed text-ink-3">
                  Taken from your sign-in — the desk replies to this address, and this request is visible only from it.
                </p>
                <div className="grid gap-3.5 sm:grid-cols-2">
                  <Field label="Your name" htmlFor="in-name">
                    <input
                      id="in-name"
                      type="text"
                      readOnly
                      aria-readonly="true"
                      value={me.name ?? ''}
                      placeholder="—"
                      className="amp-input text-base sm:text-[13px]"
                      style={{ background: 'var(--surface-3)' }}
                    />
                  </Field>
                  <Field
                    label="Email"
                    htmlFor="in-email"
                    hint="To change this, update your sign-in — the snapshot on the request never changes after sending."
                  >
                    <input
                      id="in-email"
                      type="email"
                      readOnly
                      aria-readonly="true"
                      value={me.email}
                      className="amp-input text-base sm:text-[13px]"
                      style={{ background: 'var(--surface-3)' }}
                    />
                  </Field>
                </div>
              </section>

              {/* The action bar. Below `sm` it is sticky to the bottom edge with a
                  rule and the page's own background, and the primary button is
                  full width — the mockup's 390 px rule. On a form this long the
                  Send button is otherwise two screens below the last field a
                  person edited. `-mx-1 -mb-4 px-1 pb-3` lets it meet the
                  viewport edge instead of floating inside the panel. */}
              <div
                className="sticky bottom-0 z-10 -mx-1 mt-5 flex flex-col gap-2.5 border-t border-line bg-bg px-1 pb-3 pt-2.5 sm:static sm:mx-0 sm:flex-row sm:items-center sm:border-t-0 sm:px-0 sm:pb-0 sm:pt-0"
              >
                <button
                  type="submit"
                  className="amp-btn amp-btn-primary w-full sm:w-auto"
                  disabled={screen.kind === 'sending'}
                  style={{ minHeight: 44 }}
                >
                  {screen.kind === 'sending'
                    ? <><Loader2 size={13} className="animate-spin" aria-hidden="true" />Sending…</>
                    : 'Send request'}
                </button>
                <span className="text-center text-[11.5px] leading-snug text-ink-3 sm:ml-auto sm:max-w-[38ch] sm:text-right">
                  Sending goes straight to the desk — nothing here starts work on its own.
                </span>
              </div>
            </form>
          </div>
        )}
    </RequesterChrome>
  )
}

/** One labelled field, its error and its optional hint. */
function Field({ label, htmlFor, error, hint, optional, children }: {
  label: string
  htmlFor: string
  error?: string
  hint?: string
  optional?: boolean
  children: React.ReactNode
}) {
  return (
    <div className="mb-4" data-invalid={error ? 'true' : undefined}>
      <label htmlFor={htmlFor} className="mb-1.5 block font-label text-[11px] font-semibold uppercase tracking-[0.14em] text-ink-2">
        {label}
        {optional && <span className="ml-1.5 font-medium tracking-[0.1em] text-ink-3">Optional</span>}
      </label>
      {children}
      {error && (
        <p id={`${htmlFor}-err`} className="m-0 mt-1.5 text-[12px] leading-snug" style={{ color: 'var(--danger-ink)' }}>
          {error}
        </p>
      )}
      {hint && !error && (
        <p id={`${htmlFor}-hint`} className="m-0 mt-1.5 text-[12px] leading-relaxed text-ink-3">{hint}</p>
      )}
    </div>
  )
}

/**
 * How much of a bound is used, and only once it matters.
 *
 * Silence until 80%: a counter that counts every keystroke draws the eye to a
 * limit almost nobody reaches. Past it the count is announced politely, so a
 * screen reader user is told before the text is refused rather than after.
 */
function Counter({ value, max }: { value: string; max: number }) {
  const n = charCount(value.trim())
  if (n < max * 0.8) return null
  const over = n > max
  return (
    <p
      className="m-0 mt-1 text-[11px] tabular-nums"
      style={{ color: over ? 'var(--danger-ink)' : 'var(--ink-3)' }}
      aria-live="polite"
    >
      {n} of {max} characters{over ? ' — too long to send' : ''}
    </p>
  )
}

/**
 * A failed send: what happened, whether anything was written, and a Retry that
 * re-sends the same submission (the same `Idempotency-Key`), not a new one.
 */
function FailureNotice({ notice, onRetry, onDismiss }: {
  notice: SubmissionNotice
  onRetry: () => void
  onDismiss: () => void
}) {
  return (
    <div
      role="alert"
      data-testid="submit-failure"
      className="mb-4 flex items-start gap-3 border px-3.5 py-3"
      style={{
        borderRadius: 'var(--radius)',
        borderColor: 'var(--danger)',
        borderLeft: '5px solid var(--danger)',
        background: 'var(--surface)',
      }}
    >
      <AlertTriangle size={15} className="mt-0.5 flex-shrink-0" style={{ color: 'var(--danger)' }} aria-hidden="true" />
      <div className="min-w-0 flex-1">
        <b className="block text-[13px] font-semibold text-ink">{notice.title}</b>
        <p className="m-0 mt-1 text-[12.5px] leading-relaxed text-ink-2">{notice.body}</p>
        <div className="mt-2.5 flex flex-wrap gap-2">
          <button type="button" onClick={onRetry} className="amp-btn amp-btn-sm amp-btn-primary" style={{ minHeight: 32 }}>
            <RotateCw size={11} aria-hidden="true" />Try again
          </button>
          <button type="button" onClick={onDismiss} className="amp-btn amp-btn-sm amp-btn-ghost" style={{ minHeight: 32 }}>
            Keep editing
          </button>
        </div>
      </div>
    </div>
  )
}

/**
 * The success screen: the reference, what happens next, and the one thing a
 * person actually has to do (keep the reference).
 *
 * The reference is rendered with `requestRef` so it reads as `REQ-…` and with
 * the stored token's case intact — uppercasing it would render a reference
 * nobody could paste back (base64url is case-sensitive, and the server matches
 * the stored value). The Copy button copies the same string that is displayed.
 */
function SuccessScreen({ answer, teamName, me, onOpenRequest, onViewRequests, onSendAnother }: {
  answer: RequestSubmissionAnswer
  teamName: string
  me: RequesterIdentity
  onOpenRequest?: (publicRef: string) => void
  onViewRequests?: () => void
  onSendAnother?: () => void
}) {
  const reference = requestRef(answer.request.public_ref)
  const [copied, setCopied] = useState<'idle' | 'copied' | 'failed'>('idle')

  // The copy affordance clears itself, so a second copy is possible without a
  // stray "Copied" from ten minutes ago.
  useEffect(() => {
    if (copied === 'idle') return
    const t = setTimeout(() => setCopied('idle'), 4000)
    return () => clearTimeout(t)
  }, [copied])

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(reference)
      setCopied('copied')
    } catch {
      // No clipboard (an insecure origin, or a browser that refuses without a
      // gesture chain): the reference is on screen and selectable, so say so
      // rather than claiming it was copied.
      setCopied('failed')
    }
  }

  return (
    <div className="mx-auto w-full" style={{ maxWidth: 760 }}>
      <div
        className="mb-4 border bg-surface px-6 py-7 text-center"
        style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)', borderTop: '4px solid var(--success)' }}
      >
        <span
          className="inline-flex items-center gap-1 border px-2 py-1 font-label text-[10px] font-semibold uppercase tracking-[0.14em]"
          style={{
            borderRadius: 2,
            color: 'var(--success-ink)',
            borderColor: 'color-mix(in srgb, var(--success) 40%, var(--surface))',
            background: 'color-mix(in srgb, var(--success) 7%, var(--surface))',
          }}
        >
          <Check size={11} aria-hidden="true" />Request received
        </span>

        <h1 className="m-0 mt-3 text-[22px] font-semibold tracking-[-0.01em] text-ink">
          Thanks — {teamName} has your request.
        </h1>
        <p className="mx-auto mt-2 max-w-[52ch] text-[13.5px] leading-relaxed text-ink-2">
          Your reference is all you need to check on it. Keep it somewhere safe.
        </p>

        <div
          className="mx-auto my-3.5 inline-flex flex-wrap items-center justify-center gap-2.5 border px-3.5 py-2.5"
          style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line-str)', background: 'var(--surface-2)' }}
        >
          <span className="amp-num select-all text-[17px] font-semibold tracking-[0.04em] text-ink" data-testid="success-ref">
            {reference}
          </span>
          <button
            type="button"
            onClick={() => void copy()}
            // The name is stable: a button that renames itself to "Copied"
            // loses its own label for anyone using voice or screen-reader
            // navigation, and the outcome is already announced below.
            aria-label="Copy the reference"
            className="amp-btn amp-btn-sm"
            style={{ minHeight: 32 }}
          >
            <Copy size={11} aria-hidden="true" />{copied === 'copied' ? 'Copied' : 'Copy'}
          </button>        </div>
        <p role="status" className="m-0 min-h-[1em] text-[11.5px] text-ink-3">
          {copied === 'copied' && 'Reference copied.'}
          {copied === 'failed' && 'Your browser refused the clipboard — select the reference above and copy it.'}
        </p>

        <div className="mx-auto mt-5 max-w-[52ch] text-left">
          <span className="amp-label">What happens next</span>
          <ol className="mb-0 mt-2.5 pl-5">
            <li className="mb-2 text-[13px] leading-relaxed text-ink-2">
              <b className="font-semibold text-ink">Triage.</b> Someone from {teamName} reviews your request — usually
              within one business day.
            </li>
            <li className="mb-2 text-[13px] leading-relaxed text-ink-2">
              <b className="font-semibold text-ink">Questions?</b> If they need more detail, they'll ask and it shows up
              on your request — you reply right there.
            </li>
            <li className="mb-2 text-[13px] leading-relaxed text-ink-2">
              <b className="font-semibold text-ink">Delivery.</b> When the work is ready, you'll see it under{' '}
              <i>Your requests</i>, and you decide whether it answers what you asked for.
            </li>
          </ol>

          {(onOpenRequest || onViewRequests || onSendAnother) && (
            <div className="mt-4 flex flex-wrap gap-2">
              {onOpenRequest && (
                <button
                  type="button"
                  onClick={() => onOpenRequest(answer.request.public_ref)}
                  className="amp-btn amp-btn-primary w-full sm:w-auto"
                  style={{ minHeight: 44 }}
                >
                  Track this request
                </button>
              )}
              {onViewRequests && (
                <button type="button" onClick={onViewRequests} className="amp-btn w-full sm:w-auto" style={{ minHeight: 44 }}>
                  All your requests
                </button>
              )}
              {onSendAnother && (
                <button type="button" onClick={onSendAnother} className="amp-btn amp-btn-ghost w-full sm:w-auto" style={{ minHeight: 44 }}>
                  Send another request
                </button>
              )}
            </div>
          )}
        </div>
      </div>

      <p className="m-0 flex items-start gap-2 text-[11.5px] leading-relaxed text-ink-3">
        <Lock size={13} className="mt-0.5 flex-shrink-0" aria-hidden="true" />
        <span>
          This request is visible only from {me.email}. Nothing about it was emailed to you — keep the reference above,
          and check the request from <i>Your requests</i>.
        </span>
      </p>
    </div>
  )
}
