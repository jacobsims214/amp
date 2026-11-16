/**
 * The request detail's Planning rail (AMP #3980, build slice J2; approved mockup
 * artifact 37 v1, "Request detail" → Planning).
 *
 * What it shows, and why each part is there:
 *
 *  - **The route** (`routes_to`): the one project a request is routed to (D9).
 *    A second route is a 409 naming the project the request is on now, so a
 *    move is a deliberate act with `supersede_route`, not a second link.
 *  - **The plan items** (`planned_as`): the epics, stories and tickets put in
 *    place for this request, each carrying its own approval fact. "Awaiting
 *    approval" is read from the target's own `approved` fact — never inferred
 *    from a ticket's state, because a completed ticket nobody approved is still
 *    unapproved, and a closed ticket never delivers a request (I8).
 *  - **The handoff warning**, between the rows and the button and not
 *    dismissible: "Handoff never auto-approves." The contract's core safety
 *    invariant is un-removable by placement, which is the only way a UI can
 *    promise it (slice J2's third criterion).
 *
 * Planning is a *link*, never an approval or a dispatch. The handoff creates
 * tickets that await a person's approval; nothing on this rail can approve one,
 * start one, or mark one done. That is why there is no "Approve" control here
 * and no state a click could set — the only writes are `POST …/links` (via the
 * page's own attach action) and, where the page offers it, a handoff request.
 *
 * The link rows themselves are passed in: the shipped read route answers the
 * record and the delivery aggregate, not the link list, so the page (J2's own
 * detail screen, #3984) resolves links to titles and approval facts and this
 * panel renders them.
 */
import { useState } from 'react'
import { ExternalLink, Loader2, ShieldAlert } from 'lucide-react'
import { readAttachFailure, groupLinks, plannedApproval, phaseWord, type AttachOutcome, type ResolvedRequestLink } from '../../lib/requestControls'
import type { RequestOutcome, TeamRequest } from '../../types'
import { RailBanner, RailLinkRow, RailTag } from './RequestRailBits'

export interface RequestPlanningRailProps {
  request: TeamRequest
  /** The request's link rows with their targets resolved (`routes_to`, `planned_as`, `relates_to`). */
  links: ResolvedRequestLink[]
  /** The delivery aggregate: what the linked work amounts to right now. */
  outcome: RequestOutcome
  /** Whether the caller may attach planning relations (the inbox team). */
  canPlan: boolean
  /**
   * Open the handoff form. Omitted when the page does not offer it (no routed
   * project, or the caller may not plan): the rail then says what the handoff
   * would do rather than showing a button that cannot work.
   */
  onHandoff?: () => void
  /** Change the routed project — a deliberate re-route (`supersede_route`), on the page. */
  onChangeProject?: () => void
  /** Attach one plan item. `Adding` is the rail's own affordance: an id, and the page writes the link. */
  onAttach?: (target: { kind: 'epic' | 'story' | 'task'; id: number }) => Promise<{ changed: boolean }>
  /** A row's href: where the ticket, epic, story or artifact lives. */
  hrefFor?: (target: NonNullable<ResolvedRequestLink['target']>) => string | undefined
  /** The clock for ages. */
  now?: number
}

export function RequestPlanningRail(props: RequestPlanningRailProps) {
  const { request, links, outcome, canPlan, onHandoff, onChangeProject, onAttach, hrefFor } = props
  const grouped = groupLinks(links)
  const approval = plannedApproval(grouped)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AttachOutcome | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [attachKind, setAttachKind] = useState<'epic' | 'story' | 'task'>('task')
  const [attachId, setAttachId] = useState('')

  const routedProject = grouped.route?.target
  // A closed request is not planned: rejecting or folding it says the team is
  // not going to do the work, so the rail stops offering to plan it and says
  // how to get back to a state where planning means something.
  const closed = request.state === 'rejected' || request.state === 'duplicate'
  const canHandoff = canPlan && !closed && !!grouped.route && !!onHandoff

  const attach = async () => {
    const id = Number.parseInt(attachId, 10)
    if (!onAttach || !Number.isFinite(id) || id <= 0) return
    setBusy(true)
    setFailure(null)
    setNotice(null)
    try {
      const r = await onAttach({ kind: attachKind, id })
      // A replay is success: the relation was written before (or just now by a
      // lost response). Saying "already attached" is the honest version of that.
      setNotice(r.changed ? 'Attached.' : 'Already attached: nothing changed.')
      setAttachId('')
    } catch (err) {
      setFailure(readAttachFailure(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="amp-panel" aria-label="Planning">
      <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
        <span className="amp-label">Planning</span>
        <span className="ml-auto flex items-center gap-2 text-[11.5px] text-ink-3">
          {routedProject
            ? <>routes to: <span className="text-ink">{routedProject.title}</span></>
            : 'not routed'}
        </span>
      </div>
      <div className="px-4 py-3 flex flex-col gap-3">
        {failure?.kind === 'rerouted' && (
          <RailBanner
            tone="err"
            title="This request is already routed to a project."
            actions={onChangeProject && (
              <button type="button" className="amp-btn amp-btn-sm" onClick={onChangeProject}>Change project…</button>
            )}
          >
            {failure.message} One project per request — moving it is a deliberate re-route that keeps the old one as history.
          </RailBanner>
        )}
        {failure?.kind === 'failed' && (
          <RailBanner tone="err" title="The link wasn’t written.">{failure.message}</RailBanner>
        )}
        {notice && <p className="m-0 text-[12px] text-ink-3" role="status">{notice}</p>}

        {grouped.route ? (
          <RailLinkRow
            kind="Routes to"
            target={routedProject ? undefined : `#${grouped.route.target_id}`}
            title={routedProject ? routedProject.title : 'Project no longer available'}
            href={routedProject && hrefFor ? hrefFor(routedProject) : undefined}
            tag={routedProject?.gone ? 'deleted' : undefined}
            tone={routedProject?.gone ? 'gone' : undefined}
            title_={routedProject?.gone ? 'The project this request was routed to has been deleted; the request keeps the route as history.' : undefined}
          />
        ) : (
          <p className="m-0 text-[12.5px] text-ink-2">
            Not routed yet. Routing picks the one project this request&rsquo;s work belongs to; nothing is created by routing it.
          </p>
        )}

        {grouped.planned.map(link => {
          const t = link.target
          const isTask = link.target_kind === 'task'
          const ref = t ? t.title.split(' — ')[0] : `#${link.target_id}`
          const name = t ? (t.title.split(' — ').slice(1).join(' — ') || t.title) : 'This item is no longer available'
          const gone = t?.gone === true
          return (
            <RailLinkRow
              key={`${link.link_kind}:${link.target_kind}:${link.target_id}`}
              kind={isTask ? 'Planned as' : `Planned ${link.target_kind}`}
              target={ref}
              title={name}
              href={t && !gone && hrefFor ? hrefFor(t) : undefined}
              tag={gone ? 'deleted' : t?.approved === true ? 'approved' : 'awaiting approval'}
              tone={gone ? 'gone' : t?.approved === true ? 'ok' : 'rev'}
              title_={gone
                ? 'This plan item was deleted. The link stays as a tombstone: the request was planned as this, and the plan changed.'
                : t?.approved === true
                  ? 'A person approved this item’s current plan.'
                  : 'Created from this request and awaiting a person’s approval. Nothing dispatches it until then.'}
            />
          )
        })}

        {grouped.related.map(link => (
          <RailLinkRow
            key={`${link.link_kind}:${link.target_kind}:${link.target_id}`}
            kind="Relates to"
            target={link.target?.title.split(' — ')[0] ?? `#${link.target_id}`}
            title={link.target ? (link.target.title.split(' — ').slice(1).join(' — ') || 'Related request') : 'Related request'}
            href={link.target && !link.target.gone && hrefFor ? hrefFor(link.target) : undefined}
            tag={link.target?.gone ? 'deleted' : undefined}
            tone={link.target?.gone ? 'gone' : undefined}
          />
        ))}

        {/*
          The approval counts, each its own unit so a reader (or a test) can find
          "1 awaiting a person" rather than reconstructing it from a run of text.
          An unapproved count is stated even when zero is not: "0 approved" beside
          "2 planned" is the sentence that says nothing here has been approved.
        */}
        <div className="flex items-center gap-2 flex-wrap text-[12px] text-ink-2">
          <span><span className="amp-num text-ink">{approval.total}</span> planned</span>
          {approval.total > 0 && (
            <>
              <span className="text-ink-3" aria-hidden="true">·</span>
              <span><span className="amp-num text-ink">{approval.approved}</span> approved</span>
              {approval.awaiting > 0 && (
                <>
                  <span className="text-ink-3" aria-hidden="true">·</span>
                  <span style={{ color: 'var(--link)' }}>
                    <span className="amp-num">{approval.awaiting}</span> awaiting a person
                  </span>
                </>
              )}
              {approval.gone > 0 && (
                <>
                  <span className="text-ink-3" aria-hidden="true">·</span>
                  <span className="text-ink-3"><span className="amp-num">{approval.gone}</span> re-planned</span>
                </>
              )}
            </>
          )}
          <span className="ml-auto amp-label !text-[9.5px]">{phaseWord(outcome.phase)}</span>
        </div>

        {/*
          The un-removable warning: between the rows and the actions, never
          behind a disclosure, and worded exactly as approved. It is the rail's
          reason to exist (contract §3: request text reaches agents as quoted
          untrusted data, and nothing approves or dispatches work).
        */}
        <RailBanner tone="warn" title="Handoff never auto-approves.">
          Plan items created from this request land as tickets awaiting a person&rsquo;s approval — the request text
          reaches agents as quoted, untrusted data. Nothing here dispatches work.
        </RailBanner>

        {closed && (
          <RailBanner tone="info" title="This request is closed.">
            {request.state === 'rejected'
              ? 'Reopen it before planning work for it: a rejection says the team is not going to do it.'
              : 'It was folded into another request; plan work on the canonical one.'}
          </RailBanner>
        )}

        {canHandoff ? (
          <div className="flex gap-2 flex-wrap">
            <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" onClick={onHandoff}>
              Create plan items…
            </button>
            {onChangeProject && routedProject && (
              <button type="button" className="amp-btn amp-btn-sm" onClick={onChangeProject}>Change project…</button>
            )}
          </div>
        ) : (
          <p className="m-0 flex items-start gap-1.5 text-[11.5px] text-ink-3 leading-[1.45]">
            <ShieldAlert size={12} className="flex-shrink-0 mt-[1px]" aria-hidden="true" />
            {!grouped.route
              ? 'Route the request to a project before planning items for it: a plan item has to live somewhere.'
              : 'Only the inbox team can plan items for this request.'}
          </p>
        )}

        {canPlan && onAttach && !closed && (
          <div className="border-t border-line-sub pt-3 flex flex-col gap-1.5">
            <span className="amp-label">Attach existing work</span>
            <div className="flex items-center gap-2 flex-wrap">
              <select
                className="amp-input !w-auto !text-[12.5px] !py-1"
                aria-label="What to attach"
                value={attachKind}
                onChange={e => setAttachKind(e.target.value as 'epic' | 'story' | 'task')}
              >
                <option value="task">A ticket</option>
                <option value="story">A story</option>
                <option value="epic">An epic</option>
              </select>
              <input
                className="amp-input !w-[110px] !text-[12.5px] !py-1"
                inputMode="numeric"
                aria-label={`The ${attachKind}'s id`}
                placeholder="id"
                value={attachId}
                onChange={e => setAttachId(e.target.value)}
              />
              <button
                type="button"
                className="amp-btn amp-btn-sm"
                disabled={busy || !attachId.trim()}
                onClick={attach}
              >
                {busy && <Loader2 size={11} className="animate-spin" aria-hidden="true" />}
                Attach
              </button>
            </div>
            <p className="m-0 text-[11.5px] text-ink-3 leading-[1.45]">
              Linking attaches the item to this request; it changes nothing about the item&rsquo;s own plan, approval
              or state. <ExternalLink size={10} className="inline align-baseline" aria-hidden="true" />
            </p>
          </div>
        )}

        {grouped.route && grouped.route.target && (
          <div>
            <RailTag tone="rev" title="Routed requests appear on the project's own request list; nothing is planned automatically.">
              {outcome.linked > 0 ? `${outcome.linked} linked` : 'routed, nothing planned'}
            </RailTag>
          </div>
        )}
      </div>
    </section>
  )
}
