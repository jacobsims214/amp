/**
 * The person's plan approval: which tickets wait on it, who planned them and
 * for which agent, one click to approve, and who approved last and when.
 *
 * Only a person in this web UI can approve. Agents (and a manager acting for
 * the person over MCP) are refused by the server and given a link here, so
 * this banner is where "waiting for your approval" lands.
 */
import { Link } from 'react-router-dom'
import { ArrowRight, Bot, Check, Loader2, ShieldCheck, UserRound } from 'lucide-react'
import { ago, formatWhen, planPath, plannedAt } from '../lib/approval'
import { lookupPerson, type Person } from '../lib/people'
import type { Project, Task } from '../types'

export interface PlanApprovalBannerProps {
  projectId: number
  project: Project | null
  /** open tickets planned since the last approval (awaitingApproval) */
  awaiting: Task[]
  people: Map<number, Person>
  onApprove: () => void
  approving: boolean
  error: string | null
  /** open a ticket (the drawer); without it ticket ids link to the board */
  onOpen?: (id: number) => void
  /** full: Flight plan (every ticket, and a quiet "approved" line when nothing waits).
   *  compact: the board (a few tickets, a link to the Flight plan, nothing when nothing waits). */
  variant?: 'full' | 'compact'
  /** for tests: what "now" is */
  now?: number
}

const COMPACT_ROWS = 3

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

export function PlanApprovalBanner({ projectId, project, awaiting, people, onApprove, approving, error, onOpen, variant = 'full', now }: PlanApprovalBannerProps) {
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
        <span className="text-[12.5px] text-ink-2"><LastApproved project={project} people={people} /> Nothing new has been planned since.</span>
      </section>
    )
  }

  const n = awaiting.length
  const rows = compact ? awaiting.slice(0, COMPACT_ROWS) : awaiting
  const ticketLink = (id: number) => `/project/${projectId}/board?task=${id}`
  return (
    <section id="approve" aria-label="Plan approval" className="amp-panel"
      style={{ boxShadow: `inset 3px 0 0 ${required ? 'var(--accent)' : 'var(--warning)'}` }}>
      <div className="flex flex-wrap items-start gap-x-4 gap-y-2 px-4 py-3">
        <div className="grid gap-1 min-w-0 flex-1 basis-[320px]">
          <span className="amp-label flex items-center gap-1.5"><ShieldCheck size={12} aria-hidden="true" />Plan approval</span>
          <h2 className="m-0 text-[15px] font-semibold text-ink">
            {n} ticket{n === 1 ? '' : 's'} {n === 1 ? 'waits' : 'wait'} for your approval
          </h2>
          <p className="m-0 text-[12.5px] leading-snug text-ink-2">
            {required
              ? <>Agents can't start {n === 1 ? 'it' : 'them'} until a person approves the plan. Only a person can, here in AMP: agents, and managers acting for you, can't approve.</>
              : <>This project doesn't require approval, so agents can already start {n === 1 ? 'it' : 'them'}. Approving still records that you've reviewed the plan.</>}
            {' '}<LastApproved project={project} people={people} />
          </p>
        </div>
        <div className="flex items-center gap-2 flex-wrap">
          {compact && (
            <Link to={`${planPath(projectId)}#approve`} className="amp-btn amp-btn-sm">Review in Flight plan<ArrowRight size={12} /></Link>
          )}
          <button type="button" className="amp-btn amp-btn-accent" disabled={approving} onClick={onApprove}>
            {approving ? <Loader2 size={12} className="animate-spin" /> : <Check size={13} />}Approve plan
          </button>
        </div>
        {error && <p role="alert" className="m-0 w-full text-[12.5px] text-danger-ink">{error}</p>}
      </div>

      <ul className="m-0 p-0 list-none border-t border-line" aria-label="Tickets waiting for approval">
        {rows.map(t => {
          const planner = t.created_by != null ? lookupPerson(people, t.created_by)?.name : null
          return (
            <li key={t.id} className="grid grid-cols-[auto_minmax(0,1fr)] sm:grid-cols-[auto_minmax(0,1fr)_auto] gap-x-3 gap-y-0.5 px-4 py-2 border-b border-line last:border-b-0 items-baseline">
              {onOpen
                ? <button type="button" onClick={() => onOpen(t.id)} className="amp-num text-[11.5px] text-ink-3 hover:text-ink underline-offset-2 hover:underline cursor-pointer">#{t.id}</button>
                : <Link to={ticketLink(t.id)} className="amp-num text-[11.5px] text-ink-3 hover:text-ink hover:underline">#{t.id}</Link>}
              <span className="text-[13px] text-ink truncate" title={t.name}>{t.name}</span>
              <span className="col-start-2 sm:col-start-auto text-[11.5px] text-ink-2 inline-flex flex-wrap items-center gap-x-2.5 gap-y-0.5 sm:justify-end">
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
        })}
        {compact && n > COMPACT_ROWS && (
          <li className="px-4 py-2 text-[12px] text-ink-3">
            and {n - COMPACT_ROWS} more · <Link to={`${planPath(projectId)}#approve`} className="text-ink underline underline-offset-2">see them all in Flight plan</Link>
          </li>
        )}
      </ul>
    </section>
  )
}
