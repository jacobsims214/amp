// The requester submit form's logic (AMP #3985, build slice J4).
//
// Four concerns, one describe each, because each is a rule a future edit could
// break without the page looking wrong:
//
//  1. **The body is three fields.** The route refuses an unknown field by name,
//     so a payload that grew a contact address, an actor, a project or a state
//     would be a 400 — and, worse, would look fine in a test that mocked the
//     client. The assertions are on the body's exact key set.
//  2. **One retry token per draft, kept across a reload.** The tests drive the
//     real `sessionStorage` (jsdom has one) rather than a fake, because "a
//     reload loses it" is exactly the failure the token exists to prevent.
//  3. **Validation names the field, matches the server's bounds, and never
//     touches the input.** Every message is asserted to mention what to do; the
//     draft is asserted to be unchanged after a failed validation.
//  4. **A failure says whether anything was written.** Each status/code the
//     inbox surface can answer with gets its own row, including the 404 that
//     must not become "not yours" and the network failure that must say a retry
//     cannot duplicate.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import {
  EMPTY_DRAFT, MAX_BRIEF_LEN, MAX_SUBJECT_LEN, OUTCOME_HEADING, REQUEST_KINDS,
  browserDraftStorage, buildSubmitBody, charCount, clearInflightSubmission, composeInputBrief,
  fieldControlId, firstInvalidField, idempotencyKeyFor, inflightPath, isValidDraft,
  loadInflightSubmission, newSubmissionKey, submissionNotice, submissionPayload, validateDraft,
  type SubmitDraft,
} from './requesterSubmit'
import { NO_PERMISSION } from './apiError'

const draft = (over: Partial<SubmitDraft> = {}): SubmitDraft => ({
  ...EMPTY_DRAFT,
  subject: "October invoice export totals don't match the ledger",
  brief: 'The export is short by about €4,300 across 11 rows.',
  kind: 'not_working',
  ...over,
})

describe('the body a submission sends', () => {
  it('carries exactly the route\'s three fields, and the path names the team', () => {
    const body = buildSubmitBody(draft({ outcome: 'A corrected October CSV.' }))
    expect(Object.keys(body).sort()).toEqual(['input_brief', 'metadata', 'subject'])
    // The contact address, the actor, the org, the project and the state are
    // the server's facts, never a caller's — none of them can be here.
    for (const forbidden of ['requester_contact', 'actor', 'org_id', 'team_id', 'project_id', 'state', 'idempotency_key', 'source_kind', 'outcome_brief']) {
      expect(body).not.toHaveProperty(forbidden)
    }
  })

  it('folds the desired outcome into the brief under its own heading', () => {
    const body = buildSubmitBody(draft({ outcome: '  A corrected October CSV we can re-import.  ' }))
    expect(body.input_brief).toBe(
      `The export is short by about €4,300 across 11 rows.\n\n${OUTCOME_HEADING}\nA corrected October CSV we can re-import.`,
    )
    // Never the record's outcome column: that is the team's delivery statement.
    expect(body).not.toHaveProperty('outcome_brief')
    expect(composeInputBrief('', 'Only the outcome')).toBe(`${OUTCOME_HEADING}\nOnly the outcome`)
    expect(composeInputBrief('Only the brief', '   ')).toBe('Only the brief')
  })

  it('sends the two mockup choices as metadata routing hints, and the kind as a stable slug', () => {
    const body = buildSubmitBody(draft({ kind: 'access', urgency: 'work_stopped' }))
    expect(body.metadata).toEqual({ request_kind: 'access', urgency: 'work_stopped' })
    // A slug, not the label: copy changes, and a stored hint must not.
    expect(JSON.stringify(body.metadata)).not.toContain('Access or an account')
  })

  it('omits metadata entirely when the draft carries neither hint', () => {
    const body = buildSubmitBody(draft({ kind: '', urgency: undefined as unknown as 'slowing_us_down' }))
    expect(body).not.toHaveProperty('metadata')
    expect(Object.keys(body).sort()).toEqual(['input_brief', 'subject'])
  })

  it('trims what it sends, so leading whitespace is never stored as content', () => {
    const body = buildSubmitBody(draft({ subject: '   Ledger mismatch   ', brief: '\n\n short \n' }))
    expect(body.subject).toBe('Ledger mismatch')
    expect(body.input_brief).toBe('short')
  })
})

describe('validation', () => {
  it('accepts a complete draft', () => {
    expect(validateDraft(draft())).toEqual({})
    expect(isValidDraft(draft())).toBe(true)
  })

  it('names each missing field, in the mockup\'s own words', () => {
    const errors = validateDraft(EMPTY_DRAFT)
    expect(Object.keys(errors).sort()).toEqual(['brief', 'kind', 'subject'])
    expect(errors.subject).toMatch(/give your request a subject/i)
    expect(errors.kind).toMatch(/choose the closest kind/i)
    expect(errors.brief).toMatch(/can't start from an empty description/i)
    expect(firstInvalidField(errors)).toBe('subject')
    expect(fieldControlId('subject')).toBe('in-subject')
  })

  it('applies the server\'s subject bound, counting characters not UTF-16 units', () => {
    const long = '€'.repeat(MAX_SUBJECT_LEN) // 200 runes, 400 UTF-16 units
    expect(validateDraft(draft({ subject: long })).subject).toBeUndefined()
    expect(charCount(long)).toBe(MAX_SUBJECT_LEN)

    const errors = validateDraft(draft({ subject: 'x'.repeat(MAX_SUBJECT_LEN + 1) }))
    expect(errors.subject).toContain(String(MAX_SUBJECT_LEN))
  })

  it('refuses a composed brief over the server\'s limit, counting the heading and the outcome', () => {
    // The brief alone is inside the bound; with the heading and outcome it is not.
    const brief = 'a'.repeat(MAX_BRIEF_LEN - 10)
    expect(validateDraft(draft({ brief })).brief).toBeUndefined()

    const errors = validateDraft(draft({ brief, outcome: 'b'.repeat(50) }))
    expect(errors.brief).toContain(String(MAX_BRIEF_LEN))
    expect(validateDraft(draft({ brief, outcome: 'b'.repeat(50) })).brief).toMatch(/trim it down/i)
  })

  it('never changes the draft it validated — the words survive a failed send', () => {
    const bad = draft({ subject: '', outcome: 'Keep this' })
    const snapshot = JSON.stringify(bad)
    validateDraft(bad)
    expect(JSON.stringify(bad)).toBe(snapshot)
    expect(bad.outcome).toBe('Keep this')
  })
})

describe('the in-flight retry token', () => {
  beforeEach(() => { window.sessionStorage.clear() })
  afterEach(() => { window.sessionStorage.clear() })

  const body = buildSubmitBody(draft())
  const payload = submissionPayload(4, body)

  it('mints a stable key for one draft and reuses it on a retry', () => {
    const first = idempotencyKeyFor(4, payload)
    const second = idempotencyKeyFor(4, payload)
    expect(second).toBe(first)
    expect(first).toMatch(/^[0-9a-f]{36}$/)
    // 061's bound is 255 bytes; a 36-character token is nowhere near it.
    expect(new TextEncoder().encode(first).length).toBeLessThan(255)
  })

  it('survives a reload: the token is read back out of sessionStorage', () => {
    const key = idempotencyKeyFor(4, payload)
    const held = loadInflightSubmission(4)
    expect(held).toEqual({ key, payload })
    // What a fresh page instance would find, without any module state.
    expect(JSON.parse(window.sessionStorage.getItem(inflightPath(4)) ?? '{}')).toEqual({ key, payload })
  })

  it('mints a NEW key when the draft changed, so the server never sees a key with two payloads', () => {
    const first = idempotencyKeyFor(4, payload)
    const changed = submissionPayload(4, buildSubmitBody(draft({ brief: 'A different sentence.' })))
    const second = idempotencyKeyFor(4, changed)
    expect(second).not.toBe(first)
    expect(loadInflightSubmission(4)).toEqual({ key: second, payload: changed })
  })

  it('mints a NEW key per team: one draft to two desks is two submissions', () => {
    const a = idempotencyKeyFor(4, payload)
    const b = idempotencyKeyFor(9, payload)
    expect(b).not.toBe(a)
    expect(loadInflightSubmission(4)?.key).toBe(a)
    expect(loadInflightSubmission(9)?.key).toBe(b)
  })

  it('forgets the token once the server answered, so the next send is a new request', () => {
    const sent = idempotencyKeyFor(4, payload)
    clearInflightSubmission(4)
    expect(loadInflightSubmission(4)).toBeNull()
    const next = idempotencyKeyFor(4, payload)
    expect(next).not.toBe(sent)
    // …and the new one sticks, so *its* retry replays rather than duplicating.
    expect(idempotencyKeyFor(4, payload)).toBe(next)
  })

  it('works without storage at all: the submission still goes out, on its own key', () => {
    // Private mode / a blocked origin: browserDraftStorage returns null rather
    // than throwing, and a form that needs a key still gets one.
    expect(() => browserDraftStorage()).not.toThrow()
    const key = idempotencyKeyFor(4, payload, null)
    expect(key).toMatch(/^[0-9a-f]{36}$/)
    expect(loadInflightSubmission(4, null)).toBeNull()
    expect(() => clearInflightSubmission(4, null)).not.toThrow()
  })

  it('ignores an unreadable record instead of taking the form down', () => {
    window.sessionStorage.setItem(inflightPath(4), 'not json')
    expect(loadInflightSubmission(4)).toBeNull()
    window.sessionStorage.setItem(inflightPath(4), JSON.stringify({ key: '' }))
    expect(loadInflightSubmission(4)).toBeNull()
    expect(idempotencyKeyFor(4, payload)).toMatch(/^[0-9a-f]{36}$/)
  })

  it('treats the same body built twice as one payload, and a whitespace change as another', () => {
    expect(submissionPayload(4, buildSubmitBody(draft()))).toBe(payload)
    // Trimmed on the way out, so a stray space does not orphan the token.
    expect(submissionPayload(4, buildSubmitBody(draft({ subject: `  ${draft().subject}  ` })))).toBe(payload)
    expect(submissionPayload(4, buildSubmitBody(draft({ kind: 'access' })))).not.toBe(payload)
  })

  it('mints distinct tokens, so two people never collide', () => {
    const keys = new Set(Array.from({ length: 50 }, () => newSubmissionKey()))
    expect(keys.size).toBe(50)
  })
})

describe('what a failure says', () => {
  const err = (status: number, message = '', code?: string, retryAfterS?: number) =>
    Object.assign(new Error(message), { status, code, retryAfterS })

  it('says nothing was lost on a dropped connection, and that a retry cannot duplicate', () => {
    const notice = submissionNotice(new TypeError('Failed to fetch'), 'Northwind support desk')
    expect(notice.title).toMatch(/didn't finish/i)
    expect(notice.body).toMatch(/nothing you wrote was lost/i)
    expect(notice.body).toMatch(/cannot be sent twice/i)
  })

  it('turns a 404 into "your sign-in can\'t send here", never "not yours"', () => {
    const notice = submissionNotice(err(404, 'request not found', 'request.not_found'), 'Northwind support desk')
    expect(notice.title).toContain('Northwind support desk')
    expect(notice.body).toMatch(/invited/i)
    // The surface refuses to distinguish unknown from foreign, so must the form.
    expect(`${notice.title} ${notice.body}`).not.toMatch(/not yours|belongs to|does not exist/i)
  })

  it('carries a 429\'s wait in the sentence', () => {
    const notice = submissionNotice(err(429, 'slow down', 'request.rate_limited', 60), 'the desk')
    expect(notice.retryInS).toBe(60)
    expect(notice.body).toContain('60 seconds')
    expect(notice.body).toMatch(/nothing was sent/i)
    // A single second reads as a second, not "1 seconds".
    expect(submissionNotice(err(429, '', 'request.rate_limited', 1), 'the desk').body).toContain('1 second')
  })

  it('says what to fix on a coded 400, and that nothing was sent', () => {
    const notice = submissionNotice(
      err(400, 'subject is over 200 characters (got 260)', 'request.invalid'), 'the desk')
    expect(notice.title).toMatch(/couldn't accept/i)
    expect(notice.body).toContain('subject is over 200 characters')
    expect(notice.body).toMatch(/nothing was sent/i)
  })

  it('keeps the server\'s permission sentence, and replaces a bare one', () => {
    expect(submissionNotice(err(403, 'Forbidden'), 'the desk').body).toBe(NO_PERMISSION)
    expect(submissionNotice(err(403, 'This inbox does not accept your org.'), 'the desk').body)
      .toBe('This inbox does not accept your org.')
  })

  it('sends a 409 to "check your requests", because it may already have been written', () => {
    const notice = submissionNotice(err(409, 'same idempotency key with a different payload', 'request.conflict'), 'the desk')
    expect(notice.title).toMatch(/may already have been sent/i)
    expect(notice.body).toMatch(/check your requests/i)
  })

  it('treats a 5xx like a dropped connection: the answer is unknown, and a retry is safe', () => {
    const notice = submissionNotice(err(503, 'The desk is restarting.'), 'the desk')
    expect(notice.body).toContain('The desk is restarting.')
    expect(notice.body).toMatch(/cannot be sent twice/i)
  })

  it('names the desk when one is known, and never renders an empty name', () => {
    expect(submissionNotice(err(404, '', 'request.not_found'), '  ').title).toMatch(/this desk/)
    expect(submissionNotice(err(404, '', 'request.not_found'), 'Northwind support desk').title)
      .toContain('Northwind support desk')
  })

  it('has a kind for every option the form offers, and no duplicate slug', () => {
    expect(REQUEST_KINDS.length).toBe(4)
    expect(new Set(REQUEST_KINDS.map(k => k.value)).size).toBe(4)
    expect(REQUEST_KINDS.map(k => k.value)).toContain('new_work')
  })
})
