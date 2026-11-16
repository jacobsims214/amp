/**
 * A request's history (AMP #3979; the `History` section of approved mockup 37
 * v1, and the `Activity` list artifact 38 v1 shows a requester).
 *
 * The history is the append-only `request_events` table: exactly one row per act
 * (I6), newest last, and nothing may edit or delete a row. This component renders
 * that record and adds no fact of its own — no inferred state change, no "work
 * started" for a linked ticket that closed (I8), no row for something that did
 * not happen.
 *
 * Two properties matter more than the layout:
 *
 *   1. **Rows are built from the row's own fields.** An `actor` label comes from
 *      the event the server recorded, never from a client-side user lookup, so a
 *      display name that changed (or a person the viewer may not see) cannot be
 *      conjured. The `detail` JSON is parsed only for the few documented shapes
 *      (`link`/`unlink` kinds, an `edit`'s field list) and never rendered raw.
 *   2. **A requester's timeline is its own arm.** `RequesterEvent`-shaped input is
 *      not what the projection carries today (the requester sees milestone
 *      timestamps and comments), so the requester arm renders the milestones the
 *      projection *does* carry, and the team arm renders `request_events`. There
 *      is no arm in which internal actors, claims or assignee transfers can reach
 *      a requester's page.
 *
 * `verbose` is the mockup's own split: the team's rail lists every act with its
 * actor, and a compact requester list shows only the milestones that happened.
 */
import { useState } from 'react'
import { ChevronDown, ChevronRight, History as HistoryIcon } from 'lucide-react'
import { RequestPanelEmpty, RequestPanelSection } from './RequestPanelSection'
import { REQUEST_STATE_WORD, requestEventSentence, requestEventTime } from '../../lib/requestLabels'
import { isRelayed, relayedFor } from '../../lib/approval'
import type { RequestEvent, RequestStatus, RequesterMilestone, RequesterMilestoneKey } from '../../types'

/** The team's history: the append-only event rows. */
export interface TeamHistoryProps {
  mode: 'team'
  events: RequestEvent[]
  /** How many rows to show before "Show all". Absent shows every row. */
  collapsedAfter?: number
}

/** The requester's activity: the projection's milestones, in the order they happened. */
export interface RequesterHistoryProps {
  mode: 'requester'
  milestones: RequesterMilestone[]
}

export type RequestHistoryProps = TeamHistoryProps | RequesterHistoryProps

/** What a milestone is called on the requester's page (artifact 38 v1's stepper words). */
const MILESTONE_WORD: Record<RequesterMilestoneKey, string> = {
  received: 'Request received',
  reviewing: 'Being reviewed',
  in_progress: 'In progress',
  delivered: 'Delivered',
  accepted: 'You confirmed it',
}

/** The dot colour per act: a transition carries the destination state's colour, everything else is quiet. */
const ACTION_VAR: Record<string, string> = {
  submit: 'var(--ink)',
  transition: 'var(--st-prog)',
  claim: 'var(--band-2)',
  release: 'var(--st-wait)',
  assign: 'var(--st-prog)',
  link: 'var(--ink-2)',
  unlink: 'var(--st-wait)',
  edit: 'var(--line-str)',
}

const STATE_VAR: Record<RequestStatus, string> = {
  new: 'var(--st-ready)',
  triaging: 'var(--band-2)',
  needs_information: 'var(--st-flag)',
  accepted: 'var(--st-prog)',
  in_progress: 'var(--st-prog)',
  delivered: 'var(--success)',
  requester_accepted: 'var(--ink)',
  rejected: 'var(--st-flag)',
  duplicate: 'var(--st-wait)',
}

function EventRow({ event, last }: { event: RequestEvent; last: boolean }) {
  const relayed = isRelayed(event.actor)
  const actor = relayed ? relayedFor(event.actor) : event.actor.trim()
  const dot = event.action === 'transition' && event.to_status ? STATE_VAR[event.to_status] : ACTION_VAR[event.action] ?? 'var(--line-str)'
  const sentence = requestEventSentence(event)
  const exact = new Date(event.created_at).toLocaleString()
  return (
    <li className="flex gap-2.5 min-w-0" data-testid="history-row" data-action={event.action}>
      <span className="flex flex-col items-center flex-shrink-0 pt-1.5" aria-hidden="true">
        <i className="block w-1.5 h-1.5" style={{ background: dot, borderRadius: 1 }} />
        {!last ? <span className="w-px flex-1 bg-line-sub mt-1" /> : null}
      </span>
      <div className="min-w-0 flex-1 pb-2.5">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
          <span className="text-[12.5px] text-ink-2 min-w-0 break-words" data-testid="history-sentence">
            <b className="font-semibold text-ink">{actor || 'Someone'}</b> {sentence}.
          </span>
          {relayed ? (
            <span className="font-label text-[9px] font-semibold uppercase tracking-[0.1em] px-1 py-[1px] border border-dashed border-line-str text-ink-2"
              title="An agent passed this on for the person over MCP.">
              Relayed
            </span>
          ) : null}
          <time className="ml-auto text-[11px] text-ink-3 flex-shrink-0 tabular-nums" dateTime={event.created_at} title={exact}>
            {requestEventTime(event.created_at)}
          </time>
        </div>
        {event.action === 'transition' && event.from_status && event.to_status ? (
          <div className="mt-0.5 font-mono text-[10.5px] text-ink-3" data-testid="history-states">
            {REQUEST_STATE_WORD[event.from_status]} → {REQUEST_STATE_WORD[event.to_status]}
          </div>
        ) : null}
      </div>
    </li>
  )
}

function TeamHistory({ events, collapsedAfter = 6 }: TeamHistoryProps) {
  const [all, setAll] = useState(false)
  const over = collapsedAfter > 0 && events.length > collapsedAfter
  const shown = all || !over ? events : events.slice(-collapsedAfter)
  return (
    <RequestPanelSection
      label="History"
      note="Append-only: one row per act"
      actions={<span className="text-[11.5px] text-ink-3 tabular-nums">{events.length} {events.length === 1 ? 'row' : 'rows'}</span>}
    >
      {events.length === 0 ? (
        <RequestPanelEmpty>No acts recorded yet.</RequestPanelEmpty>
      ) : (
        <ol className="list-none m-0 p-0" aria-label="Request history">
          {shown.map((e, i) => <EventRow key={e.id} event={e} last={i === shown.length - 1} />)}
        </ol>
      )}
      {over ? (
        <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm self-start" onClick={() => setAll(v => !v)}
          aria-expanded={all}>
          {all ? <ChevronDown size={11} aria-hidden="true" /> : <ChevronRight size={11} aria-hidden="true" />}
          {all ? 'Show the last few' : `Show all ${events.length}`}
        </button>
      ) : null}
    </RequestPanelSection>
  )
}

function RequesterHistory({ milestones }: RequesterHistoryProps) {
  // Only the steps that happened: the stepper above already shows what is
  // pending, and repeating "not yet" in a list of events would read as work
  // that has started when the state says otherwise.
  const done = milestones.filter(m => m.done)
  return (
    <RequestPanelSection
      label="Activity"
      note="What has happened so far"
      actions={<HistoryIcon size={12} className="text-ink-3" aria-hidden="true" />}
    >
      {done.length === 0 ? (
        <RequestPanelEmpty>Nothing yet.</RequestPanelEmpty>
      ) : (
        <ol className="list-none m-0 p-0" aria-label="Request activity">
          {done.map((m, i) => (
            <li key={m.key} className="flex gap-2.5 min-w-0" data-testid="history-row" data-milestone={m.key}>
              <span className="flex flex-col items-center flex-shrink-0 pt-1.5" aria-hidden="true">
                <i className="block w-1.5 h-1.5" style={{ background: m.key === 'accepted' ? 'var(--ink)' : 'var(--st-prog)', borderRadius: 1 }} />
                {i < done.length - 1 ? <span className="w-px flex-1 bg-line-sub mt-1" /> : null}
              </span>
              <div className="min-w-0 flex-1 pb-2.5">
                <div className="flex flex-wrap items-baseline gap-x-2">
                  <span className="text-[12.5px] font-semibold text-ink">{MILESTONE_WORD[m.key] ?? 'Updated'}</span>
                  {m.at ? (
                    <time className="ml-auto text-[11px] text-ink-3 flex-shrink-0 tabular-nums" dateTime={m.at} title={new Date(m.at).toLocaleString()}>
                      {requestEventTime(m.at)}
                    </time>
                  ) : null}
                </div>
                {m.note ? <p className="m-0 mt-0.5 text-[12px] text-ink-2 leading-snug">{m.note}</p> : null}
              </div>
            </li>
          ))}
        </ol>
      )}
    </RequestPanelSection>
  )
}

/** A request's history. The `mode` discriminant picks the row vocabulary: events for the team, milestones for its requester. */
export function RequestHistory(props: RequestHistoryProps) {
  return props.mode === 'requester' ? <RequesterHistory {...props} /> : <TeamHistory {...props} />
}
