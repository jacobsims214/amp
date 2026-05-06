import { useState, useMemo, useEffect } from 'react'
import { useParams } from 'react-router-dom'
import { RefreshCw, Loader2, Wifi, WifiOff, ChevronDown, ChevronRight, EyeOff, Eye, GitBranch, MoreHorizontal, Plus } from 'lucide-react'
import { useBoardData } from '../hooks/useBoardData'
import { useSSE } from '../hooks/useSSE'
import { TaskCard } from '../components/TaskCard'
import { TaskDrawer } from '../components/TaskDrawer'
import { FilterBar, type FilterState } from '../components/FilterBar'
import { CrudModal } from '../components/CrudModal'
import { ConfirmDeleteModal } from '../components/ConfirmDeleteModal'
import { ProjectNav } from '../components/ProjectNav'
import { api } from '../api/client'
import type { Task, TaskState, Epic, Story } from '../types'

// Label column width and task column min-width are tuned for responsiveness.
// Label col: 180px | task col: 168px → total minimum = 180 + 5×168 = 1020px (fits 13" screens)
const LABEL_COL = 180
const TASK_COL  = 168

const COLUMNS: {
  state: TaskState
  label: string
  color: string
  countBg: string
  topBorder: string
}[] = [
  { state: 'scheduled',   label: 'Scheduled',   color: '#FBBF24', countBg: 'rgba(245,158,11,0.12)',  topBorder: '#F59E0B' },
  { state: 'backlog',     label: 'Backlog',      color: '#7E91A8', countBg: 'rgba(126,145,168,0.12)', topBorder: '#283A57' },
  { state: 'in_progress', label: 'In Progress',  color: '#818CF8', countBg: 'rgba(99,102,241,0.15)',  topBorder: '#6366F1' },
  { state: 'blocked',     label: 'Blocked',      color: '#F87171', countBg: 'rgba(239,68,68,0.12)',   topBorder: '#EF4444' },
  { state: 'completed',   label: 'Done',         color: '#10B981', countBg: 'rgba(16,185,129,0.12)',  topBorder: '#10B981' },
]

function isScheduledTask(task: Task): boolean {
  return task.state === 'blocked' && task.block_reason?.startsWith('scheduled:') === true
}

const inputCls =
  'w-full bg-[#08101F] border border-[#1E2C45] rounded-lg px-3 py-2 text-sm text-[#DDE6F0] placeholder-[#3D5068] ' +
  'focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all font-[inherit]'

const labelCls = 'text-xs font-medium text-[#7E91A8] block mb-1.5'

export function Board() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)

  const { epics, stories, tasks, loading, error, refresh } = useBoardData(pid)
  const [selectedTask, setSelectedTask] = useState<Task | null>(null)
  const [filter, setFilter] = useState<FilterState>({ query: '', state: '', epicId: null })
  const [collapsedEpics, setCollapsedEpics] = useState<Set<number>>(new Set())
  const [collapsedStories, setCollapsedStories] = useState<Set<number>>(new Set())
  const [hideCompleted, setHideCompleted] = useState(true)
  const [liveStatus, setLiveStatus] = useState<'connected' | 'disconnected'>('disconnected')

  // Epic CRUD
  const [epicModal, setEpicModal] = useState<{ mode: 'create' | 'edit'; epic?: Epic } | null>(null)
  const [epicDeleteTarget, setEpicDeleteTarget] = useState<Epic | null>(null)
  const [epicForm, setEpicForm] = useState({ name: '', description: '', priority: '1' })
  const [epicSaving, setEpicSaving] = useState(false)
  const [openEpicMenu, setOpenEpicMenu] = useState<number | null>(null)

  // Story CRUD
  const [storyModal, setStoryModal] = useState<{ mode: 'create' | 'edit'; story?: Story; epicId?: number } | null>(null)
  const [storyDeleteTarget, setStoryDeleteTarget] = useState<Story | null>(null)
  const [storyForm, setStoryForm] = useState({ name: '', description: '', acceptance_criteria: '', priority: '1' })
  const [storySaving, setStorySaving] = useState(false)
  const [openStoryMenu, setOpenStoryMenu] = useState<number | null>(null)

  // Task create
  const [taskCreateModal, setTaskCreateModal] = useState<{ story: Story } | null>(null)
  const [taskForm, setTaskForm] = useState({ name: '', description: '', acceptance_criteria: '', assigned_to: 'amp-worker', priority: '1' })
  const [taskSaving, setTaskSaving] = useState(false)

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

  const q = filter.query.toLowerCase()
  const filteredTasks = useMemo(() => {
    return tasks.filter(t => {
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
  }, [tasks, filter, q, stories, epics])

  const filteredTaskIds = useMemo(() => new Set(filteredTasks.map(t => t.id)), [filteredTasks])
  const hasFilter = !!(filter.query || filter.state || filter.epicId !== null)

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

  const handleEpicSave = async () => {
    if (!epicForm.name.trim()) return
    setEpicSaving(true)
    try {
      if (epicModal?.mode === 'create') await api.createEpic(pid, epicForm)
      else if (epicModal?.epic) await api.updateEpic(epicModal.epic.id, epicForm)
      setEpicModal(null); refresh()
    } finally { setEpicSaving(false) }
  }

  const handleEpicDelete = async () => {
    if (!epicDeleteTarget) return
    setEpicSaving(true)
    try { await api.deleteEpic(epicDeleteTarget.id); setEpicDeleteTarget(null); refresh() }
    finally { setEpicSaving(false) }
  }

  const handleStorySave = async () => {
    if (!storyForm.name.trim()) return
    setStorySaving(true)
    try {
      if (storyModal?.mode === 'create' && storyModal.epicId) await api.createStory(storyModal.epicId, storyForm)
      else if (storyModal?.story) await api.updateStory(storyModal.story.id, storyForm)
      setStoryModal(null); refresh()
    } finally { setStorySaving(false) }
  }

  const handleStoryDelete = async () => {
    if (!storyDeleteTarget) return
    setStorySaving(true)
    try { await api.deleteStory(storyDeleteTarget.id); setStoryDeleteTarget(null); refresh() }
    finally { setStorySaving(false) }
  }

  const handleTaskCreate = async () => {
    if (!taskForm.name.trim() || !taskCreateModal) return
    setTaskSaving(true)
    try {
      await api.createTask(pid, { epic_id: taskCreateModal.story.epic_id, story_id: taskCreateModal.story.id, ...taskForm })
      setTaskCreateModal(null); refresh()
    } finally { setTaskSaving(false) }
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center h-screen" style={{ background: '#08101F' }}>
        <Loader2 size={22} className="text-[#6366F1] animate-spin" />
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex items-center justify-center h-screen text-[#F87171] text-sm" style={{ background: '#08101F' }}>
        {error}
      </div>
    )
  }

  return (
    <div className="flex flex-col h-screen overflow-hidden" style={{ background: '#08101F' }}>
      {/* Top navigation */}
      <ProjectNav
        projectId={pid}
        currentView="board"
        rightSlot={
          <>
            {/* New Epic */}
            <button
              onClick={() => { setEpicForm({ name: '', description: '', priority: '1' }); setEpicModal({ mode: 'create' }) }}
              className="flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg text-xs font-semibold text-[#818CF8] transition-all cursor-pointer active:scale-[0.97]"
              style={{ background: 'rgba(99,102,241,0.1)', border: '1px solid rgba(99,102,241,0.2)' }}
            >
              <Plus size={12} />
              Epic
            </button>

            {/* Hide/show completed epics */}
            {completedEpicIds.size > 0 && (
              <button
                onClick={() => setHideCompleted(h => !h)}
                className="flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg text-xs font-semibold border transition-all cursor-pointer"
                style={hideCompleted
                  ? { background: 'rgba(16,185,129,0.1)', border: '1px solid rgba(16,185,129,0.25)', color: '#10B981' }
                  : { background: 'transparent', border: '1px solid #1E2C45', color: '#3D5068' }
                }
              >
                {hideCompleted ? <Eye size={12} /> : <EyeOff size={12} />}
                <span>{hideCompleted ? `${completedEpicIds.size} hidden` : 'Hide done'}</span>
              </button>
            )}

            {/* Live status */}
            <div className={`flex items-center gap-1.5 text-xs font-medium ${liveStatus === 'connected' ? 'text-[#10B981]' : 'text-[#3D5068]'}`}>
              {liveStatus === 'connected'
                ? <><span className="w-1.5 h-1.5 rounded-full bg-[#10B981]" style={{ boxShadow: '0 0 5px #10B981' }} /><Wifi size={11} /></>
                : <WifiOff size={11} />
              }
              <span className="hidden sm:inline">{liveStatus === 'connected' ? 'Live' : 'Connecting…'}</span>
            </div>

            <button
              onClick={refresh}
              className="p-1.5 text-[#3D5068] hover:text-[#DDE6F0] transition-colors cursor-pointer rounded-lg hover:bg-[#172540]"
            >
              <RefreshCw size={13} />
            </button>
          </>
        }
      />

      {/* Filter bar */}
      <div className="px-5 py-3 flex-shrink-0" style={{ borderBottom: '1px solid #192238', background: '#08101F' }}>
        <FilterBar filter={filter} onChange={setFilter} epicOptions={epics.map(e => ({ id: e.id, name: e.name }))} counts={counts} />
      </div>

      {/* Board scroll area */}
      <div className="flex-1 overflow-auto">
        {epics.length === 0 ? (
          <EmptyState />
        ) : (
          <div className="min-w-max">
            {/* Column headers — sticky */}
            <div className="sticky top-0 z-10 flex" style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45' }}>
              {/* Row label spacer */}
              <div className="flex-shrink-0 px-3 py-3 border-r border-[#192238]" style={{ width: LABEL_COL }} />
              {COLUMNS.map(col => (
                <div
                  key={col.state}
                  className="flex-1 px-3 py-3 border-r border-[#192238] last:border-r-0 flex items-center gap-2"
                  style={{ minWidth: TASK_COL, borderTop: `2px solid ${col.topBorder}22` }}
                >
                  <span className="text-xs font-bold uppercase tracking-widest" style={{ color: col.color }}>
                    {col.label}
                  </span>
                  <span
                    className="text-xs px-1.5 py-0.5 rounded-full font-semibold tabular-nums"
                    style={{ background: col.countBg, color: col.color }}
                  >
                    {counts[col.state as keyof typeof counts]}
                  </span>
                </div>
              ))}
            </div>

            {/* Epics */}
            {epics.map(epic => {
              const epicStories = stories.filter(s => s.epic_id === epic.id)
              const epicTasks = tasks.filter(t => t.epic_id === epic.id)
              const isEpicCollapsed = collapsedEpics.has(epic.id)
              const isDone = completedEpicIds.has(epic.id)

              if (hideCompleted && isDone && !hasFilter) return null
              if (hasFilter && epicTasks.filter(t => filteredTaskIds.has(t.id)).length === 0) return null

              return (
                <div key={epic.id} className="border-b border-[#192238]">
                  {/* Epic header */}
                  <div
                    className="flex items-center cursor-pointer group transition-colors"
                    style={{ borderBottom: '1px solid #192238' }}
                    onClick={() => toggleEpic(epic.id)}
                    onMouseEnter={e => (e.currentTarget as HTMLElement).style.background = '#0D1726'}
                    onMouseLeave={e => (e.currentTarget as HTMLElement).style.background = 'transparent'}
                  >
                    <div className="flex-shrink-0 px-3 py-3 border-r border-[#192238] flex items-center gap-2 relative" style={{ width: LABEL_COL }}>
                      <span className="text-[#283A57] group-hover:text-[#7E91A8] transition-colors">
                        {isEpicCollapsed ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                      </span>
                      <div className="min-w-0 flex-1">
                        <div className="text-xs font-bold text-[#DDE6F0] truncate">{epic.name}</div>
                        <div className="text-xs text-[#3D5068] tabular-nums">
                          {epicStories.length} stor{epicStories.length !== 1 ? 'ies' : 'y'} · {epicTasks.length} task{epicTasks.length !== 1 ? 's' : ''}
                        </div>
                      </div>
                      <button
                        onClick={e => {
                          e.stopPropagation()
                          setStoryForm({ name: '', description: '', acceptance_criteria: '', priority: '1' })
                          setStoryModal({ mode: 'create', epicId: epic.id })
                        }}
                        className="p-1 opacity-0 group-hover:opacity-100 hover:bg-[#172540] rounded-md transition-all cursor-pointer"
                        title="Add story"
                      >
                        <Plus size={11} className="text-[#7E91A8]" />
                      </button>
                      <button
                        onClick={e => { e.stopPropagation(); setOpenEpicMenu(openEpicMenu === epic.id ? null : epic.id) }}
                        className="p-1 opacity-0 group-hover:opacity-100 hover:bg-[#172540] rounded-md transition-all cursor-pointer"
                      >
                        <MoreHorizontal size={12} className="text-[#7E91A8]" />
                      </button>
                       {openEpicMenu === epic.id && (
                         <div className="absolute right-1 top-9 z-20 rounded-xl py-1 min-w-[108px]"
                           style={{ background: '#0D1726', border: '1px solid #1E2C45', boxShadow: '0 8px 32px rgba(0,0,0,0.5)' }}>
                          <button
                            onClick={e => {
                              e.stopPropagation()
                              setEpicForm({ name: epic.name, description: epic.description || '', priority: epic.priority || '1' })
                              setEpicModal({ mode: 'edit', epic }); setOpenEpicMenu(null)
                            }}
                            className="w-full text-left px-3 py-1.5 text-xs text-[#DDE6F0] hover:bg-[#172540] transition-colors cursor-pointer"
                          >Edit</button>
                          <button
                            onClick={e => { e.stopPropagation(); setEpicDeleteTarget(epic); setOpenEpicMenu(null) }}
                            className="w-full text-left px-3 py-1.5 text-xs text-[#F87171] hover:bg-[#EF4444]/10 transition-colors cursor-pointer"
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
                        <div key={col.state} className="flex-1 px-3 py-3 border-r border-[#192238] last:border-r-0" style={{ minWidth: TASK_COL }}>
                          {n > 0 && (
                            <span className="text-xs px-1.5 py-0.5 rounded-full font-semibold tabular-nums"
                              style={{ background: col.countBg, color: col.color }}>
                              {n}
                            </span>
                          )}
                        </div>
                      )
                    })}
                  </div>

                  {/* Stories */}
                  {!isEpicCollapsed && epicStories.map(story => {
                    const storyTasks = tasks.filter(t => t.story_id === story.id)
                    const isStoryCollapsed = collapsedStories.has(story.id)
                    if (hasFilter && storyTasks.filter(t => filteredTaskIds.has(t.id)).length === 0) return null

                    return (
                      <div key={story.id} className="border-b border-[#192238]/60 last:border-b-0">
                        {/* Story header */}
                        <div
                          className="flex items-center cursor-pointer group transition-colors"
                          onClick={() => toggleStory(story.id)}
                          onMouseEnter={e => (e.currentTarget as HTMLElement).style.background = '#08101F'}
                          onMouseLeave={e => (e.currentTarget as HTMLElement).style.background = 'transparent'}
                        >
                          <div className="flex-shrink-0 px-3 py-2.5 pl-7 border-r border-[#192238] flex items-center gap-2 relative" style={{ width: LABEL_COL }}>
                            <span className="text-[#1E2C45] group-hover:text-[#283A57] transition-colors">
                              {isStoryCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
                            </span>
                            <div className="min-w-0 flex-1">
                              <div className="text-xs font-medium text-[#7E91A8] truncate">{story.name}</div>
                              <div className="text-xs text-[#3D5068] tabular-nums">{storyTasks.length} task{storyTasks.length !== 1 ? 's' : ''}</div>
                            </div>
                            <button
                              onClick={e => {
                                e.stopPropagation()
                                setTaskForm({ name: '', description: '', acceptance_criteria: '', assigned_to: 'amp-worker', priority: '1' })
                                setTaskCreateModal({ story })
                              }}
                              className="p-1 opacity-0 group-hover:opacity-100 hover:bg-[#172540] rounded-md transition-all cursor-pointer"
                              title="Add task"
                            >
                              <Plus size={11} className="text-[#7E91A8]" />
                            </button>
                            <button
                              onClick={e => { e.stopPropagation(); setOpenStoryMenu(openStoryMenu === story.id ? null : story.id) }}
                              className="p-1 opacity-0 group-hover:opacity-100 hover:bg-[#172540] rounded-md transition-all cursor-pointer"
                            >
                              <MoreHorizontal size={11} className="text-[#7E91A8]" />
                            </button>
                             {openStoryMenu === story.id && (
                               <div className="absolute right-1 top-8 z-20 rounded-xl py-1 min-w-[108px]"
                                 style={{ background: '#0D1726', border: '1px solid #1E2C45', boxShadow: '0 8px 32px rgba(0,0,0,0.5)' }}>
                                <button
                                  onClick={e => {
                                    e.stopPropagation()
                                    setStoryForm({ name: story.name, description: story.description || '', acceptance_criteria: story.acceptance_criteria || '', priority: story.priority || '1' })
                                    setStoryModal({ mode: 'edit', story }); setOpenStoryMenu(null)
                                  }}
                                  className="w-full text-left px-3 py-1.5 text-xs text-[#DDE6F0] hover:bg-[#172540] transition-colors cursor-pointer"
                                >Edit</button>
                                <button
                                  onClick={e => { e.stopPropagation(); setStoryDeleteTarget(story); setOpenStoryMenu(null) }}
                                  className="w-full text-left px-3 py-1.5 text-xs text-[#F87171] hover:bg-[#EF4444]/10 transition-colors cursor-pointer"
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
                              <div key={col.state} className="flex-1 px-3 py-2.5 border-r border-[#192238] last:border-r-0" style={{ minWidth: TASK_COL }}>
                                {n > 0 && (
                                  <span className="text-xs px-1.5 py-0.5 rounded-full tabular-nums"
                                    style={{ background: col.countBg, color: col.color }}>
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
                            <div className="flex-shrink-0 border-r border-[#192238]" style={{ width: LABEL_COL }} />
                            {COLUMNS.map(col => {
                              let colTasks: Task[] = []
                              if (col.state === 'scheduled') colTasks = storyTasks.filter(t => isScheduledTask(t))
                              else if (col.state === 'blocked') colTasks = storyTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t))
                              else colTasks = storyTasks.filter(t => t.state === col.state)
                              return (
                                 <div
                                   key={col.state}
                                   className="flex-1 p-1.5 border-r border-[#192238] last:border-r-0"
                                   style={{ minWidth: TASK_COL, background: 'rgba(8,16,31,0.4)' }}
                                >
                                  <div className="space-y-2">
                                    {colTasks.map(task => (
                                      <TaskCard
                                        key={task.id}
                                        task={task}
                                        onClick={() => setSelectedTask(task)}
                                        dimmed={hasFilter && !filteredTaskIds.has(task.id)}
                                        isScheduled={col.state === 'scheduled'}
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
      <TaskDrawer task={selectedTask} onClose={() => setSelectedTask(null)} onRefresh={refresh} />

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
          </div>
        </CrudModal>
      )}

      {epicDeleteTarget && (
        <ConfirmDeleteModal
          title={`Delete "${epicDeleteTarget.name}"`}
          description="This will permanently delete the epic and all its stories and tasks."
          onClose={() => setEpicDeleteTarget(null)} onConfirm={handleEpicDelete} deleting={epicSaving}
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
            <div><label className={labelCls}>Priority</label>
              <select value={storyForm.priority} onChange={e => setStoryForm(f => ({ ...f, priority: e.target.value }))} className={inputCls}>
                <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
              </select></div>
          </div>
        </CrudModal>
      )}

      {storyDeleteTarget && (
        <ConfirmDeleteModal
          title={`Delete "${storyDeleteTarget.name}"`}
          description="This will permanently delete the story and all its tasks."
          onClose={() => setStoryDeleteTarget(null)} onConfirm={handleStoryDelete} deleting={storySaving}
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
            <div><label className={labelCls}>Assigned To</label>
              <input value={taskForm.assigned_to} onChange={e => setTaskForm(f => ({ ...f, assigned_to: e.target.value }))} className={inputCls} placeholder="amp-worker" /></div>
            <div><label className={labelCls}>Priority</label>
              <select value={taskForm.priority} onChange={e => setTaskForm(f => ({ ...f, priority: e.target.value }))} className={inputCls}>
                <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
              </select></div>
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
        style={{ background: '#0D1726', border: '1px solid #1E2C45' }}
      >
        <GitBranch size={22} className="text-[#283A57]" />
      </div>
      <div className="text-center">
        <p className="text-sm font-semibold text-[#DDE6F0]">No epics yet</p>
        <p className="text-xs text-[#7E91A8] mt-1.5 max-w-xs leading-relaxed">
          Ask the amp-manager to plan your project — tasks will appear here in real time.
        </p>
      </div>
    </div>
  )
}
