/**
 * The requester's accept-or-reopen bar (AMP #3987, build slice J6; approved
 * mockup artifact 38 v1's "Does this answer your request?" block).
 *
 * Two acts, and they are the two the lifecycle actually offers from `delivered`:
 * acceptance (`delivered → requester_accepted`, the only row in the transition
 * table whose minimum is `RequestCapabilityRequester`) and reopen
 * (`delivered → triaging`, a reopen transition rather than a state — D6). From
 * `requester_accepted` only reopen remains, so the bar renders what is legal
 * rather than what is pretty.
 *
 * ## Why there is no "reject the outcome", no rating and no note box
 *
 * The contract's requester side is binary on purpose (§4): either the outcome
 * answers the request or it does not, and "it does not" is a reopen — not a third
 * state. Anything else would need a state the lifecycle does not have.
 *
 * The note is a harder case, and the mockup is optimistic about it: its copy says
 * "reopen it and say what — the desk picks it back up". **The API has no such
 * field.** `POST /api/requests/{ref}/reopen` takes a precondition and nothing
 * else, and the requester surface has no comment route at all (a `comment` on a
 * request is a team route, and the requester's own words only reach the thread as
 * an `answer`, which is legal only out of `needs_information`). So a note box
 * here would accept words and silently drop them — the one failure mode this
 * surface must never have. Instead the bar says plainly that the desk will ask if
 * it needs detail, which is true: reopening lands the request back in the queue
 * in a state where asking is legal, and the desk's question then opens the answer
 * box. When a requester comment route exists, this becomes a textarea plus one
 * extra call and nothing else on the page changes.
 *
 * ## Why acceptance is offered without a "confirm?" step
 *
 * It is reversible: reopen is the very next arm. A modal on an act the product
 * lets you undo in one click is friction, not safety.
 */
import { useState } from 'react'
import { Check, Loader2, RotateCcw } from 'lucide-react'
import { requestApi } from '../../api/client'
import { requesterActNotice, type RequesterActNotice } from '../../lib/requesterStatus'

export interface RequesterAcceptBarProps {
  /** The reference the requester holds. */
  publicRef: string
  /** The request's state: `delivered` offers both acts, `requester_accepted` only a reopen. */
  state: 'delivered' | 'requester_accepted'
  /** The revision the page read, when it has one. */
  expectedRev?: number
  /** Called after the server recorded the act, with the state it returned. */
  onChanged?: (state: string, rev: number) => void
}

type Arm = 'idle' | 'accepting' | 'reopening'

export function RequesterAcceptBar({ publicRef, state, expectedRev, onChanged }: RequesterAcceptBarProps) {
  const [arm, setArm] = useState<Arm>('idle')
  const [notice, setNotice] = useState<RequesterActNotice | null>(null)

  const rev = expectedRev && expectedRev > 0 ? { expected_rev: expectedRev } : {}
  const busy = arm !== 'idle'

  const accept = async () => {
    setArm('accepting')
    setNotice(null)
    try {
      const res = await requestApi.accept(publicRef, rev)
      onChanged?.(res.request.state, res.request.rev)
    } catch (e) {
      setNotice(requesterActNotice(e, 'accept'))
    } finally {
      setArm('idle')
    }
  }

  const reopen = async () => {
    setArm('reopening')
    setNotice(null)
    try {
      const res = await requestApi.ownReopen(publicRef, rev)
      onChanged?.(res.request.state, res.request.rev)
    } catch (e) {
      setNotice(requesterActNotice(e, 'reopen'))
    } finally {
      setArm('idle')
    }
  }

  return (
    <div
      className="border bg-surface px-4 py-3.5"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
      data-testid="requester-accept-bar"
    >
      <h3 className="m-0 mb-1 font-label text-[12px] font-semibold uppercase tracking-[0.16em] text-ink-2">
        {state === 'delivered' ? 'Does this answer your request?' : 'You accepted this outcome'}
      </h3>
      <p className="m-0 mb-3 text-[13px] leading-relaxed text-ink-2">
        {state === 'delivered'
          ? "Only you can close this. If something's missing or wrong, reopen it — the desk picks it back up and asks you anything it needs."
          : 'Reopening sends it back to the desk. They will ask you what is still missing.'}
      </p>

      {notice && (
        <div
          role="alert"
          data-testid="accept-notice"
          className="mb-3 border px-3 py-2.5"
          style={{ borderRadius: 'var(--radius)', borderColor: 'var(--danger)', background: 'var(--surface)' }}
        >
          <b className="block text-[12.5px] font-semibold text-ink">{notice.title}</b>
          <p className="m-0 mt-1 text-[12px] leading-relaxed text-ink-2">{notice.body}</p>
          {notice.reload && (
            <p className="m-0 mt-1.5 text-[11.5px] leading-relaxed text-ink-3">
              Reload the page to see where it stands now.
            </p>
          )}
        </div>
      )}

      <div className="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
        {state === 'delivered' && (
          <button
            type="button"
            className="amp-btn amp-btn-primary w-full sm:w-auto"
            style={{ minHeight: 44 }}
            disabled={busy}
            onClick={() => void accept()}
            data-testid="accept-button"
          >
            {arm === 'accepting'
              ? <><Loader2 size={12} className="animate-spin" aria-hidden="true" />Accepting…</>
              : <><Check size={12} aria-hidden="true" />Yes — accept the outcome</>}
          </button>
        )}
        <button
          type="button"
          className="amp-btn w-full sm:w-auto"
          style={{ minHeight: 44 }}
          disabled={busy}
          onClick={() => void reopen()}
          data-testid="reopen-button"
        >
          {arm === 'reopening'
            ? <><Loader2 size={12} className="animate-spin" aria-hidden="true" />Reopening…</>
            : <><RotateCcw size={12} aria-hidden="true" />Something's missing — reopen</>}
        </button>
      </div>

      <p className="m-0 mt-2.5 text-[11.5px] leading-relaxed text-ink-3">
        A note can't be attached from this page yet — reopening puts the request back in the desk's queue, and they
        will ask you what is missing.
      </p>
    </div>
  )
}
