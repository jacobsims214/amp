/**
 * The requester's tracking screen: what each lifecycle state means for them, and
 * the exact words to use when one of their own acts is refused (AMP #3987, build
 * slice J6 of `discoveries/team-inbox-approved-build-slices.md`; approved mockup
 * artifact 38 v1, the "Request status" and "No access" screens).
 *
 * Everything here is either a *reading* of a value the server already decided or
 * a sentence about a refusal the server already made. Nothing derives a state, a
 * milestone or a permission: the projection (`domain.RequesterRequestView`) is a
 * whitelist the server builds, `state_word` is the server's own word for the
 * state, and `milestones[].done` is gated server-side against the lifecycle
 * (`RequesterMilestoneDoneAllowed`) so a closing ticket can never make a step
 * look done.
 *
 * ## Why the track is a separate vocabulary from the state
 *
 * The projection carries nine lifecycle states and five milestone keys. The
 * requester's screen has fewer shapes than that: `new`/`triaging`/`accepted`/
 * `in_progress` are all "we're on it", and `rejected`/`duplicate` are both
 * "closed" (which is exactly what `RequesterStateWord` says for them). Collapsing
 * them here — in one function, with the mapping written out — is what stops a
 * component from inventing a sixth arm, or from showing a "rejected" banner on a
 * request that was folded into a duplicate.
 *
 * ## Why the access states are NOT derived from an error
 *
 * Three of the mockup's states are access states: "we can't find that request",
 * "you've been invited", "your sending access was removed". The first is a real
 * server answer (the surface's single 404). The other two are **not**, on
 * purpose: `authz.TeamSubmit` answers the same not-found for a person who was
 * never invited and a person whose grant was revoked, so that a guessed address
 * cannot be used to learn whether an inbox exists or whether someone used to have
 * access. A screen that inferred "revoked" from a 404 would rebuild exactly the
 * oracle the server refuses to be. So this module never maps an error onto an
 * access state other than not-found: `invite_pending` and `grant_revoked` come
 * from the *entry point* (an invite link knows the team and the address; a
 * submission that was refused knows the attempt), never from a read.
 */
import type { RequestStatus, RequesterOutputStatus } from '../types'

/**
 * The bound on one answer. It is the server's, not a UI guess: an answer is
 * stored as a `question`/`answer` comment, and `domain.RequestComment.Validate`
 * bounds a comment body with `NormalizeRequestBrief` → `MaxRequestBriefLen`
 * (20000 characters). Checking it here means a person is told in the box rather
 * than after a round trip.
 */
export const MAX_ANSWER_LEN = 20000

/** The five shapes the tracking screen has, collapsed from the nine states. */
export type RequesterTrack = 'progress' | 'needs_reply' | 'delivered' | 'accepted' | 'closed'

/**
 * Which arm of the screen a state renders.
 *
 * `needs_reply` is its own arm because it is the only state that asks the
 * requester for something — the answer box exists there and nowhere else, which
 * matches the lifecycle: an answer is only legal out of `needs_information`
 * (`planRequestAnswer`).
 */
export function requesterTrack(state: RequestStatus): RequesterTrack {
  switch (state) {
    case 'needs_information':
      return 'needs_reply'
    case 'delivered':
      return 'delivered'
    case 'requester_accepted':
      return 'accepted'
    case 'rejected':
    case 'duplicate':
      return 'closed'
    default:
      // new, triaging, accepted, in_progress — and anything a future schema adds
      // that this UI does not know: "we're on it" is the safe reading of an
      // unknown working state, and it promises nothing.
      return 'progress'
  }
}

/** The tag tone for a track: the mockup's four colours, one per arm. */
export type RequesterTagTone = 'prog' | 'flag' | 'ok' | 'done'

export function requesterTagTone(track: RequesterTrack): RequesterTagTone {
  switch (track) {
    case 'needs_reply':
      return 'flag'
    case 'delivered':
      return 'ok'
    case 'accepted':
    case 'closed':
      return 'done'
    default:
      return 'prog'
  }
}

/** The CSS colour of a track's tag and its stepper marker. */
export function requesterTrackVar(track: RequesterTrack): string {
  switch (track) {
    case 'needs_reply':
      return 'var(--st-flag)'
    case 'delivered':
      return 'var(--success)'
    case 'accepted':
      return 'var(--ink)'
    case 'closed':
      return 'var(--st-wait)'
    default:
      return 'var(--st-prog)'
  }
}

/**
 * One sentence on what the state means for the requester — the "Where it stands"
 * panel of the mockup's in-progress state.
 *
 * These are the requester's words, never the desk's: "the desk is looking at
 * it", not "triaging". An unknown state gets the neutral sentence rather than
 * nothing, because an empty panel reads as an error.
 */
export const REQUESTER_TRACK_NOTE: Readonly<Record<RequesterTrack, string>> = {
  progress:
    'The desk has your request and is working on it. Nothing is needed from you right now — if that changes, you will be asked here.',
  needs_reply:
    'The desk needs a bit more from you before it can continue. Your answer goes straight back to them.',
  delivered:
    'The desk says the work is ready. Check what was delivered, then accept it or reopen it and say what is missing.',
  accepted:
    'You accepted the outcome, so this request is closed. Nothing further is needed from you.',
  closed:
    'The desk has closed this request. You can still read everything that was shared with you here.',
}

/** The answer box's own validation: empty, or over the server's comment bound. */
export function validateAnswer(text: string): string | null {
  const trimmed = text.trim()
  if (!trimmed) return 'Write your answer first — the desk is waiting on it.'
  const n = [...trimmed].length
  if (n > MAX_ANSWER_LEN) {
    return `That answer is ${n} characters, and ${MAX_ANSWER_LEN} is the most one message can carry. ` +
      'Shorten it — the desk can ask again for anything else.'
  }
  return null
}

/** How much of the answer bound is used; `null` while it does not matter. */
export function answerCounter(text: string): string | null {
  const n = [...text.trim()].length
  if (n < MAX_ANSWER_LEN * 0.8) return null
  return `${n} of ${MAX_ANSWER_LEN} characters${n > MAX_ANSWER_LEN ? ' — too long to send' : ''}`
}

// ---- Refusals of the requester's own acts ----

/** Which of the requester's three acts a notice is about. */
export type RequesterAct = 'answer' | 'accept' | 'reopen'

/**
 * What to tell a requester whose own act was refused.
 *
 * Every sentence says whether anything was recorded, because that is the
 * question a person actually has. The three answers are: a 4xx refusal wrote
 * nothing (the service refuses before it writes); a 409 means the row moved
 * while they were looking at it, so nothing was written *against the version they
 * saw*; and a dropped connection or a 5xx is the one case where the answer is
 * unknown — and there the sentence is that reloading shows the truth, which it
 * does, because these three acts are all transitions (a repeat is refused as a
 * conflict rather than writing twice).
 */
export interface RequesterActNotice {
  title: string
  body: string
  /** Whether offering "Reload the request" is honest and useful. */
  reload: boolean
}

function actFacts(err: unknown): { status?: number; code?: string; message: string; retryAfterS?: number } {
  const e = err as { status?: number; code?: string; message?: string; retryAfterS?: number } | null | undefined
  return {
    status: typeof e?.status === 'number' ? e.status : undefined,
    code: typeof e?.code === 'string' ? e.code : undefined,
    message: typeof e?.message === 'string' ? e.message.trim() : '',
    retryAfterS: typeof e?.retryAfterS === 'number' ? e.retryAfterS : undefined,
  }
}

const ACT_VERB: Record<RequesterAct, { done: string; did: string }> = {
  answer: { done: 'sent', did: 'send your answer' },
  accept: { done: 'recorded', did: 'accept the outcome' },
  reopen: { done: 'recorded', did: 'reopen the request' },
}

export function requesterActNotice(err: unknown, act: RequesterAct): RequesterActNotice {
  const { status, code, message, retryAfterS } = actFacts(err)
  const verb = ACT_VERB[act]
  const nothing = `Nothing was ${verb.done}.`

  if (status === 409 || code === 'request.conflict') {
    return {
      title: 'This request moved while you were looking at it',
      body:
        `${message ? `${message} ` : ''}${nothing} Reload it to see where it stands now, then decide again.`,
      reload: true,
    }
  }

  if (status === 404 || code === 'request.not_found') {
    return {
      title: "This request isn't available to you any more",
      body:
        `${nothing} A request is visible only from the sign-in that sent it, so it may have been removed, ` +
        'or it may no longer be yours. Reload to check.',
      reload: true,
    }
  }

  if (status === 403) {
    return {
      title: `Only the request's own requester can ${verb.did.replace(/^to /, '')}`,
      body: `${nothing} ${message || 'This sign-in is not the one that sent the request.'}`,
      reload: false,
    }
  }

  if (status === 429 || code === 'request.rate_limited') {
    const wait = retryAfterS && retryAfterS > 0 ? ` in ${retryAfterS} second${retryAfterS === 1 ? '' : 's'}` : ' in a moment'
    return { title: 'That was a lot at once', body: `${nothing} Try again${wait}.`, reload: false }
  }

  if (status === 400 || code === 'request.invalid') {
    return {
      title: "The desk couldn't take that",
      body: `${message || 'Something about it was refused.'} ${nothing}`,
      reload: false,
    }
  }

  if (status !== undefined && status < 500) {
    return {
      title: 'That did not go through',
      body: `${message || `The desk's system refused it (HTTP ${status}).`} ${nothing}`,
      reload: false,
    }
  }

  return {
    title: 'We could not tell whether that reached the desk',
    body:
      `The connection failed part-way, so ${act === 'answer' ? 'your answer' : 'the change'} may or may not have ` +
      'been recorded. Reload the request to see the truth before trying again — a repeat is refused rather than ' +
      'applied twice.',
    reload: true,
  }
}

/**
 * What a *read* of the request failed with.
 *
 * `not_found` covers every "you cannot see this" the surface answers with: an
 * unknown reference, another person's reference, and a request whose requester
 * changed. They are one arm because they are one server answer, and the screen
 * must render them identically — the whole point of the surface's single 404 is
 * that a reference cannot be probed. A 403 is folded in for the same reason (a
 * credential never reaches a person's page), and everything else is a real
 * failure worth a retry button.
 */
export type RequesterReadFailure = 'not_found' | 'failed'

export function requesterReadFailure(err: unknown): RequesterReadFailure {
  const { status, code } = actFacts(err)
  if (status === 404 || status === 403 || code === 'request.not_found') return 'not_found'
  return 'failed'
}

// ---- Shared outputs ----

/**
 * The three words a requester sees about a file, and nothing else.
 *
 * `preparing` is deliberately vague in the *right* direction: it covers a scan
 * that has not finished and a lead's hold, both of which are the team's to
 * resolve and neither of which a requester can act on. `unavailable` is
 * terminal — the projection only ever says it when nothing can serve the bytes.
 */
export function requesterOutputWord(status: RequesterOutputStatus): string {
  switch (status) {
    case 'ready':
      return 'Ready'
    case 'preparing':
      return 'Preparing'
    default:
      return 'Unavailable'
  }
}

/** Whether a file row may offer a download. The projection only sets a ref for a ready file. */
export function requesterOutputDownloadable(ref: string | undefined): boolean {
  return typeof ref === 'string' && ref.trim() !== ''
}

/**
 * The path a `download_ref` is used at.
 *
 * The projection's ref is an opaque handle, not a row id and not a URL — the read
 * path mints it (`service.RequestDownloadLinker`) and the serve route accepts it
 * at `service.RequestDownloadGrantPrefix` (`/api/request-downloads/`). This
 * mirrors that one constant; a ref that already looks like a path is used as it
 * is, so a future absolute URL needs no change here.
 */
export function requesterDownloadPath(ref: string): string {
  const value = ref.trim()
  if (!value) return ''
  return value.startsWith('/') ? value : `/api/request-downloads/${encodeURIComponent(value)}`
}
