/**
 * One ticket as a full page, /project/:projectId/ticket/:ticketId
 * (v2/docs/url-state.md). It is the ticket drawer's own sections (TaskDrawer
 * with layout="page"): the same tabs, owner, runs, artifacts and history,
 * laid out in a wide column. The drawer's "Open full page" links here; "Back
 * to board" returns to the board with the drawer open.
 *
 * A ticket that is in another project redirects to its own project's URL; one
 * that doesn't exist (404) or that the viewer can't see (403) says so.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, Navigate, useLocation, useNavigate, useParams } from 'react-router-dom'
import { LockKeyhole, SearchX } from 'lucide-react'
import { api, type HttpError } from '../api/client'
import { TaskDrawer } from '../components/TaskDrawer'
import { Skeleton, SkeletonText } from '../components/ui/Skeleton'
import { useAuth } from '../hooks/useAuth'
import { useDelayedFlag } from '../hooks/useDelayedFlag'
import { useSSE } from '../hooks/useSSE'
import { projectLink, ticketPageLink } from '../lib/projectLink'
import type { ProjectAccess, ProjectMember, SSEEvent, Task } from '../types'

function LoadFailed({ id, status, onRetry }: { id: string; status?: number; onRetry: () => void }) {
  const expired = status === 401
  return (
    <div className="px-7 py-10 max-w-[640px]" role="alert">
      <span className="amp-label">Couldn't load</span>
      <h1 className="amp-heading m-0 mt-2 text-[22px] text-ink">Ticket {id} didn't load</h1>
      <p className="text-[13px] text-ink-2 mt-2">
        {expired ? 'Your session may have expired. Sign in again, then retry.' : 'Something went wrong reaching the server. The ticket may be fine.'}{' '}
        <button type="button" onClick={onRetry} className="underline">Try again</button>
      </p>
    </div>
  )
}

function Unavailable({ id, forbidden }: { id: string; forbidden: boolean }) {
  const Icon = forbidden ? LockKeyhole : SearchX
  return (
    <div className="px-7 py-10 max-w-[640px]">
      <span className="amp-label">{forbidden ? 'No access' : 'Not found'}</span>
      <h1 className="amp-heading m-0 mt-2 text-[22px] text-ink flex items-center gap-2.5">
        <Icon size={18} className="text-ink-3" aria-hidden="true" />
        {forbidden ? `You don't have access to ticket ${id}` : `Ticket ${id} was not found, or you can't see it`}
      </h1>
      <p className="text-[13px] text-ink-2 mt-2">
        {forbidden ? "It's in a project you aren't a member of. Ask its owner to add you, or " : 'It may have been deleted. '}
        <Link to="/projects" className="underline">open a project you can see</Link>.
      </p>
    </div>
  )
}

function TicketPageSkeleton() {
  return (
    <div className="mx-auto w-full max-w-[1080px]" data-testid="ticket-page-loading" aria-busy="true">
      <div className="px-6 pt-5 pb-4 border-b border-line grid gap-3">
        <Skeleton w={90} h={10} />
        <Skeleton w="55%" h={20} />
      </div>
      <div className="px-6 py-4 border-b border-line"><Skeleton w="40%" h={10} /></div>
      <div className="p-6 grid gap-4"><SkeletonText lines={4} /><SkeletonText lines={3} /></div>
    </div>
  )
}

const NO_MEMBERS: ProjectMember[] = []

export function TicketPage() {
  const { projectId = '', ticketId = '' } = useParams()
  const navigate = useNavigate()
  const { search } = useLocation()
  const { me } = useAuth()
  const id = Number(ticketId)
  const valid = Number.isSafeInteger(id) && id > 0
  const [loaded, setLoaded] = useState<{ id: number; task: Task | null; status?: number } | null>(null)
  // Per-project data, tagged with the project it was read for, so another
  // project's role and members never drive this ticket's controls.
  const [proj, setProj] = useState<{ pid: number; members: ProjectMember[]; myAccess: ProjectAccess | null } | null>(null)
  const [projSettings, setProjSettings] = useState<{ pid: number; openReassign: boolean; flagMinutes?: number } | null>(null)
  const task = loaded?.id === id ? loaded.task : null
  const pid = task?.project_id

  // A read for an id the page has since left must not land (it would replace the new ticket).
  const read = useCallback((live: () => boolean) => {
    api.getTask(id)
      .then(t => { if (live()) setLoaded({ id, task: t }) })
      .catch((e: HttpError) => { if (live()) setLoaded(prev => prev?.id === id && prev.task ? prev : { id, task: null, status: e.status }) })
  }, [id])
  const current = useRef(id)
  useEffect(() => { current.current = id }, [id])
  const refresh = useCallback(() => { if (valid) read(() => current.current === id) }, [id, valid, read])
  useEffect(() => {
    if (!valid) return
    let live = true
    read(() => live)
    return () => { live = false }
  }, [valid, read])

  // A page is left open: re-read when this ticket changes elsewhere (an agent
  // starts or finishes it, someone reassigns or edits it).
  useSSE(pid ?? null, useCallback((e: SSEEvent) => {
    const p = e.payload as { id?: unknown; task_id?: unknown } | null
    if (!e.type.startsWith('task.') && !e.type.startsWith('run.')) return
    if (p && (p.id === id || p.task_id === id)) refresh()
  }, [id, refresh]))

  useEffect(() => {
    if (pid == null) return
    let live = true
    api.listProjectMembers(pid).then(r => { if (live) setProj({ pid, members: r.members, myAccess: r.my_access }) }).catch(() => {})
    api.getProject(pid).then(p => { if (live) setProjSettings({ pid, openReassign: p.open_reassign ?? false, flagMinutes: p.not_started_flag_minutes }) }).catch(() => {})
    return () => { live = false }
  }, [pid])
  const members = proj && proj.pid === pid ? proj.members : NO_MEMBERS
  const myAccess = proj && proj.pid === pid ? proj.myAccess : null
  const openReassign = projSettings && projSettings.pid === pid ? projSettings.openReassign : false
  const flagMinutes = projSettings && projSettings.pid === pid ? projSettings.flagMinutes : undefined

  const waiting = useDelayedFlag(valid && loaded?.id !== id)
  if (!valid) return <Unavailable id={`"${ticketId}"`} forbidden={false} />
  if (loaded?.id === id && !loaded.task) {
    // Only a 404 or 403 says anything about the ticket; any other failure (500, 401, network) is a load failure.
    if (loaded.status === 404 || loaded.status === 403) return <Unavailable id={`#${id}`} forbidden={loaded.status === 403} />
    return <LoadFailed id={`#${id}`} status={loaded.status} onRetry={() => { setLoaded(null); refresh() }} />
  }
  if (!task) return waiting ? <TicketPageSkeleton /> : null
  // Ids are global: a ticket under another project's URL opens under its own.
  if (String(task.project_id) !== projectId) return <Navigate to={ticketPageLink(task.project_id, task.id, search.slice(1))} replace />

  return (
    <div className="h-full overflow-y-auto">
      <TaskDrawer
        layout="page"
        task={task}
        onClose={() => navigate(projectLink(task.project_id, 'board'))}
        onRefresh={refresh}
        onTaskUpdated={t => setLoaded({ id, task: t })}
        members={members}
        meId={me?.user_id}
        myAccess={myAccess}
        openReassign={openReassign}
        notStartedFlagMinutes={flagMinutes}
      />
    </div>
  )
}
