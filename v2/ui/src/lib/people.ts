import type { ProjectAccess, ProjectMember, Task } from '../types'

// Shared helpers for showing people (ticket owners, epic/story owners,
// creators) and for the board's owner filter. Kept out of component files so
// react-refresh can hot-reload the components.

export interface Person {
  id: number
  name: string
  email: string
}

const ACCESS_RANK: Record<ProjectAccess, number> = {
  member: 0,
  lead: 1,
  owner: 2,
  org_admin: 3,
  global_admin: 4,
}

/** true when access is at least project owner (owner, org_admin or global_admin) */
export function canManageProject(access: ProjectAccess | null | undefined): boolean {
  return !!access && (ACCESS_RANK[access] ?? -1) >= ACCESS_RANK.owner
}

export function personName(p: { display_name?: string; email?: string }): string {
  return p.display_name?.trim() || p.email?.split('@')[0] || 'Unknown'
}

export function initials(name: string): string {
  const parts = name.replace(/[@._-]+/g, ' ').trim().split(/\s+/).filter(Boolean)
  if (parts.length === 0) return '?'
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase()
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
}

/** Build a user_id → Person lookup from the project members list. */
export function peopleById(members: ProjectMember[]): Map<number, Person> {
  return new Map(members.map(m => [m.user_id, { id: m.user_id, name: personName(m), email: m.email }]))
}

/** Resolve a user id to a Person, falling back to "User #id" for non-members. */
export function lookupPerson(people: Map<number, Person>, id: number | null | undefined): Person | null {
  if (id == null) return null
  return people.get(id) ?? { id, name: `User #${id}`, email: '' }
}

/**
 * Whether the caller may work a ticket (edit, schedule, change state, dispatch,
 * complete): its owner, anyone when it's unassigned, or org/global admins.
 * Team leads and project owners must reassign someone else's ticket first.
 * Unknown user id (local-dev mode) → allowed; the server stays authoritative.
 */
export function canWorkTask(task: Task, meId: number | undefined, access: ProjectAccess | null | undefined): boolean {
  const owner = task.owner_user_id ?? null
  if (owner === null || meId == null || owner === meId) return true
  return !!access && (ACCESS_RANK[access] ?? -1) >= ACCESS_RANK.org_admin
}

/**
 * Whether the caller may change how a ticket fits the plan — its text,
 * dependencies, story, metadata: its owner, its creator, anyone for an
 * unassigned ticket, and leads and up. (Working it stays owner-only.)
 */
export function canPlanTask(task: Task, meId: number | undefined, access: ProjectAccess | null | undefined): boolean {
  const owner = task.owner_user_id ?? null
  if (meId == null || owner === null || owner === meId || task.created_by === meId) return true
  return !!access && (ACCESS_RANK[access] ?? -1) >= ACCESS_RANK.lead
}

/** Leads, project owners and admins may take over someone else's ticket. */
export function canReassignTask(access: ProjectAccess | null | undefined): boolean {
  return !!access && (ACCESS_RANK[access] ?? -1) >= ACCESS_RANK.lead
}

/**
 * Whether the caller may reassign, release or take over this ticket when it
 * isn't theirs: leads and up always; any project member too when the project
 * has open reassign on and nobody is working the ticket.
 */
export function canTakeOverTask(task: Task, access: ProjectAccess | null | undefined, openReassign: boolean): boolean {
  if (canReassignTask(access)) return true
  return openReassign && !!access && task.state !== 'in_progress'
}

// ---- Board owner filter ----

/** 'mine' (default) | 'unassigned' | 'everyone' | a project member's user id */
export type OwnerFilter = 'mine' | 'unassigned' | 'everyone' | number

/**
 * Whether the owner filter actually narrows the view. "Mine" without a known
 * user id (local-dev mode, auth disabled) behaves like "Everyone".
 */
export function ownerFilterNarrows(filter: OwnerFilter, meId: number | undefined): boolean {
  if (filter === 'everyone') return false
  if (filter === 'mine') return meId != null
  return true
}

/**
 * "My work" = tickets I own, unassigned tickets I created, and legacy
 * unassigned tickets with no creator.
 */
export function taskMatchesOwner(task: Task, filter: OwnerFilter, meId: number | undefined): boolean {
  const owner = task.owner_user_id ?? null
  const creator = task.created_by ?? null
  switch (filter) {
    case 'everyone':
      return true
    case 'unassigned':
      return owner === null
    case 'mine':
      if (meId == null) return true
      return owner === meId || (owner === null && (creator === meId || creator === null))
    default:
      return owner === filter
  }
}
