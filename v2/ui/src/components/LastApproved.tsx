/**
 * "Last approved Mon, Oct 27, 13:00 by Jacob Sims." The project's last plan
 * approval, informational since #2198 (each ticket carries its own). Used by
 * the Flight plan's approval line and project settings.
 */
import { formatWhen } from '../lib/approval'
import { lookupPerson, type Person } from '../lib/people'
import type { Project } from '../types'

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
