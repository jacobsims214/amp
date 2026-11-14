/**
 * The request detail's Deliver panel (AMP #3980, build slice J2; approved mockup
 * artifact 37 v1, "Request detail" → Deliver, plus the delivered state's
 * evidence rows).
 *
 * Two jobs, and both of them exist to stop a request being "delivered" by
 * accident:
 *
 *  1. **The guard.** `delivered` needs an outcome brief — what is and is not
 *     included — plus at least one live `delivered_by` link, or an explicit
 *     no-artifact record (contract invariant I2). The button is disabled until
 *     the server's own aggregate says `delivered_eligible`, and the reason it is
 *     disabled is a sentence next to it, not a tooltip: a person who cannot press
 *     a button must be able to read why without discovering it by hovering.
 *
 *  2. **The evidence.** Once delivered, the panel shows the brief and each piece
 *     of evidence with its own state (merged / approved / pending verify /
 *     deleted), so the delivery reads as evidenced rather than asserted.
 *     Acceptance is stated as the requester's alone (I1/D5): no control here
 *     accepts anything, and no agent, token or team member can.
 *
 * What it deliberately does not do: no close-by-ticket-state. A ticket reaching
 * `completed` changes nothing on this panel, which is I8 made visible — the
 * aggregate's `phase` and `done` counts update, `delivered_eligible` does not.
 */
import { useState } from 'react'
import { Check, Loader2, ShieldAlert } from 'lucide-react'
import {
  deliverGuard, deliverGuardSentence, evidenceRows, outcomeBrief, outcomeSummary, notDeliveredYet, type ResolvedRequestLink,
} from '../../lib/requestControls'
import { isZeroTime } from '../../lib/approval'
import type { RequestLifecycleAnswer, RequestOutcome, TeamRequest } from '../../types'
import { RailBanner, RailLinkRow } from './RequestRailBits'

export interface RequestDeliverPanelProps {
  request: TeamRequest
  /** The delivery aggregate for this request (`outcome` on the detail read). */
  outcome: RequestOutcome
  /** The delivered_by links with their targets resolved, for the evidence rows. */
  evidence: ResolvedRequestLink[]
  /** Whether the caller is on the inbox team and may deliver it. */
  canDeliver: boolean
  /** State that the outcome exists. Omitted when the page does not offer it. */
  onDeliver?: (input: { outcome_brief: string; no_artifact: boolean }) => Promise<RequestLifecycleAnswer>
  /**
   * Link work as delivery evidence — "the delivered_by attach". Omitted when the
   * page offers no attach path; the panel then states what is missing instead.
   */
  onAttachEvidence?: (target: { kind: 'task' | 'artifact' | 'receipt'; id: number }) => Promise<{ changed: boolean }>
  /** A row's href: where the evidence actually lives. */
  hrefFor?: (target: NonNullable<ResolvedRequestLink['target']>) => string | undefined
  /** The requester's display name/contact, for "waiting on the requester". */
  requesterLabel?: string
}

/** A failure the panel states in place. */
type Failure = { title: string; message: string }

export function RequestDeliverPanel(props: RequestDeliverPanelProps) {
  const { request, outcome, evidence, canDeliver, onDeliver, onAttachEvidence, hrefFor, requesterLabel } = props
  const guard = deliverGuard(request, outcome)
  const [brief, setBrief] = useState('')
  const [noArtifact, setNoArtifact] = useState(false)
  const [busy, setBusy] = useState<'deliver' | 'attach' | null>(null)
  const [failure, setFailure] = useState<Failure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [attachKind, setAttachKind] = useState<'task' | 'artifact' | 'receipt'>('task')
  const [attachId, setAttachId] = useState('')

  const briefText = brief.trim()
  // The panel's own precondition, on top of the server's: a delivery needs a
  // sentence in the box it is looking at. The server's gate is still the
  // authority — this only keeps a person from pressing a button that would be
  // refused, with the reason already written beside it.
  const canPressDeliver = guard.ready && briefText !== '' && canDeliver && !!onDeliver
  const delivered = guard.delivered
  const briefInRecord = outcomeBrief(request)
  const rows = evidenceRows(evidence, outcome)

  const deliver = async () => {
    if (!onDeliver || !canPressDeliver) return
    setBusy('deliver')
    setFailure(null)
    setNotice(null)
    try {
      const answer = await onDeliver({ outcome_brief: briefText, no_artifact: noArtifact })
      setNotice(answer.changed ? null : 'This request was already delivered: nothing changed.')
      setBrief('')
    } catch (err) {
      const e = err as { status?: number; rev?: number; message?: string } | null
      setFailure({
        title: e?.status === 409 ? 'This request changed since your view loaded.' : 'The delivery wasn’t recorded.',
        message: (e?.message?.trim() || 'The request could not be delivered.') +
          (e?.status === 409 && e.rev != null ? ` It is at rev ${e.rev} now.` : '') + ' Nothing was changed.',
      })
    } finally {
      setBusy(null)
    }
  }

  const attach = async () => {
    const id = Number.parseInt(attachId, 10)
    if (!onAttachEvidence || !Number.isFinite(id) || id <= 0) return
    setBusy('attach')
    setFailure(null)
    setNotice(null)
    try {
      const r = await onAttachEvidence({ kind: attachKind, id })
      setNotice(r.changed ? 'Linked as delivery evidence.' : 'Already linked: nothing changed.')
      setAttachId('')
    } catch (err) {
      const e = err as { message?: string } | null
      setFailure({ title: 'The evidence wasn’t linked.', message: `${e?.message?.trim() || 'The link could not be written.'} Nothing was changed.` })
    } finally {
      setBusy(null)
    }
  }

  return (
    <section className="amp-panel" aria-label="Delivery">
      <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
        <span className="amp-label">Deliver</span>
        <span className="ml-auto text-[11.5px]" style={{ color: delivered ? 'var(--success-ink)' : 'var(--ink-3)' }}>
          {delivered
            ? (request.state === 'requester_accepted' ? 'accepted by the requester' : 'delivered · awaiting the requester')
            : outcomeSummary(outcome)}
        </span>
      </div>
      <div className="px-4 py-3 flex flex-col gap-3">
        {failure && (
          <RailBanner tone="err" title={failure.title}>{failure.message}</RailBanner>
        )}
        {notice && <p className="m-0 text-[12px] text-ink-3" role="status">{notice}</p>}

        {delivered ? (
          <>
            <div className="grid gap-1">
              <span className="amp-label">Outcome brief</span>
              <p className="m-0 text-[13px] leading-[1.5] whitespace-pre-wrap">
                {briefInRecord ?? 'No outcome brief was recorded.'}
              </p>
            </div>
            {outcome.no_artifact && !briefInRecord && (
              <p className="m-0 text-[12.5px] text-ink-2">
                The team stated there is nothing to link. For the requester&rsquo;s view to say that honestly, the brief
                still has to say what was and was not included.
              </p>
            )}
            {rows.length > 0 && (
              <div className="flex flex-col">
                {rows.map(r => (
                  <RailLinkRow
                    key={`${r.link.link_kind}:${r.link.target_kind}:${r.link.target_id}`}
                    kind="Delivered by"
                    target={r.link.target?.title.split(' — ')[0] ?? `#${r.link.target_id}`}
                    title={r.link.target ? (r.link.target.title.split(' — ').slice(1).join(' — ') || r.link.target.title) : 'This item is no longer available'}
                    href={r.link.target && !r.link.target.gone && hrefFor ? hrefFor(r.link.target) : undefined}
                    tag={r.word}
                    tone={r.tone}
                    title_={r.note}
                  />
                ))}
              </div>
            )}
            {rows.some(r => r.note) && (
              <ul className="m-0 pl-4 flex flex-col gap-1 text-[11.5px] text-ink-3 leading-[1.45]">
                {rows.filter(r => r.note).map(r => (
                  <li key={`note:${r.link.target_kind}:${r.link.target_id}`}>
                    <span className="amp-num">{r.link.target?.title.split(' — ')[0] ?? `#${r.link.target_id}`}</span>: {r.note}
                  </li>
                ))}
              </ul>
            )}
            <p className="m-0 flex items-start gap-1.5 text-[12px] text-ink-2 leading-[1.45]">
              <ShieldAlert size={12} className="flex-shrink-0 mt-[1px]" aria-hidden="true" />
              Acceptance belongs to the requester alone (I1) — no one on the team, and no agent, can accept it for them.
              {request.accepted_at && !isZeroTime(request.accepted_at) && (
                <> Accepted {request.accepted_at.slice(0, 10)}.</>
              )}
              {!request.accepted_at && requesterLabel && <> Waiting on {requesterLabel}.</>}
            </p>
          </>
        ) : (
          <>
            <p className="m-0 text-[12.5px] text-ink-2 leading-[1.45]">
              Delivering needs an outcome brief (what is and isn&rsquo;t included) and at least one delivered link — a
              ticket, an artifact or a receipt.
            </p>

            {guard.block === 'closed' ? (
              <RailBanner tone="info" title="Nothing to deliver.">{guard.reason}</RailBanner>
            ) : (
              <>
                <div className="grid gap-1">
                  <label className="amp-label" htmlFor="outcome-brief">Outcome brief</label>
                  <textarea
                    id="outcome-brief"
                    className="amp-input resize-y min-h-[64px] !text-[13px]"
                    rows={3}
                    value={brief}
                    disabled={busy !== null || !canDeliver}
                    onChange={e => setBrief(e.target.value)}
                    placeholder="What is and is not included — the requester reads this"
                    aria-describedby="deliver-guard"
                  />
                  {briefInRecord && (
                    <p className="m-0 text-[11.5px] text-ink-3 leading-[1.45]">
                      A brief is already stored. Typing a new one replaces it when you deliver.
                    </p>
                  )}
                </div>

                {onAttachEvidence && canDeliver && (
                  <div className="flex flex-col gap-1.5">
                    <span className="amp-label">Delivered by</span>
                    <div className="flex items-center gap-2 flex-wrap">
                      <select
                        className="amp-input !w-auto !text-[12.5px] !py-1"
                        aria-label="What kind of evidence"
                        value={attachKind}
                        onChange={e => setAttachKind(e.target.value as 'task' | 'artifact' | 'receipt')}
                      >
                        <option value="task">A ticket</option>
                        <option value="artifact">An artifact</option>
                        <option value="receipt">A receipt</option>
                      </select>
                      <input
                        className="amp-input !w-[110px] !text-[12.5px] !py-1"
                        inputMode="numeric"
                        aria-label={`The ${attachKind}'s id`}
                        placeholder="id"
                        value={attachId}
                        onChange={e => setAttachId(e.target.value)}
                      />
                      <button type="button" className="amp-btn amp-btn-sm" disabled={busy !== null || !attachId.trim()} onClick={attach}>
                        {busy === 'attach' && <Loader2 size={11} className="animate-spin" aria-hidden="true" />}
                        Link
                      </button>
                    </div>
                    <p className="m-0 text-[11.5px] text-ink-3 leading-[1.45]">
                      Linking evidence never marks anything done or approved: it records what the delivery points at.
                    </p>
                  </div>
                )}

                {rows.length === 0 && outcome.total > 0 && (
                  // The aggregate counts evidence, but this panel was given no
                  // link rows to show: say so rather than present an empty list
                  // as "nothing delivered".
                  <p className="m-0 text-[12px] text-ink-3">
                    {outcome.total} delivered item{outcome.total === 1 ? '' : 's'} counted; the detail read didn&rsquo;t
                    include their rows.
                  </p>
                )}

                <label className="flex items-start gap-2 text-[12px] text-ink-2">
                  <input
                    type="checkbox"
                    className="mt-[3px]"
                    checked={noArtifact}
                    disabled={busy !== null || !canDeliver || outcome.total > 0}
                    onChange={e => setNoArtifact(e.target.checked)}
                  />
                  <span>
                    There is nothing to link: state that explicitly.
                    {outcome.total > 0 && (
                      <span className="text-ink-3"> It is either linked to its evidence or explicitly no-artifact, not both.</span>
                    )}
                  </span>
                </label>

                <div className="flex flex-col gap-1.5">
                  <button
                    type="button"
                    className="amp-btn amp-btn-sm self-start"
                    disabled={busy !== null || !canPressDeliver}
                    aria-describedby="deliver-guard"
                    title={canPressDeliver ? undefined : guard.reason || 'Add an outcome brief and a delivered link first'}
                    onClick={deliver}
                  >
                    {busy === 'deliver'
                      ? <Loader2 size={11} className="animate-spin" aria-hidden="true" />
                      : <Check size={11} aria-hidden="true" />}
                    Mark delivered
                  </button>
                  <p id="deliver-guard" className="m-0 text-[11.5px] text-ink-3 leading-[1.45]" data-guard={guard.block}>
                    {deliverGuardSentence(guard, briefText !== '', canDeliver)}
                  </p>
                </div>
              </>
            )}
          </>
        )}

        {notDeliveredYet(outcome, request.state) && (
          <RailBanner tone="info" title="Done isn’t delivered.">
            {notDeliveredYet(outcome, request.state)}
          </RailBanner>
        )}
      </div>
    </section>
  )
}

