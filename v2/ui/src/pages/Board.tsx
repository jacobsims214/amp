import { useState, useMemo, useEffect } from 'react'
import { useParams } from 'react-router-dom'
import { RefreshCw, Loader2, WifiOff, ChevronDown, ChevronRight, EyeOff, Eye, GitBranch, MoreHorizontal, Plus, Users, ListChecks } from 'lucide-react'
import { useBoardData } from '../hooks/useBoardData'
import { useSSE } from '../hooks/useSSE'
import { useAuth } from '../hooks/useAuth'
import { TaskCard } from '../components/TaskCard'
import { TaskDrawer } from '../components/TaskDrawer'
import { FilterBar } from '../components/FilterBar'
import { DEFAULT_FILTER, type FilterState } from '../lib/boardFilter'
import { CrudModal } from '../components/CrudModal'
import { MetadataEditor } from '../components/MetadataEditor'
import { DependencyPicker } from '../components/DependencyPicker'
import { PlanCheckModal } from '../components/PlanCheckModal'
import { metadataPatch } from '../lib/metadata'
import { ConfirmDeleteModal } from '../components/ConfirmDeleteModal'
import { ProjectNav } from '../components/ProjectNav'
import { OwnerStack } from '../components/OwnerAvatar'
import { OwnersEditor } from '../components/OwnersEditor'
import { TeamAccessModal } from '../components/TeamAccessModal'
import { api } from '../api/client'
import { withClears } from '../lib/clear'
import { lookupPerson, ownerFilterNarrows, peopleById, personName, taskMatchesOwner, type Person } from '../lib/people'
import type { Task, TaskState, Epic, Story, ProjectMember, ProjectAccess, Metadata } from '../types'

// Label column width and task column min-width are tuned for responsiveness.
// Label col: 180px | task col: 168px → total minimum = 180 + 5×168 = 1020px (fits 13" screens)
const LABEL_COL = 180
const TASK_COL  = 200

const COLUMNS: {
  state: TaskState
  label: string
  color: string
  countBg: string
  topBorder: string
}[] = [
  { state: 'scheduled',   label: 'Scheduled',             color: 'var(--st-sched)', countBg: 'transparent', topBorder: 'var(--st-sched)' },
  { state: 'backlog',     label: 'Ready',                 color: 'var(--st-ready)', countBg: 'transparent', topBorder: 'var(--st-ready)' },
  { state: 'in_progress', label: 'In progress',           color: 'var(--st-prog)',  countBg: 'transparent', topBorder: 'var(--st-prog)' },
  { state: 'blocked',     label: 'Waiting & questions',   color: 'var(--st-flag)',  countBg: 'transparent', topBorder: 'var(--st-flag)' },
  { state: 'completed',   label: 'Done',                  color: 'var(--st-done)',  countBg: 'transparent', topBorder: 'var(--st-done)' },
]

function isScheduledTask(task: Task): boolean {
  return task.state === 'blocked' && task.block_reason?.startsWith('scheduled:') === true
}

const inputCls =
  'w-full bg-bg border border-line rounded-lg px-3 py-2 text-sm text-ink placeholder-ink-3 ' +
  'focus:outline-none focus:border-primary focus:ring-2 focus:ring-primary/20 transition-all font-[inherit]'

const labelCls = 'text-xs font-medium text-ink-2 block mb-1.5'

// owner: '' = me (server default), 'none' = unassigned, or a member's user id
const EMPTY_TASK_FORM = { name: '', description: '', acceptance_criteria: '', assigned_to: 'amp-worker', priority: '1', owner: '', dependency_ids: [] as number[] }

export function Board() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)

  const { epics, stories, tasks, loading, error, refresh, applyTask, applyEpic, applyStory } = useBoardData(pid)
  const { me } = useAuth()
  const meId = me?.user_id
  // Track the selected task by id so the drawer reflects live updates
  // (assign responses, SSE) instead of a stale snapshot.
  const [selectedTaskId, setSelectedTaskId] = useState<number | null>(null)
  const selectedTask = selectedTaskId === null ? null : tasks.find(t => t.id === selectedTaskId) ?? null
  const [filter, setFilter] = useState<FilterState>(DEFAULT_FILTER)
  const [collapsedEpics, setCollapsedEpics] = useState<Set<number>>(new Set())
  const [collapsedStories, setCollapsedStories] = useState<Set<number>>(new Set())
  const [hideCompleted, setHideCompleted] = useState(true)
  const [liveStatus, setLiveStatus] = useState<'connected' | 'disconnected'>('disconnected')

  // Project members: names for owners/creators, the owner filter, and assignment.
  const [members, setMembers] = useState<ProjectMember[]>([])
  const [myAccess, setMyAccess] = useState<ProjectAccess | null>(null)
  const [openReassign, setOpenReassign] = useState(false)
  const [teamAccessOpen, setTeamAccessOpen] = useState(false)
  const people = useMemo(() => peopleById(members), [members])
  const ownersOf = (ids: number[] | undefined): Person[] =>
    (ids ?? []).map(id => lookupPerson(people, id)!)

  useEffect(() => {
    api.listProjectMembers(pid)
      .then(r => { setMembers(r.members); setMyAccess(r.my_access) })
      .catch(() => { /* names fall back to "User #id"; actions still surface API errors */ })
    api.getProject(pid)
      .then(p => setOpenReassign(p.open_reassign ?? false))
      .catch(() => { /* the server still enforces the rule; this only shapes buttons */ })
  }, [pid])

  // Epic CRUD
  const [epicModal, setEpicModal] = useState<{ mode: 'create' | 'edit'; epic?: Epic } | null>(null)
  const [epicDeleteTarget, setEpicDeleteTarget] = useState<Epic | null>(null)
  const [epicForm, setEpicForm] = useState({ name: '', description: '', priority: '1' })
  const [epicSaving, setEpicSaving] = useState(false)
  const [epicError, setEpicError] = useState<string | null>(null)
  const [openEpicMenu, setOpenEpicMenu] = useState<number | null>(null)

  // Story CRUD
  const [storyModal, setStoryModal] = useState<{ mode: 'create' | 'edit'; story?: Story; epicId?: number } | null>(null)
  const [storyDeleteTarget, setStoryDeleteTarget] = useState<Story | null>(null)
  const [storyForm, setStoryForm] = useState({ name: '', description: '', acceptance_criteria: '', priority: '1' })
  // The epic a story being edited should live in (moving it on save).
  const [storyEpicId, setStoryEpicId] = useState<number | null>(null)
  const [planCheckOpen, setPlanCheckOpen] = useState(false)
  const [storySaving, setStorySaving] = useState(false)
  const [storyError, setStoryError] = useState<string | null>(null)
  const [openStoryMenu, setOpenStoryMenu] = useState<number | null>(null)

  // Task create
  const [taskCreateModal, setTaskCreateModal] = useState<{ story: Story } | null>(null)
  const [taskForm, setTaskForm] = useState({ ...EMPTY_TASK_FORM })
  const [taskSaving, setTaskSaving] = useState(false)
  const [taskError, setTaskError] = useState<string | null>(null)

  // The epic/story being edited, read live from board data so owner changes show immediately.
  const editingEpic = epicModal?.epic ? epics.find(e => e.id === epicModal.epic!.id) ?? epicModal.epic : null
  const editingStory = storyModal?.story ? stories.find(s => s.id === storyModal.story!.id) ?? storyModal.story : null

  const completedEpicIds = useMemo(() => {
    return new Set(
      epics.filter(epic => {
        const epicTasks = tasks.filter(t => t.epic_id === epic.id)
        return epicTasks.length > 0 && epicTasks.every(t => t.state === 'completed')
      }).map(e => e.id)
    )
  }, [epics, tasks])

  useSSE(pid, (event) => {
    if (event.type === 'connected') setLiveStatus('connected')
  })

  useEffect(() => {
    if (openEpicMenu === null) return
    const h = () => setOpenEpicMenu(null)
    document.addEventListener('click', h)
    return () => document.removeEventListener('click', h)
  }, [openEpicMenu])

  useEffect(() => {
    if (openStoryMenu === null) return
    const h = () => setOpenStoryMenu(null)
    document.addEventListener('click', h)
    return () => document.removeEventListener('click', h)
  }, [openStoryMenu])

  // Owner filter hides other people's tickets outright (default: my work);
  // query/state/epic filters then dim non-matching cards as before.
  const ownerNarrows = ownerFilterNarrows(filter.owner, meId)
  const ownerTasks = useMemo(
    () => ownerNarrows ? tasks.filter(t => taskMatchesOwner(t, filter.owner, meId)) : tasks,
    [tasks, filter.owner, meId, ownerNarrows],
  )

  const q = filter.query.toLowerCase()
  const filteredTasks = useMemo(() => {
    return ownerTasks.filter(t => {
      const displayState = isScheduledTask(t) ? 'scheduled' : t.state
      if (filter.state && displayState !== filter.state) return false
      if (filter.epicId !== null && t.epic_id !== filter.epicId) return false
      if (q) {
        const story = stories.find(s => s.id === t.story_id)
        const epic = epics.find(e => e.id === t.epic_id)
        const hay = `${t.name} ${t.description} #${t.id} ${story?.name ?? ''} ${epic?.name ?? ''}`.toLowerCase()
        if (!hay.includes(q)) return false
      }
      return true
    })
  }, [ownerTasks, filter, q, stories, epics])

  const filteredTaskIds = useMemo(() => new Set(filteredTasks.map(t => t.id)), [filteredTasks])
  const hasFieldFilter = !!(filter.query || filter.state || filter.epicId !== null)
  const hasFilter = hasFieldFilter || ownerNarrows
  // In "Mine" view (with no other narrowing), epics/stories I own stay visible
  // even with no visible tickets, so a freshly created empty epic shows up.
  const showOwnedEmpty = filter.owner === 'mine' && meId != null && !filter.query && !filter.state
  const iOwn = (ids: number[] | undefined) => meId != null && !!ids?.includes(meId)

  const counts = useMemo(() => ({
    scheduled:   filteredTasks.filter(t => isScheduledTask(t)).length,
    backlog:     filteredTasks.filter(t => t.state === 'backlog').length,
    in_progress: filteredTasks.filter(t => t.state === 'in_progress').length,
    blocked:     filteredTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t)).length,
    completed:   filteredTasks.filter(t => t.state === 'completed').length,
    total:       filteredTasks.length,
  }), [filteredTasks])

  const toggleEpic = (id: number) =>
    setCollapsedEpics(prev => { const s = new Set(prev); s.has(id) ? s.delete(id) : s.add(id); return s })
  const toggleStory = (id: number) =>
    setCollapsedStories(prev => { const s = new Set(prev); s.has(id) ? s.delete(id) : s.add(id); return s })

  const errMsg = (e: unknown, fallback: string) => e instanceof Error ? e.message : fallback

  // Metadata being edited in the epic / story dialog: what it started as,
  // the edited value, and what the item inherits (shown read-only).
  type MetaDraft = { initial?: Metadata; value: Metadata; error: string | null; inherited?: Metadata }
  const [epicMeta, setEpicMeta] = useState<MetaDraft>({ value: {}, error: null })
  const [storyMeta, setStoryMeta] = useState<MetaDraft>({ value: {}, error: null })
  const startMeta = (set: React.Dispatch<React.SetStateAction<MetaDraft>>, own: Metadata | undefined, inherited: Promise<Metadata>) => {
    set({ initial: own ?? {}, value: own ?? {}, error: null })
    // Keep any edits already made while the inherited fields load.
    inherited.then(inh => set(d => ({ ...d, inherited: inh }))).catch(() => {})
  }
  const projectMeta = () => api.getProject(pid).then(p => p.metadata ?? {})

  const openEpicModal = (m: { mode: 'create' | 'edit'; epic?: Epic }) => {
    setEpicError(null); setEpicModal(m)
    startMeta(setEpicMeta, m.epic?.metadata, m.epic ? api.inheritedMetadata('epic', m.epic.id) : projectMeta())
  }
  const openStoryModal = (m: { mode: 'create' | 'edit'; story?: Story; epicId?: number }) => {
    setStoryError(null); setStoryModal(m); setStoryEpicId(m.story?.epic_id ?? null)
    const parent = m.epicId != null ? epics.find(e => e.id === m.epicId) : undefined
    startMeta(setStoryMeta, m.story?.metadata, m.story
      ? api.inheritedMetadata('story', m.story.id)
      : parent ? api.inheritedMetadata('epic', parent.id).then(inh => ({ ...inh, ...(parent.metadata ?? {}) })) : projectMeta())
  }

  const handleEpicSave = async () => {
    if (!epicForm.name.trim()) return
    if (epicMeta.error) { setEpicError(epicMeta.error); return }
    setEpicSaving(true)
    setEpicError(null)
    try {
      if (epicModal?.mode === 'create') await api.createEpic(pid, { ...epicForm, metadata: epicMeta.value })
      else if (epicModal?.epic) await api.updateEpic(epicModal.epic.id, { ...withClears(epicForm, ['description']), metadata: metadataPatch(epicMeta.initial, epicMeta.value) })
      setEpicModal(null); refresh()
    } catch (e) {
      setEpicError(errMsg(e, 'Save failed'))
    } finally { setEpicSaving(false) }
  }

  const handleEpicDelete = async () => {
    if (!epicDeleteTarget) return
    setEpicSaving(true)
    setEpicError(null)
    try { await api.deleteEpic(epicDeleteTarget.id); setEpicDeleteTarget(null); refresh() }
    catch (e) { setEpicError(errMsg(e, 'Delete failed')) }
    finally { setEpicSaving(false) }
  }

  const handleStorySave = async () => {
    if (!storyForm.name.trim()) return
    if (storyMeta.error) { setStoryError(storyMeta.error); return }
    setStorySaving(true)
    setStoryError(null)
    try {
      if (storyModal?.mode === 'create' && storyModal.epicId) await api.createStory(storyModal.epicId, { ...storyForm, metadata: storyMeta.value })
      else if (storyModal?.story) {
        await api.updateStory(storyModal.story.id, { ...withClears(storyForm, ['description', 'acceptance_criteria']), metadata: metadataPatch(storyMeta.initial, storyMeta.value) })
        if (storyEpicId != null && storyEpicId !== storyModal.story.epic_id) await api.moveStory(storyModal.story.id, storyEpicId)
      }
      setStoryModal(null); refresh()
    } catch (e) {
      setStoryError(errMsg(e, 'Save failed'))
    } finally { setStorySaving(false) }
  }

  const handleStoryDelete = async () => {
    if (!storyDeleteTarget) return
    setStorySaving(true)
    setStoryError(null)
    try { await api.deleteStory(storyDeleteTarget.id); setStoryDeleteTarget(null); refresh() }
    catch (e) { setStoryError(errMsg(e, 'Delete failed')) }
    finally { setStorySaving(false) }
  }

  const handleTaskCreate = async () => {
    if (!taskForm.name.trim() || !taskCreateModal) return
    setTaskSaving(true)
    setTaskError(null)
    try {
      const { owner, ...rest } = taskForm
      await api.createTask(pid, {
        epic_id: taskCreateModal.story.epic_id,
        story_id: taskCreateModal.story.id,
        ...rest,
        // '' = me (server default); 'none' = unassigned; else a member's user id
        ...(owner ? { owner } : {}),
      })
      setTaskCreateModal(null); refresh()
    } catch (e) {
      setTaskError(errMsg(e, 'Create failed'))
    } finally { setTaskSaving(false) }
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center h-full" style={{ background: 'var(--bg)' }}>
        <Loader2 size={22} className="text-primary animate-spin" />
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex items-center justify-center h-full text-danger-ink text-sm" style={{ background: 'var(--bg)' }}>
        {error}
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full overflow-hidden" style={{ background: 'var(--bg)' }}>
      {/* Top navigation */}
      <ProjectNav
        projectId={pid}
        currentView="board"
        rightSlot={
          <>
            {/* New Epic */}
            <button
              onClick={() => { setEpicForm({ name: '', description: '', priority: '1' }); openEpicModal({ mode: 'create' }) }}
              className="amp-btn amp-btn-primary"
            >
              <Plus size={12} />
              Epic
            </button>

            {/* Plan check */}
            <button
              onClick={() => setPlanCheckOpen(true)}
              className="amp-btn"
              title="Check the plan's dependencies before dispatching"
            >
              <ListChecks size={12} />
              <span className="hidden sm:inline">Check plan</span>
            </button>

            {/* Team access */}
            <button
              onClick={() => setTeamAccessOpen(true)}
              className="amp-btn"
              title="Teams with access to this project"
            >
              <Users size={12} />
              <span className="hidden sm:inline">Team access</span>
            </button>

            {/* Hide/show completed epics */}
            {completedEpicIds.size > 0 && (
              <button
                onClick={() => setHideCompleted(h => !h)}
                className={`amp-btn ${hideCompleted ? 'amp-btn-primary' : ''}`} aria-pressed={hideCompleted}
              >
                {hideCompleted ? <Eye size={12} /> : <EyeOff size={12} />}
                <span>{hideCompleted ? `${completedEpicIds.size} hidden` : 'Hide done'}</span>
              </button>
            )}

            {/* Live status */}
            <div className={`flex items-center gap-1.5 amp-num text-[11px] ${liveStatus === 'connected' ? 'text-success-ink' : 'text-ink-3'}`}>
              {liveStatus === 'connected'
                ? <span className="w-1.5 h-1.5 bg-success" />
                : <WifiOff size={11} />
              }
              <span className="hidden sm:inline">{liveStatus === 'connected' ? 'Live' : 'Connecting…'}</span>
            </div>

            <button
              onClick={refresh}
              className="p-1.5 text-ink-3 hover:text-ink transition-colors cursor-pointer rounded-lg hover:bg-surface-3"
            >
              <RefreshCw size={13} />
            </button>
          </>
        }
      />

      {/* Filter bar */}
      <div className="px-5 py-3 flex-shrink-0" style={{ borderBottom: '1px solid var(--line-sub)', background: 'var(--bg)' }}>
        <FilterBar
          filter={filter}
          onChange={setFilter}
          epicOptions={epics.map(e => ({ id: e.id, name: e.name }))}
          memberOptions={members.map(m => ({ id: m.user_id, name: personName(m) + (m.user_id === meId ? ' (me)' : '') }))}
          counts={counts}
        />
      </div>

      {/* Board scroll area */}
      <div className="flex-1 overflow-auto">
        {epics.length === 0 ? (
          <EmptyState />
        ) : (
          <div style={{ minWidth: LABEL_COL + COLUMNS.length * TASK_COL }}>
            {/* Column headers — sticky */}
            <div className="sticky top-0 z-10 flex" style={{ background: 'var(--surface)', borderBottom: '1px solid var(--line)' }}>
              {/* Row label spacer */}
              <div className="flex-shrink-0 px-3 py-3 border-r border-line-sub" style={{ width: LABEL_COL }} />
              {COLUMNS.map(col => (
                <div
                  key={col.state}
                  className="flex-1 px-3 py-3 border-r border-line last:border-r-0 flex items-center gap-2"
                  style={{ minWidth: TASK_COL, boxShadow: `inset 0 3px 0 ${col.topBorder}` }}
                >
                  <i className="inline-block w-2 h-2" style={{ background: col.color }} aria-hidden="true" />
                  <span className="font-label text-[11px] font-semibold uppercase tracking-[0.14em] text-ink">{col.label}</span>
                  <span className="amp-num text-[11px] text-ink-3 ml-auto">{counts[col.state as keyof typeof counts]}</span>
                </div>
              ))}
            </div>

            {/* Epics */}
            {epics.map(epic => {
              const allEpicStories = stories.filter(s => s.epic_id === epic.id)
              const epicTasks = ownerTasks.filter(t => t.epic_id === epic.id)
              const isEpicCollapsed = collapsedEpics.has(epic.id)
              const isDone = completedEpicIds.has(epic.id)
              const storyVisible = (story: Story) => !hasFilter
                || ownerTasks.some(t => t.story_id === story.id && filteredTaskIds.has(t.id))
                || (showOwnedEmpty && iOwn(story.owner_ids))
              const epicStories = allEpicStories.filter(storyVisible)
              const epicOwners = ownersOf(epic.owner_ids)

              if (hideCompleted && isDone && !hasFieldFilter) return null
              if (hasFilter && !epicTasks.some(t => filteredTaskIds.has(t.id))) {
                const keepOwned = showOwnedEmpty && (filter.epicId === null || filter.epicId === epic.id)
                  && (iOwn(epic.owner_ids) || epicStories.length > 0)
                if (!keepOwned) return null
              }

              return (
                <div key={epic.id} className="border-b border-line-sub">
                  {/* Epic header */}
                  <div
                    className="flex items-center cursor-pointer group transition-colors bg-surface-2 hover:bg-surface-3"
                    style={{ borderBottom: '1px solid var(--line)' }}
                    onClick={() => toggleEpic(epic.id)}
                  >
                    <div className="flex-shrink-0 px-3 py-3 border-r border-line-sub flex items-center gap-2 relative" style={{ width: LABEL_COL }}>
                      <span className="text-line-str group-hover:text-ink-2 transition-colors">
                        {isEpicCollapsed ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                      </span>
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2"><span className="amp-bands-v h-[12px]" aria-hidden="true" /><span className="font-label text-[12px] font-semibold uppercase tracking-[0.12em] text-ink truncate">{epic.name}</span></div>
                        <div className="flex items-center gap-1.5 text-xs text-ink-3 tabular-nums">
                          <span className="truncate">
                            {epicStories.length} stor{epicStories.length !== 1 ? 'ies' : 'y'} · {epicTasks.length} task{epicTasks.length !== 1 ? 's' : ''}
                          </span>
                          <OwnerStack people={epicOwners} max={2} size={14} />
                        </div>
                      </div>
                      <button
                        onClick={e => {
                          e.stopPropagation()
                          setStoryForm({ name: '', description: '', acceptance_criteria: '', priority: '1' })
                          openStoryModal({ mode: 'create', epicId: epic.id })
                        }}
                        className="p-1 opacity-0 group-hover:opacity-100 hover:bg-surface-3 rounded-md transition-all cursor-pointer"
                        title="Add story"
                      >
                        <Plus size={11} className="text-ink-2" />
                      </button>
                      <button
                        onClick={e => { e.stopPropagation(); setOpenEpicMenu(openEpicMenu === epic.id ? null : epic.id) }}
                        className="p-1 opacity-0 group-hover:opacity-100 hover:bg-surface-3 rounded-md transition-all cursor-pointer"
                      >
                        <MoreHorizontal size={12} className="text-ink-2" />
                      </button>
                       {openEpicMenu === epic.id && (
                         <div className="absolute right-1 top-9 z-20 py-1 min-w-[108px]"
                           style={{ background: 'var(--surface)', border: '1px solid var(--line)', boxShadow: '0 8px 24px color-mix(in srgb, var(--ink) 10%, transparent)' }}>
                          <button
                            onClick={e => {
                              e.stopPropagation()
                              setEpicForm({ name: epic.name, description: epic.description || '', priority: epic.priority || '1' })
                              openEpicModal({ mode: 'edit', epic }); setOpenEpicMenu(null)
                            }}
                            className="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-surface-3 transition-colors cursor-pointer"
                          >Edit</button>
                          <button
                            onClick={e => { e.stopPropagation(); setEpicError(null); setEpicDeleteTarget(epic); setOpenEpicMenu(null) }}
                            className="w-full text-left px-3 py-1.5 text-xs text-danger-ink hover:bg-danger/10 transition-colors cursor-pointer"
                          >Delete</button>
                        </div>
                      )}
                    </div>
                    {COLUMNS.map(col => {
                      let n = 0
                      if (col.state === 'scheduled') n = epicTasks.filter(t => isScheduledTask(t)).length
                      else if (col.state === 'blocked') n = epicTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t)).length
                      else n = epicTasks.filter(t => t.state === col.state).length
                      return (
                        <div key={col.state} className="flex-1 px-3 py-3 border-r border-line-sub last:border-r-0" style={{ minWidth: TASK_COL }}>
                          {n > 0 && (
                            <span className="amp-num text-[11.5px]" style={{ color: col.color }}>
                              {n}
                            </span>
                          )}
                        </div>
                      )
                    })}
                  </div>

                  {/* Stories */}
                  {!isEpicCollapsed && epicStories.map(story => {
                    const storyTasks = ownerTasks.filter(t => t.story_id === story.id)
                    const isStoryCollapsed = collapsedStories.has(story.id)

                    return (
                      <div key={story.id} className="border-b border-line-sub/60 last:border-b-0">
                        {/* Story header */}
                        <div
                          className="flex items-center cursor-pointer group transition-colors bg-surface hover:bg-surface-2 border-b border-line"
                          onClick={() => toggleStory(story.id)}
                        >
                          <div className="flex-shrink-0 px-3 py-2.5 pl-7 border-r border-line-sub flex items-center gap-2 relative" style={{ width: LABEL_COL }}>
                            <span className="text-line group-hover:text-line-str transition-colors">
                              {isStoryCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
                            </span>
                            <div className="min-w-0 flex-1">
                              <div className="text-[12.5px] font-medium text-ink truncate">{story.name}</div>
                              <div className="flex items-center gap-1.5 text-xs text-ink-3 tabular-nums">
                                <span className="truncate">{storyTasks.length} task{storyTasks.length !== 1 ? 's' : ''}</span>
                                <OwnerStack people={ownersOf(story.owner_ids)} max={2} size={14} />
                              </div>
                            </div>
                            <button
                              onClick={e => {
                                e.stopPropagation()
                                setTaskForm({ ...EMPTY_TASK_FORM })
                                setTaskError(null)
                                setTaskCreateModal({ story })
                              }}
                              className="p-1 opacity-0 group-hover:opacity-100 hover:bg-surface-3 rounded-md transition-all cursor-pointer"
                              title="Add task"
                            >
                              <Plus size={11} className="text-ink-2" />
                            </button>
                            <button
                              onClick={e => { e.stopPropagation(); setOpenStoryMenu(openStoryMenu === story.id ? null : story.id) }}
                              className="p-1 opacity-0 group-hover:opacity-100 hover:bg-surface-3 rounded-md transition-all cursor-pointer"
                            >
                              <MoreHorizontal size={11} className="text-ink-2" />
                            </button>
                             {openStoryMenu === story.id && (
                               <div className="absolute right-1 top-8 z-20 py-1 min-w-[108px]"
                                 style={{ background: 'var(--surface)', border: '1px solid var(--line)', boxShadow: '0 8px 24px color-mix(in srgb, var(--ink) 10%, transparent)' }}>
                                <button
                                  onClick={e => {
                                    e.stopPropagation()
                                    setStoryForm({ name: story.name, description: story.description || '', acceptance_criteria: story.acceptance_criteria || '', priority: story.priority || '1' })
                                    openStoryModal({ mode: 'edit', story }); setOpenStoryMenu(null)
                                  }}
                                  className="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-surface-3 transition-colors cursor-pointer"
                                >Edit</button>
                                <button
                                  onClick={e => { e.stopPropagation(); setStoryError(null); setStoryDeleteTarget(story); setOpenStoryMenu(null) }}
                                  className="w-full text-left px-3 py-1.5 text-xs text-danger-ink hover:bg-danger/10 transition-colors cursor-pointer"
                                >Delete</button>
                              </div>
                            )}
                          </div>
                          {COLUMNS.map(col => {
                            let n = 0
                            if (col.state === 'scheduled') n = storyTasks.filter(t => isScheduledTask(t)).length
                            else if (col.state === 'blocked') n = storyTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t)).length
                            else n = storyTasks.filter(t => t.state === col.state).length
                            return (
                              <div key={col.state} className="flex-1 px-3 py-2.5 border-r border-line-sub last:border-r-0" style={{ minWidth: TASK_COL }}>
                                {n > 0 && (
                                  <span className="amp-num text-[11px]" style={{ color: col.color }}>
                                    {n}
                                  </span>
                                )}
                              </div>
                            )
                          })}
                        </div>

                        {/* Task rows */}
                        {!isStoryCollapsed && (
                          <div className="flex">
                            <div className="flex-shrink-0 border-r border-line-sub" style={{ width: LABEL_COL }} />
                            {COLUMNS.map(col => {
                              let colTasks: Task[] = []
                              if (col.state === 'scheduled') colTasks = storyTasks.filter(t => isScheduledTask(t))
                              else if (col.state === 'blocked') colTasks = storyTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t))
                                .sort((a, b) => Number(!!b.block_reason) - Number(!!a.block_reason))
                              else colTasks = storyTasks.filter(t => t.state === col.state)
                              return (
                                 <div
                                   key={col.state}
                                   className="flex-1 p-2 border-r border-line last:border-r-0"
                                   style={{ minWidth: TASK_COL, background: 'var(--bg)' }}
                                >
                                  <div className="space-y-1.5">
                                    {colTasks.map(task => (
                                      <TaskCard
                                        key={task.id}
                                        task={task}
                                        onClick={() => setSelectedTaskId(task.id)}
                                        dimmed={hasFilter && !filteredTaskIds.has(task.id)}
                                        isScheduled={col.state === 'scheduled'}
                                        owner={lookupPerson(people, task.owner_user_id)}
                                      />
                                    ))}
                                    {colTasks.length === 0 && <div className="h-8" />}
                                  </div>
                                </div>
                              )
                            })}
                          </div>
                        )}
                      </div>
                    )
                  })}
                </div>
              )
            })}
          </div>
        )}
      </div>

      {/* Task drawer */}
      <TaskDrawer
        task={selectedTask}
        onClose={() => setSelectedTaskId(null)}
        onRefresh={refresh}
        members={members}
        meId={meId}
        myAccess={myAccess}
        openReassign={openReassign}
        onTaskUpdated={applyTask}
      />

      {planCheckOpen && (
        <PlanCheckModal projectId={pid} tasks={tasks}
          onOpenTask={id => { setPlanCheckOpen(false); setSelectedTaskId(id) }}
          onClose={() => { setPlanCheckOpen(false); refresh() }} />
      )}

      {teamAccessOpen && (
        <TeamAccessModal
          projectId={pid}
          myAccess={myAccess}
          onProjectUpdated={p => setOpenReassign(p.open_reassign ?? false)}
          onClose={() => setTeamAccessOpen(false)}
        />
      )}

      {/* Epic CRUD modals */}
      {epicModal && (
        <CrudModal title={epicModal.mode === 'create' ? 'New Epic' : 'Edit Epic'} onClose={() => setEpicModal(null)} onSave={handleEpicSave} saving={epicSaving}>
          <div className="space-y-4">
            <div><label className={labelCls}>Name *</label>
              <input autoFocus value={epicForm.name} onChange={e => setEpicForm(f => ({ ...f, name: e.target.value }))} className={inputCls} placeholder="Epic name" /></div>
            <div><label className={labelCls}>Description</label>
              <textarea value={epicForm.description} onChange={e => setEpicForm(f => ({ ...f, description: e.target.value }))} rows={3} className={`${inputCls} resize-none`} placeholder="Optional description" /></div>
            <div><label className={labelCls}>Priority</label>
              <select value={epicForm.priority} onChange={e => setEpicForm(f => ({ ...f, priority: e.target.value }))} className={inputCls}>
                <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
              </select></div>
            {editingEpic && (
              <div><label className={labelCls}>Owners</label>
                <OwnersEditor
                  ownerIds={editingEpic.owner_ids ?? []}
                  people={people}
                  members={members}
                  meId={meId}
                  onAdd={async id => applyEpic(await api.addEpicOwner(editingEpic.id, id))}
                  onRemove={async id => applyEpic(await api.removeEpicOwner(editingEpic.id, id))}
                /></div>
            )}
            <div><label className={labelCls}>Metadata</label>
              <MetadataEditor key={epicModal.epic?.id ?? 'new'} initial={epicMeta.initial} inherited={epicMeta.inherited}
                onChange={(value, error) => setEpicMeta(m => ({ ...m, value, error }))} /></div>
            {epicError && <p className="text-xs text-danger-ink">{epicError}</p>}
          </div>
        </CrudModal>
      )}

      {epicDeleteTarget && (
        <ConfirmDeleteModal
          title={`Delete "${epicDeleteTarget.name}"`}
          description="This will permanently delete the epic and all its stories and tasks."
          onClose={() => setEpicDeleteTarget(null)} onConfirm={handleEpicDelete} deleting={epicSaving} error={epicError}
        />
      )}

      {storyModal && (
        <CrudModal title={storyModal.mode === 'create' ? 'New Story' : 'Edit Story'} onClose={() => setStoryModal(null)} onSave={handleStorySave} saving={storySaving}>
          <div className="space-y-4">
            <div><label className={labelCls}>Name *</label>
              <input autoFocus value={storyForm.name} onChange={e => setStoryForm(f => ({ ...f, name: e.target.value }))} className={inputCls} placeholder="Story name" /></div>
            <div><label className={labelCls}>Description</label>
              <textarea value={storyForm.description} onChange={e => setStoryForm(f => ({ ...f, description: e.target.value }))} rows={2} className={`${inputCls} resize-none`} /></div>
            <div><label className={labelCls}>Acceptance Criteria</label>
              <textarea value={storyForm.acceptance_criteria} onChange={e => setStoryForm(f => ({ ...f, acceptance_criteria: e.target.value }))} rows={3} className={`${inputCls} resize-none`} /></div>
            {storyModal.mode === 'edit' && storyEpicId != null && (
              <div><label className={labelCls}>Epic</label>
                <select value={storyEpicId} onChange={e => setStoryEpicId(Number(e.target.value))} className={inputCls}>
                  {epics.map(ep => <option key={ep.id} value={ep.id}>{ep.name}</option>)}
                </select>
                {storyEpicId !== storyModal.story?.epic_id && (
                  <p className="text-[11px] text-ink-2 mt-1">Saving moves the story and its tickets to this epic; dependencies are kept.</p>
                )}</div>
            )}
            <div><label className={labelCls}>Priority</label>
              <select value={storyForm.priority} onChange={e => setStoryForm(f => ({ ...f, priority: e.target.value }))} className={inputCls}>
                <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
              </select></div>
            {editingStory && (
              <div><label className={labelCls}>Owners</label>
                <OwnersEditor
                  ownerIds={editingStory.owner_ids ?? []}
                  people={people}
                  members={members}
                  meId={meId}
                  onAdd={async id => applyStory(await api.addStoryOwner(editingStory.id, id))}
                  onRemove={async id => applyStory(await api.removeStoryOwner(editingStory.id, id))}
                /></div>
            )}
            <div><label className={labelCls}>Metadata</label>
              <MetadataEditor key={storyModal.story?.id ?? `new-${storyModal.epicId}`} initial={storyMeta.initial} inherited={storyMeta.inherited}
                onChange={(value, error) => setStoryMeta(m => ({ ...m, value, error }))} /></div>
            {storyError && <p className="text-xs text-danger-ink">{storyError}</p>}
          </div>
        </CrudModal>
      )}

      {storyDeleteTarget && (
        <ConfirmDeleteModal
          title={`Delete "${storyDeleteTarget.name}"`}
          description="This will permanently delete the story and all its tasks."
          onClose={() => setStoryDeleteTarget(null)} onConfirm={handleStoryDelete} deleting={storySaving} error={storyError}
        />
      )}

      {taskCreateModal && (
        <CrudModal title="New Task" onClose={() => setTaskCreateModal(null)} onSave={handleTaskCreate} saving={taskSaving}>
          <div className="space-y-4">
            <div><label className={labelCls}>Name *</label>
              <input autoFocus value={taskForm.name} onChange={e => setTaskForm(f => ({ ...f, name: e.target.value }))} className={inputCls} placeholder="Task name" /></div>
            <div><label className={labelCls}>Description</label>
              <textarea value={taskForm.description} onChange={e => setTaskForm(f => ({ ...f, description: e.target.value }))} rows={3} className={`${inputCls} resize-none`} /></div>
            <div><label className={labelCls}>Acceptance Criteria</label>
              <textarea value={taskForm.acceptance_criteria} onChange={e => setTaskForm(f => ({ ...f, acceptance_criteria: e.target.value }))} rows={3} className={`${inputCls} resize-none`} /></div>
            <div><label className={labelCls}>Owner</label>
              <select value={taskForm.owner} onChange={e => setTaskForm(f => ({ ...f, owner: e.target.value }))} className={inputCls}>
                <option value="">Me</option>
                <option value="none">Unassigned</option>
                {members.filter(m => m.user_id !== meId).map(m => (
                  <option key={m.user_id} value={String(m.user_id)}>{personName(m)}</option>
                ))}
              </select></div>
            <div><label className={labelCls}>Agent</label>
              <input value={taskForm.assigned_to} onChange={e => setTaskForm(f => ({ ...f, assigned_to: e.target.value }))} className={inputCls} placeholder="amp-worker" /></div>
            <div><label className={labelCls}>Priority</label>
              <select value={taskForm.priority} onChange={e => setTaskForm(f => ({ ...f, priority: e.target.value }))} className={inputCls}>
                <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
              </select></div>
            <div><label className={labelCls}>Depends on</label>
              <DependencyPicker tasks={tasks} value={taskForm.dependency_ids}
                onChange={ids => setTaskForm(f => ({ ...f, dependency_ids: ids }))} />
              {taskForm.dependency_ids.length === 0 && tasks.length > 0 && (
                <p className="text-[11px] text-warning-ink mt-1.5 leading-relaxed">
                  With no dependencies this ticket is ready to start right away, in parallel with everything else
                  that's ready. If it needs another ticket's output, add it here.
                </p>
              )}</div>
            {taskError && <p className="text-xs text-danger-ink">{taskError}</p>}
          </div>
        </CrudModal>
      )}
    </div>
  )
}

function EmptyState() {
  return (
    <div className="flex flex-col items-center justify-center h-64 gap-4">
      <div
        className="w-14 h-14 rounded-2xl flex items-center justify-center"
        style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}
      >
        <GitBranch size={22} className="text-line-str" />
      </div>
      <div className="text-center">
        <p className="text-sm font-semibold text-ink">No epics yet</p>
        <p className="text-xs text-ink-2 mt-1.5 max-w-xs leading-relaxed">
          Ask the amp-manager to plan your project — tasks will appear here in real time.
        </p>
      </div>
    </div>
  )
}
