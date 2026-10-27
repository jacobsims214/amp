/**
 * The person's plan approval (#2198: per ticket). Which tickets wait, split
 * into the person's own (ticked by default) and other people's, who planned
 * them and for which agent, and an Approve for what's ticked. Approving
 * someone else's ticket is "on behalf of" its owner: it needs project Lead
 * access or above and an explicit confirm.
 *
 * Only a person in this web UI can approve. Agents (and a manager acting for
 * the person over MCP) are refused by the server and given a link here, so
 * this banner is where "waiting for your approval" lands.
 */
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowRight, Bot, Check, Loader2, ShieldCheck, UserRound, Users } from 'lucide-react'
import { ago, approvalOwner, canApproveOnBehalf, formatWhen, listNames, newestPlannedAt, planPath, plannedAt, splitAwaiting } from '../lib/approval'
import { lookupPerson, type Person } from '../lib/people'
import type { Project, ProjectAccess, Task } from '../types'
import { projectLink } from '../lib/projectLink'

/** What the banner asks the page to approve. */
export interface ApproveSelection {
  /** exactly the tickets ticked */
  taskIds: number[]
  /** the newest planned_at among them, verbatim (#2192) */
  asOf: string | undefined
  /** some aren't the person's own */
  onBehalf: boolean
  /** every ticket the banner showed, so the page can tell what arrived after */
  shown: number[]
}

export interface PlanApprovalBannerProps {
  projectId: number
  project: Project | null
  /** open tickets whose plan nobody has approved (awaitingApproval) */
  awaiting: Task[]
  people: Map<number, Person>
  /** the signed-in person (undefined in local dev / while loading) */
  meId?: number | null
  /** their access to the project: Lead or above may approve on others' behalf */
  myAccess?: ProjectAccess | null
  onApprove: (sel: ApproveSelection) => void
  approving: boolean
  error: string | null
  /** the approval covered less than what waits now (tickets came in after the page loaded) */
  notice?: string | null
  /** open a ticket (the drawer); without it ticket ids link to the board */
  onOpen?: (id: number) => void
  /** full: Flight plan (every ticket, ticks, approve on behalf, and a quiet "approved" line when nothing waits).
   *  compact: the board (a few of your tickets, "Approve yours", others only as a link to the Flight plan).
   *  Compact offers Approve only when it lists all of your waiting tickets: a person approves what they've seen. */
  variant?: 'full' | 'compact'
  /** for tests: what "now" is */
  now?: number
}

/** The banner's ticks: your own are ticked unless unticked; others' only when ticked. */
export interface Ticks {
  /** your own tickets the person unticked */
  untickedYours: Set<number>
  /** other people's tickets the person ticked */
  tickedOthers: Set<number>
}

export interface PlanApprovalViewProps extends PlanApprovalBannerProps {
  ticks: Ticks
  onToggle: (id: number, yours: boolean) => void
  /** the person clicked "Approve on behalf of ..." and must confirm */
  confirming: boolean
  onConfirming: (on: boolean) => void
}

const COMPACT_ROWS = 3
const EMPTY_TICKS: Ticks = { untickedYours: new Set(), tickedOthers: new Set() }

/** "Jacob Sims", or what the server recorded when no person did (the upgrade's grandfathering). */
function approverName(project: Project, people: Map<number, Person>): string | null {
  if (project.plan_approved_by != null) return lookupPerson(people, project.plan_approved_by)?.name ?? null
  return project.plan_approved_at ? 'the AMP upgrade (plans in place then were kept as approved)' : null
}

export function LastApproved({ project, people }: { project: Project; people: Map<number, Person> }) {
  const when = formatWhen(project.plan_approved_at)
  const who = approverName(project, people)
  if (!when) return <>This plan has never been approved.</>
  return <>Last approved <span className="amp-num">{when}</span>{who ? <> by <span className="text-ink">{who}</span></> : null}.</>
}

/** Stateful wrapper: keeps the ticks and the confirm step; PlanApprovalView draws them. */
export function PlanApprovalBanner(props: PlanApprovalBannerProps) {
  const [ticks, setTicks] = useState<Ticks>(EMPTY_TICKS)
  const [confirming, setConfirming] = useState(false)
  const onToggle = (id: number, yours: boolean) => {
    setConfirming(false)
    setTicks(prev => {
      const key = yours ? 'untickedYours' : 'tickedOthers'
      const next = new Set(prev[key])
      if (next.has(id)) next.delete(id); else next.add(id)
      return { ...prev, [key]: next }
    })
  }
  const onApprove = (sel: ApproveSelection) => { setConfirming(false); props.onApprove(sel) }
  return <PlanApprovalView {...props} onApprove={onApprove} ticks={ticks} onToggle={onToggle} confirming={confirming} onConfirming={setConfirming} />
}

/** The owner's name, or who planned it when nobody owns it. */
function ownerLabel(t: Task, people: Map<number, Person>): string {
  const who = approvalOwner(t)
  if (who == null) return 'nobody'
  const name = lookupPerson(people, who)?.name ?? `User #${who}`
  return t.owner_user_id == null ? `${name} (unassigned, planned by them)` : name
}

function ownerName(t: Task, people: Map<number, Person>): string {
  const who = approvalOwner(t)
  return who == null ? 'nobody' : lookupPerson(people, who)?.name ?? `User #${who}`
}

export function PlanApprovalView({ projectId, project, awaiting, people, meId, myAccess, onApprove, approving, error, notice, onOpen, variant = 'full', now,
  ticks, onToggle, confirming, onConfirming }: PlanApprovalViewProps) {
  if (!project) return null
  const compact = variant === 'compact'
  const required = project.require_plan_approval !== false

  if (awaiting.length === 0) {
    if (compact) return null
    return (
      <section id="approve" aria-label="Plan approval" className="amp-panel flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5"
        style={{ boxShadow: 'inset 3px 0 0 var(--success)' }}>
        <ShieldCheck size={15} className="text-success-ink" aria-hidden="true" />
        <span className="text-[13px] font-semibold text-ink">Plan approved</span>
        <span className="text-[12.5px] text-ink-2"><LastApproved project={project} people={people} /> Every open ticket's plan is approved. Open a ticket to see who approved it.</span>
      </section>
    )
  }

  const { yours, others } = splitAwaiting(awaiting, meId)
  const lead = canApproveOnBehalf(myAccess)
  const ticketLink = (id: number) => projectLink(projectId, 'board', id)
  const flightPlan = `${planPath(projectId)}#approve`
  const accent = required ? 'var(--accent)' : 'var(--warning)'
  const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

  // The board: your own tickets only; others' are a link to the Flight plan.
  if (compact) {
    if (yours.length === 0) {
      return (
        <section id="approve" aria-label="Plan approval" className="amp-panel flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5"
          style={{ boxShadow: `inset 3px 0 0 ${accent}` }}>
          <Users size={14} className="text-ink-2" aria-hidden="true" />
          <span className="text-[12.5px] text-ink-2">
            {plural(others.length, "ticket waits for its owner's", "tickets wait for their owners'")} approval. None of them are yours.
          </span>
          <Link to={flightPlan} className="amp-btn amp-btn-sm ml-auto">See them in Flight plan<ArrowRight size={12} /></Link>
        </section>
      )
    }
    const rows = yours.slice(0, COMPACT_ROWS)
    // "Approve yours" covers exactly your tickets shown here, so it's offered only when all are shown.
    const all = rows.length === yours.length
    return (
      <section id="approve" aria-label="Plan approval" className="amp-panel" style={{ boxShadow: `inset 3px 0 0 ${accent}` }}>
        <div className="flex flex-wrap items-start gap-x-4 gap-y-2 px-4 py-3">
          <div className="grid gap-1 min-w-0 flex-1 basis-[320px]">
            <span className="amp-label flex items-center gap-1.5"><ShieldCheck size={12} aria-hidden="true" />Plan approval</span>
            <h2 className="m-0 text-[15px] font-semibold text-ink">
              {plural(yours.length, 'of your tickets waits', 'of your tickets wait')} for your approval
            </h2>
            <p className="m-0 text-[12.5px] leading-snug text-ink-2">
              {required
                ? <>Agents can't start {yours.length === 1 ? 'it' : 'them'} until you approve. Only a person can, here in AMP.</>
                : <>This project doesn't require approval; approving records that you've reviewed the plan.</>}
            </p>
          </div>
          <div className="flex items-center gap-2 flex-wrap">
            <Link to={flightPlan} className={all ? 'amp-btn amp-btn-sm' : 'amp-btn amp-btn-accent'}>
              Review{all ? '' : ` all ${yours.length} of yours`} in Flight plan<ArrowRight size={12} />
            </Link>
            {all && (
              <button type="button" className="amp-btn amp-btn-accent" disabled={approving}
                onClick={() => onApprove({ taskIds: rows.map(t => t.id), asOf: newestPlannedAt(rows), onBehalf: false, shown: rows.map(t => t.id) })}>
                {approving ? <Loader2 size={12} className="animate-spin" /> : <Check size={13} />}Approve yours ({rows.length})
              </button>
            )}
          </div>
          {notice && <p role="status" className="m-0 w-full text-[12.5px] text-warning-ink">{notice}</p>}
          {error && <p role="alert" className="m-0 w-full text-[12.5px] text-danger-ink">{error}</p>}
        </div>
        <ul className="m-0 p-0 list-none border-t border-line" aria-label="Your tickets waiting for approval">
          {rows.map(t => <TicketRow key={t.id} t={t} people={people} now={now} onOpen={onOpen} link={ticketLink(t.id)} />)}
          {yours.length > COMPACT_ROWS && (
            <li className="px-4 py-2 text-[12px] text-ink-3">
              and {yours.length - COMPACT_ROWS} more of yours · <Link to={flightPlan} className="text-ink underline underline-offset-2">see them all and approve in Flight plan</Link>
            </li>
          )}
          {others.length > 0 && (
            <li className="px-4 py-2 text-[12px] text-ink-3">
              {plural(others.length, "other person's ticket waits for its owner", "of other people's tickets wait for their owners")} · <Link to={flightPlan} className="text-ink underline underline-offset-2">see them in Flight plan</Link>
            </li>
          )}
        </ul>
      </section>
    )
  }

  // The Flight plan: every waiting ticket, yours ticked; others' tickable by leads, on their owners' behalf.
  const selectedYours = yours.filter(t => !ticks.untickedYours.has(t.id))
  const selectedOthers = lead ? others.filter(t => ticks.tickedOthers.has(t.id)) : []
  const selected = [...selectedYours, ...selectedOthers]
  const onBehalf = selectedOthers.length > 0
  const behalfNames = listNames(selectedOthers.map(t => ownerName(t, people)))
  const send = () => onApprove({ taskIds: selected.map(t => t.id), asOf: newestPlannedAt(selected), onBehalf, shown: awaiting.map(t => t.id) })
  const label = onBehalf ? `Approve on behalf of ${behalfNames} (${selected.length})` : `Approve selected (${selected.length})`

  return (
    <section id="approve" aria-label="Plan approval" className="amp-panel" style={{ boxShadow: `inset 3px 0 0 ${accent}` }}>
      <div className="flex flex-wrap items-start gap-x-4 gap-y-2 px-4 py-3">
        <div className="grid gap-1 min-w-0 flex-1 basis-[320px]">
          <span className="amp-label flex items-center gap-1.5"><ShieldCheck size={12} aria-hidden="true" />Plan approval</span>
          <h2 className="m-0 text-[15px] font-semibold text-ink">
            {yours.length > 0
              ? <>{plural(yours.length, 'of your tickets waits', 'of your tickets wait')} for your approval</>
              : <>None of your tickets wait for approval</>}
            {others.length > 0 && <span className="font-normal text-ink-2"> · {plural(others.length, "other person's waits", "of other people's wait")} for {others.length === 1 ? 'its owner' : 'their owners'}</span>}
          </h2>
          <p className="m-0 text-[12.5px] leading-snug text-ink-2">
            {required
              ? <>Agents can't start a ticket until a person approves it. Only a person can, here in AMP: agents, and managers acting for you, can't approve.</>
              : <>This project doesn't require approval, so agents can already start these. Approving still records that the plan was reviewed.</>}
            {' '}Approving covers your own tickets; anyone else's is approved on their behalf, which only project leads, owners and admins can do.
            {' '}<LastApproved project={project} people={people} />
          </p>
        </div>
        <div className="flex items-center gap-2 flex-wrap">
          {!confirming && (
            <button type="button" className="amp-btn amp-btn-accent" disabled={approving || selected.length === 0}
              title={selected.length === 0 ? 'Tick the tickets to approve' : undefined}
              onClick={() => onBehalf ? onConfirming(true) : send()}>
              {approving ? <Loader2 size={12} className="animate-spin" /> : <Check size={13} />}{label}
            </button>
          )}
        </div>
        {confirming && onBehalf && (
          <div role="alertdialog" aria-label="Confirm approving on behalf" className="w-full grid gap-2 px-3 py-2.5 border border-line-str bg-bg">
            <p className="m-0 text-[12.5px] text-ink">
              You're approving {plural(selectedOthers.length, 'ticket', 'tickets')} that {selectedOthers.length === 1 ? "isn't" : "aren't"} yours, on behalf of <b className="font-semibold">{behalfNames}</b>
              {selectedYours.length > 0 && <>, along with {plural(selectedYours.length, 'ticket', 'tickets')} of your own</>}.
              {' '}They haven't approved {selectedOthers.length === 1 ? 'it' : 'them'} themselves. Agents can start {selectedOthers.length === 1 ? 'it' : 'them'} once approved, and the ticket history records that you approved on their behalf.
            </p>
            <div className="flex items-center gap-2 flex-wrap">
              <button type="button" className="amp-btn amp-btn-accent" disabled={approving} onClick={send}>
                {approving ? <Loader2 size={12} className="animate-spin" /> : <Check size={13} />}Yes, approve on their behalf
              </button>
              <button type="button" className="amp-btn amp-btn-sm" onClick={() => onConfirming(false)}>Cancel</button>
            </div>
          </div>
        )}
        {notice && <p role="status" className="m-0 w-full text-[12.5px] text-warning-ink">{notice}</p>}
        {error && <p role="alert" className="m-0 w-full text-[12.5px] text-danger-ink">{error}</p>}
      </div>

      {yours.length > 0 && (
        <TicketGroup title="Yours" count={yours.length} hint="ticked: approving these is your own go">
          {yours.map(t => (
            <TicketRow key={t.id} t={t} people={people} now={now} onOpen={onOpen} link={ticketLink(t.id)}
              tick={{ checked: !ticks.untickedYours.has(t.id), onChange: () => onToggle(t.id, true), label: `Approve #${t.id} (yours)` }} />
          ))}
        </TicketGroup>
      )}
      {others.length > 0 && (
        <TicketGroup title="Others'" count={others.length}
          hint={lead ? 'tick one to approve it on its owner\'s behalf' : 'waiting for their owners'}>
          {!lead && (
            <li className="px-4 py-2 text-[12px] text-ink-2 border-b border-line">
              Only project leads, owners and admins can approve someone else's ticket. Ask its owner to approve it here, or a lead to approve it on their behalf.
            </li>
          )}
          {others.map(t => (
            <TicketRow key={t.id} t={t} people={people} now={now} onOpen={onOpen} link={ticketLink(t.id)} owner={ownerLabel(t, people)}
              tick={{
                checked: lead && ticks.tickedOthers.has(t.id), onChange: () => onToggle(t.id, false), disabled: !lead,
                label: lead ? `Approve #${t.id} on behalf of ${ownerName(t, people)}` : `#${t.id} is ${ownerName(t, people)}'s: only a project lead can approve it on their behalf`,
              }} />
          ))}
        </TicketGroup>
      )}
    </section>
  )
}

function TicketGroup({ title, count, hint, children }: { title: string; count: number; hint: string; children: React.ReactNode }) {
  return (
    <div className="border-t border-line">
      <div className="px-4 pt-2 pb-1 flex items-baseline gap-2">
        <span className="amp-label">{title} · <span className="amp-num">{count}</span></span>
        <span className="text-[11.5px] text-ink-3">{hint}</span>
      </div>
      <ul className="m-0 p-0 list-none" aria-label={title === 'Yours' ? 'Your tickets waiting for approval' : "Other people's tickets waiting for approval"}>{children}</ul>
    </div>
  )
}

function TicketRow({ t, people, now, onOpen, link, owner, tick }: {
  t: Task; people: Map<number, Person>; now?: number; onOpen?: (id: number) => void; link: string
  /** whose it is, for other people's tickets */
  owner?: string
  tick?: { checked: boolean; onChange: () => void; disabled?: boolean; label: string }
}) {
  const planner = t.created_by != null ? lookupPerson(people, t.created_by)?.name : null
  return (
    <li className={`grid ${tick ? 'grid-cols-[auto_auto_minmax(0,1fr)] sm:grid-cols-[auto_auto_minmax(0,1fr)_auto]' : 'grid-cols-[auto_minmax(0,1fr)] sm:grid-cols-[auto_minmax(0,1fr)_auto]'} gap-x-3 gap-y-0.5 px-4 py-2 border-b border-line last:border-b-0 items-baseline`}>
      {tick && (
        <input type="checkbox" className="accent-[var(--ink)] self-center" checked={tick.checked} disabled={tick.disabled}
          onChange={tick.onChange} aria-label={tick.label} title={tick.disabled ? tick.label : undefined} />
      )}
      {onOpen
        ? <button type="button" onClick={() => onOpen(t.id)} className="amp-num text-[11.5px] text-ink-3 hover:text-ink underline-offset-2 hover:underline cursor-pointer">#{t.id}</button>
        : <Link to={link} className="amp-num text-[11.5px] text-ink-3 hover:text-ink hover:underline">#{t.id}</Link>}
      <span className="text-[13px] text-ink truncate" title={t.name}>{t.name}</span>
      <span className={`${tick ? 'col-start-3' : 'col-start-2'} sm:col-start-auto text-[11.5px] text-ink-2 inline-flex flex-wrap items-center gap-x-2.5 gap-y-0.5 sm:justify-end`}>
        {owner && (
          <span className="inline-flex items-center gap-1 text-ink" title="Whose approval it waits for">
            <UserRound size={11} aria-hidden="true" />owner {owner}
          </span>
        )}
        <span className="inline-flex items-center gap-1" title="Who planned it (the manager runs as this person)">
          <UserRound size={11} aria-hidden="true" />planned by {planner ?? 'an agent'}
        </span>
        <span className="inline-flex items-center gap-1" title={t.performer === 'human' ? 'A person does this ticket' : 'The agent that will work it once approved'}>
          {t.performer === 'human' ? <UserRound size={11} aria-hidden="true" /> : <Bot size={11} aria-hidden="true" />}
          {t.performer === 'human' ? 'for a person' : `for ${t.assigned_to || 'an agent'}`}
        </span>
        <span className="amp-num text-ink-3">{ago(plannedAt(t), now)}</span>
      </span>
    </li>
  )
}
