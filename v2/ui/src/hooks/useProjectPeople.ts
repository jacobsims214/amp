import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import { peopleById, type Person } from '../lib/people'
import type { ProjectAccess, ProjectMember } from '../types'

/** A project's members, the viewer's access to it, and its reassign rule. */
export function useProjectPeople(projectId: number) {
  const [members, setMembers] = useState<ProjectMember[]>([])
  const [myAccess, setMyAccess] = useState<ProjectAccess | null>(null)
  const [openReassign, setOpenReassign] = useState(false)
  useEffect(() => {
    api.listProjectMembers(projectId)
      .then(r => { setMembers(r.members); setMyAccess(r.my_access) })
      .catch(() => { /* names fall back to "User #id"; the server still enforces access */ })
    api.getProject(projectId)
      .then(p => setOpenReassign(p.open_reassign ?? false))
      .catch(() => { /* only shapes buttons */ })
  }, [projectId])
  const people: Map<number, Person> = useMemo(() => peopleById(members), [members])
  return { members, myAccess, openReassign, people }
}
