/**
 * The requester's three access states (AMP #3987, build slice J6; approved mockup
 * artifact 38 v1's "No access" screen: "We can't find that request", "You've been
 * invited to send requests", "Your sending access was removed").
 *
 * ## The one rule that decides this file's shape
 *
 * Two of the three states are **not** server answers, and the third is the only
 * one a read can produce:
 *
 * | state | where it comes from |
 * |---|---|
 * | `not_found` | the request read's single 404 — an unknown reference, another person's reference, and a request whose requester changed are all this one answer |
 * | `invite_pending` | the entry point: an invite link knows the team and the address it was sent to |
 * | `grant_revoked` | the entry point: a submission that was refused knows the attempt, and the desk's own message says access was removed |
 *
 * The server cannot tell `invite_pending` from `grant_revoked` from `not_found`
 * and must not: `authz.TeamSubmit` answers one not-found for all of them, so that
 * a guessed address cannot be used to learn whether an inbox exists or whether
 * somebody used to have access. A screen that derived "revoked" from a 404 would
 * rebuild exactly the oracle the server refuses to be — so this component takes
 * the state as a **prop** and never infers it from an error.
 *
 * ## The indistinguishable-404 requirement, made structural
 *
 * `not_found` is one component with one set of words and no branch on why. There
 * is no prop for "foreign" or "unknown", no `reason` field, and no state that
 * renders a different sentence for a request that exists but is not yours — which
 * is why the page cannot accidentally build the oracle either. Its test asserts
 * that two different failures render byte-identical text.
 *
 * ## What none of the three ever says
 *
 * No project, epic, ticket, team member, org, agent or internal state name. The
 * team's *public display name* is the one team fact the mockup shows (a person
 * needs to know who they wrote to), and the address is the person's own.
 */
import type { ReactNode } from 'react'
import { LifeBuoy, Lock, MailCheck, ShieldOff } from 'lucide-react'

/** Which access state a page is in. See the table above for where each may come from. */
export type RequesterAccessState = 'not_found' | 'invite_pending' | 'grant_revoked'

export interface RequesterAccessStatesProps {
  state: RequesterAccessState
  /** The desk's public display name, when the entry point knows it. */
  teamName?: string
  /** The address the invitation was sent to, or the signed-in address. Theirs, never a lookup. */
  email?: string
  /** The requester's own two screens, as the page's own callbacks (#3988 owns routing). */
  onViewRequests?: () => void
  /** Sign in (invite pending) — the invite is tied to the address it was sent to. */
  onSignIn?: () => void
  /** Ask the desk to restore access (revoked). Omitted, no such button. */
  onContactDesk?: () => void
}

export function RequesterAccessStates({
  state, teamName, email, onViewRequests, onSignIn, onContactDesk,
}: RequesterAccessStatesProps) {
  const desk = teamName?.trim() || 'the desk'
  const address = email?.trim() || ''

  return (
    <div
      className="mx-auto mt-8 max-w-[560px] border bg-surface px-6 py-8 text-center sm:mt-14 sm:px-7 sm:py-9"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
      data-testid="requester-access-state"
      data-access-state={state}
    >
      <span className="mx-auto mb-4 flex w-[18px] flex-col gap-[2px]" aria-hidden="true">
        <i className="block h-[3px] bg-band-1" /><i className="block h-[3px] bg-band-2" />
        <i className="block h-[3px] bg-band-3" /><i className="block h-[3px] bg-band-4" />
      </span>

      {state === 'not_found' && <NotFound desk={desk} onViewRequests={onViewRequests} onContactDesk={onContactDesk} />}
      {state === 'invite_pending' && <InvitePending desk={desk} address={address} onSignIn={onSignIn} />}
      {state === 'grant_revoked' && <GrantRevoked desk={desk} onViewRequests={onViewRequests} onContactDesk={onContactDesk} />}
    </div>
  )
}

/**
 * "We can't find that request".
 *
 * One set of words for every reason it can be true, because the server gives one
 * answer for every reason it can be true. The sentence says *why* it is written
 * that way ("for your privacy we can't say which"), which is both honest and the
 * only thing that keeps a person from assuming the link is broken.
 */
function NotFound({ desk, onViewRequests, onContactDesk }: { desk: string; onViewRequests?: () => void; onContactDesk?: () => void }) {
  return (
    <>
      <h1 className="m-0 text-[20px] font-semibold tracking-[-0.01em] text-ink">We can't find that request</h1>
      <p className="mx-auto mb-5 mt-2 max-w-[46ch] text-[13px] leading-relaxed text-ink-2">
        The link may be wrong, or the request belongs to a different sign-in. For your privacy we can't say which —
        requests are only ever visible to the address that sent them.
      </p>
      <Actions
        primary={onViewRequests ? { label: 'Your requests', onClick: onViewRequests } : undefined}
        secondary={onContactDesk ? { label: `Contact ${desk}`, onClick: onContactDesk } : undefined}
      />
    </>
  )
}

/** "You've been invited to send requests" — the address the invitation names, and a sign-in. */
function InvitePending({ desk, address, onSignIn }: { desk: string; address: string; onSignIn?: () => void }) {
  return (
    <>
      <h1 className="m-0 text-[20px] font-semibold tracking-[-0.01em] text-ink">You've been invited to send requests</h1>
      <p className="mx-auto mb-5 mt-2 max-w-[46ch] text-[13px] leading-relaxed text-ink-2">
        <b className="font-semibold text-ink">{desk}</b> has invited
        {address ? <> <b className="font-semibold text-ink break-words">{address}</b></> : ' this address'} to send them
        requests and track the answers here. Sign in with that address to get started — no account on their systems is
        needed, and they never see anything of yours but your requests.
      </p>
      <Actions primary={onSignIn ? { label: 'Sign in to continue', onClick: onSignIn } : undefined} />
      <p className="mb-0 mt-4 flex items-start justify-center gap-2 text-[11.5px] leading-relaxed text-ink-3">
        <MailCheck size={13} className="mt-0.5 flex-shrink-0" aria-hidden="true" />
        <span>
          Signed in with a different address? Sign out and use the invited one — invitations are tied to the address
          they were sent to.
        </span>
      </p>
    </>
  )
}

/**
 * "Your sending access was removed".
 *
 * The one thing it must not imply is that the person's existing requests went
 * with it: a revoked grant removes the ability to *send*, and `authz.TeamSubmit`
 * is a submit-only capability that confers no read on anybody else's request.
 * Their own requests stay theirs.
 */
function GrantRevoked({ desk, onViewRequests, onContactDesk }: { desk: string; onViewRequests?: () => void; onContactDesk?: () => void }) {
  return (
    <>
      <h1 className="m-0 text-[20px] font-semibold tracking-[-0.01em] text-ink">Your sending access was removed</h1>
      <p className="mx-auto mb-5 mt-2 max-w-[46ch] text-[13px] leading-relaxed text-ink-2">
        {desk} has removed your ability to send new requests.{' '}
        <b className="font-semibold text-ink">Requests you already sent are still yours to see</b> — their status and
        anything delivered to you remain available.
      </p>
      <Actions
        primary={onViewRequests ? { label: 'Your existing requests', onClick: onViewRequests } : undefined}
        secondary={onContactDesk ? { label: 'Ask the desk to restore access', onClick: onContactDesk } : undefined}
      />
    </>
  )
}

/** The two buttons the mockup gives each state. Below `sm` they stack full width and 44 px tall. */
function Actions({ primary, secondary }: {
  primary?: { label: string; onClick: () => void }
  secondary?: { label: string; onClick: () => void }
}) {
  if (!primary && !secondary) return null
  return (
    <div className="flex flex-col justify-center gap-2 sm:flex-row sm:flex-wrap">
      {primary && (
        <button type="button" onClick={primary.onClick} className="amp-btn amp-btn-primary w-full sm:w-auto" style={{ minHeight: 44 }}>
          {primary.label}
        </button>
      )}
      {secondary && (
        <button type="button" onClick={secondary.onClick} className="amp-btn w-full sm:w-auto" style={{ minHeight: 44 }}>
          {secondary.label}
        </button>
      )}
    </div>
  )
}

/**
 * A quiet line for a page that wants to say what a *read failure* was without
 * turning a transient 500 into an access state.
 *
 * It is deliberately not part of the three: a dropped connection is not "we can't
 * find that request", and telling a person their request does not exist because a
 * query timed out is the exact misreport this whole file exists to avoid.
 */
export function RequesterLoadFailure({ message, onRetry, children }: {
  message: string
  onRetry?: () => void
  children?: ReactNode
}) {
  return (
    <div
      role="alert"
      data-testid="requester-load-failure"
      className="mx-auto mt-8 max-w-[560px] border px-5 py-5"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--danger)', borderLeft: '5px solid var(--danger)', background: 'var(--surface)' }}
    >
      <h1 className="m-0 flex items-center gap-2 text-[16px] font-semibold text-ink">
        <LifeBuoy size={15} className="flex-shrink-0" aria-hidden="true" />
        This request didn't load
      </h1>
      <p className="m-0 mt-1.5 text-[12.5px] leading-relaxed text-ink-2">{message}</p>
      <p className="m-0 mt-1.5 flex items-start gap-2 text-[11.5px] leading-relaxed text-ink-3">
        <Lock size={12} className="mt-0.5 flex-shrink-0" aria-hidden="true" />
        <span>Nothing about your request changed — this page only reads it.</span>
      </p>
      {onRetry && (
        <button type="button" onClick={onRetry} className="amp-btn amp-btn-sm mt-3" style={{ minHeight: 32 }}>
          <ShieldOff size={11} aria-hidden="true" />Try again
        </button>
      )}
      {children}
    </div>
  )
}
