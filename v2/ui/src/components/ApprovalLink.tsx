/**
 * The approval line on the Board and Flight plan (decisions/human-work-ux.md §2):
 * read-only, one line. "N tickets await your approval → My work", linking to
 * the project's My work tab with the oldest of them focused. Approving happens
 * only in My work; these views are for reading the plan.
 */
import { Link } from 'react-router-dom'
import { ArrowRight, ShieldCheck } from 'lucide-react'
import { splitAwaiting } from '../lib/approval'
import { projectLink } from '../lib/projectLink'
import type { Person } from '../lib/people'
import type { Project, Task } from '../types'
import { LastApproved } from './LastApproved'

export interface ApprovalLinkProps {
  projectId: number
  project: Project | null
  /** open tickets whose plan nobody has approved (awaitingApproval), oldest first */
  awaiting: Task[]
  /** the signed-in person (undefined in local dev / while loading) */
  meId?: number | null
  people: Map<number, Person>
  /** Flight plan: a quiet "Plan approved" line when nothing waits; the Board shows nothing */
  showApproved?: boolean
}

const tickets = (n: number) => `${n} ${n === 1 ? 'ticket' : 'tickets'}`

export function ApprovalLink({ projectId, project, awaiting, meId, people, showApproved = false }: ApprovalLinkProps) {
  if (!project) return null
  if (awaiting.length === 0) {
    if (!showApproved) return null
    return (
      <p id="approve" aria-label="Plan approval" className="m-0 flex flex-wrap items-center gap-x-2 gap-y-1 text-[12.5px] text-ink-2">
        <ShieldCheck size={13} className="text-success-ink" aria-hidden="true" />
        <span className="font-semibold text-ink">Plan approved.</span>
        <span><LastApproved project={project} people={people} /></span>
      </p>
    )
  }
  const { yours, others } = splitAwaiting(awaiting, meId)
  const othersText = others.length > 0
    ? `${tickets(others.length)} ${others.length === 1 ? 'awaits its owner' : 'await their owners'}`
    : null

  return (
    <p id="approve" aria-label="Plan approval" className="m-0 flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px] text-ink-2">
      <ShieldCheck size={13} className="text-violet" aria-hidden="true" />
      {yours.length > 0 ? (
        <Link to={projectLink(projectId, 'my-work', yours[0].id)}
          className="inline-flex items-center gap-1 font-semibold text-ink underline underline-offset-2 decoration-line-str hover:decoration-ink">
          {tickets(yours.length)} {yours.length === 1 ? 'awaits' : 'await'} your approval<ArrowRight size={12} aria-hidden="true" /><span>My work</span>
        </Link>
      ) : (
        <span>None of your tickets await approval.</span>
      )}
      {othersText && <span>· {othersText}</span>}
    </p>
  )
}
