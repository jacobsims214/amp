import { useState, useMemo, useEffect } from 'react'
import { useParams, Link } from 'react-router-dom'
import { ArrowLeft, RefreshCw, Loader2, Wifi, WifiOff, ChevronDown, ChevronRight, EyeOff, Eye, GitBranch, BarChart2, BookOpen, MoreHorizontal, Plus } from 'lucide-react'
import { useBoardData } from '../hooks/useBoardData'
import { useSSE } from '../hooks/useSSE'
import { TaskCard } from '../components/TaskCard'
import { TaskDrawer } from '../components/TaskDrawer'
import { FilterBar, type FilterState } from '../components/FilterBar'
import { CrudModal } from '../components/CrudModal'
import { ConfirmDeleteModal } from '../components/ConfirmDeleteModal'
import { api } from '../api/client'
import type { Task, TaskState, Epic, Story } from '../types'

const COLUMNS: { state: TaskState; label: string; headerColor: string; countColor: string }[] = [
  { state: 'scheduled',   label: 'Scheduled',    headerColor: 'text-[#d29922]',  countColor: 'bg-[#d29922]/15 text-[#d29922]' },
  { state: 'backlog',     label: 'Backlog',      headerColor: 'text-[#8b949e]',  countColor: 'bg-[#8b949e]/15 text-[#8b949e]' },
  { state: 'in_progress', label: 'In Progress',  headerColor: 'text-[#58a6ff]',  countColor: 'bg-[#58a6ff]/15 text-[#58a6ff]' },
  { state: 'blocked',     label: 'Blocked',      headerColor: 'text-[#f85149]',  countColor: 'bg-[#f85149]/15 text-[#f85149]' },
  { state: 'completed',   label: 'Done',          headerColor: 'text-[#3fb950]',  countColor: 'bg-[#3fb950]/15 text-[#3fb950]' },
]

// Helper function to detect if a task is scheduled
function isScheduledTask(task: Task): boolean {
  return task.state === 'blocked' && task.block_reason?.startsWith('scheduled:') === true
}

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

  // Epic CRUD state
   const [epicModal, setEpicModal] = useState<{ mode: 'create' | 'edit'; epic?: Epic } | null>(null)
   const [epicDeleteTarget, setEpicDeleteTarget] = useState<Epic | null>(null)
   const [epicForm, setEpicForm] = useState({ name: '', description: '', priority: '1' })
   const [epicSaving, setEpicSaving] = useState(false)
   const [openEpicMenu, setOpenEpicMenu] = useState<number | null>(null)

   // Story CRUD state
   const [storyModal, setStoryModal] = useState<{ mode: 'create' | 'edit'; story?: Story; epicId?: number } | null>(null)
   const [storyDeleteTarget, setStoryDeleteTarget] = useState<Story | null>(null)
   const [storyForm, setStoryForm] = useState({ name: '', description: '', acceptance_criteria: '', priority: '1' })
   const [storySaving, setStorySaving] = useState(false)
   const [openStoryMenu, setOpenStoryMenu] = useState<number | null>(null)

   // Task create state
   const [taskCreateModal, setTaskCreateModal] = useState<{ story: Story } | null>(null)
   const [taskForm, setTaskForm] = useState({
     name: '',
     description: '',
     acceptance_criteria: '',
     assigned_to: 'amp-worker',
     priority: '1',
   })
   const [taskSaving, setTaskSaving] = useState(false)

  // An epic is "done" when it has at least one task and every task is completed.
  const completedEpicIds = useMemo(() => {
    return new Set(
      epics
        .filter(epic => {
          const epicTasks = tasks.filter(t => t.epic_id === epic.id)
          return epicTasks.length > 0 && epicTasks.every(t => t.state === 'completed')
        })
        .map(e => e.id)
    )
  }, [epics, tasks])

  // SSE live indicator
  useSSE(pid, (event) => {
    if (event.type === 'connected') setLiveStatus('connected')
  })

  // Close menu on outside click
   useEffect(() => {
     if (openEpicMenu === null) return
     const handler = () => setOpenEpicMenu(null)
     document.addEventListener('click', handler)
     return () => document.removeEventListener('click', handler)
   }, [openEpicMenu])

   // Close story menu on outside click
   useEffect(() => {
     if (openStoryMenu === null) return
     const handler = () => setOpenStoryMenu(null)
     document.addEventListener('click', handler)
     return () => document.removeEventListener('click', handler)
   }, [openStoryMenu])

  // Filter logic
  const q = filter.query.toLowerCase()
  const filteredTasks = useMemo(() => {
    return tasks.filter(t => {
      // Map scheduled tasks to 'scheduled' state for filtering purposes
      const displayState = isScheduledTask(t) ? 'scheduled' : t.state
      if (filter.state && displayState !== filter.state) return false
      if (filter.epicId !== null && t.epic_id !== filter.epicId) return false
      if (q) {
        const story = stories.find(s => s.id === t.story_id)
        const epic  = epics.find(e => e.id === t.epic_id)
        const haystack = `${t.name} ${t.description} #${t.id} ${story?.name ?? ''} ${epic?.name ?? ''}`.toLowerCase()
        if (!haystack.includes(q)) return false
      }
      return true
    })
  }, [tasks, filter, q, stories, epics])

  const filteredTaskIds = useMemo(() => new Set(filteredTasks.map(t => t.id)), [filteredTasks])
  const hasFilter = !!(filter.query || filter.state || filter.epicId !== null)

  // Counts for filter bar
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
      if (epicModal?.mode === 'create') {
        await api.createEpic(pid, epicForm)
      } else if (epicModal?.epic) {
        await api.updateEpic(epicModal.epic.id, epicForm)
      }
      setEpicModal(null)
      refresh()
    } finally {
      setEpicSaving(false)
    }
  }

  const handleEpicDelete = async () => {
     if (!epicDeleteTarget) return
     setEpicSaving(true)
     try {
       await api.deleteEpic(epicDeleteTarget.id)
       setEpicDeleteTarget(null)
       refresh()
     } finally {
       setEpicSaving(false)
     }
   }

   const handleStorySave = async () => {
     if (!storyForm.name.trim()) return
     setStorySaving(true)
     try {
       if (storyModal?.mode === 'create' && storyModal.epicId) {
         await api.createStory(storyModal.epicId, storyForm)
       } else if (storyModal?.story) {
         await api.updateStory(storyModal.story.id, storyForm)
       }
       setStoryModal(null)
       refresh()
     } finally {
       setStorySaving(false)
     }
   }

   const handleStoryDelete = async () => {
     if (!storyDeleteTarget) return
     setStorySaving(true)
     try {
       await api.deleteStory(storyDeleteTarget.id)
       setStoryDeleteTarget(null)
       refresh()
     } finally {
       setStorySaving(false)
     }
   }

   const handleTaskCreate = async () => {
     if (!taskForm.name.trim() || !taskCreateModal) return
     setTaskSaving(true)
     try {
       await api.createTask(pid, {
         epic_id: taskCreateModal.story.epic_id,
         story_id: taskCreateModal.story.id,
         ...taskForm,
       })
       setTaskCreateModal(null)
       refresh()
     } finally {
       setTaskSaving(false)
     }
   }

  if (loading) {
    return (
      <div className="flex items-center justify-center h-screen">
        <Loader2 size={24} className="text-[#58a6ff] animate-spin" />
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex items-center justify-center h-screen text-[#f85149] text-sm">
        {error}
      </div>
    )
  }

  return (
    <div className="flex flex-col h-screen bg-[#0d1117] overflow-hidden">
      {/* Top bar */}
      <header className="flex items-center gap-3 px-5 py-3 border-b border-[#21262d] bg-[#161b22] flex-shrink-0">
        <Link to="/" className="flex items-center gap-1.5 text-[#8b949e] hover:text-[#e6edf3] transition-colors">
          <ArrowLeft size={14} />
        </Link>
        <div className="w-px h-4 bg-[#30363d]" />
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold text-[#e6edf3]">Board</span>
          <span className="text-xs text-[#484f58]">·</span>
          <span className="text-xs text-[#8b949e]">{epics.length} epic{epics.length !== 1 ? 's' : ''}</span>
          <span className="text-xs text-[#484f58]">·</span>
          <span className="text-xs text-[#8b949e]">{tasks.length} task{tasks.length !== 1 ? 's' : ''}</span>
        </div>
        <div className="ml-auto flex items-center gap-3">
          <Link
            to={`/project/${pid}/kb`}
            className="flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs text-[#8b949e] hover:text-[#e6edf3] border border-[#30363d] hover:border-[#484f58] transition-colors"
          >
            <BookOpen size={12} />
            KB
          </Link>
          <Link
            to={`/project/${pid}/dag`}
            className="flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs text-[#8b949e] hover:text-[#e6edf3] border border-[#30363d] hover:border-[#484f58] transition-colors"
          >
            <GitBranch size={12} />
            DAG
          </Link>
          <Link
            to={`/project/${pid}/report`}
            className="flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs text-[#8b949e] hover:text-[#e6edf3] border border-[#30363d] hover:border-[#484f58] transition-colors"
          >
            <BarChart2 size={12} />
            Report
          </Link>
           <button
            onClick={() => {
              setEpicForm({ name: '', description: '', priority: '1' })
              setEpicModal({ mode: 'create' })
            }}
            className="flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs text-[#8b949e] hover:text-[#e6edf3] border border-[#30363d] hover:border-[#484f58] transition-colors"
          >
            <Plus size={12} />
            Epic
          </button>
          {completedEpicIds.size > 0 && (
            <button
              onClick={() => setHideCompleted(h => !h)}
              className={`flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-medium border transition-colors ${
                hideCompleted
                  ? 'bg-[#3fb950]/10 border-[#3fb950]/30 text-[#3fb950]'
                  : 'bg-transparent border-[#30363d] text-[#8b949e] hover:text-[#e6edf3] hover:border-[#484f58]'
              }`}
              title={hideCompleted ? 'Show completed epics' : 'Hide completed epics'}
            >
              {hideCompleted ? <Eye size={12} /> : <EyeOff size={12} />}
              <span>{hideCompleted ? `${completedEpicIds.size} hidden` : `Hide done`}</span>
            </button>
          )}
          <div className={`flex items-center gap-1.5 text-xs ${liveStatus === 'connected' ? 'text-[#3fb950]' : 'text-[#8b949e]'}`}>
            {liveStatus === 'connected' ? <Wifi size={12} /> : <WifiOff size={12} />}
            <span>{liveStatus === 'connected' ? 'Live' : 'Connecting…'}</span>
          </div>
          <button
            onClick={refresh}
            className="flex items-center gap-1.5 text-xs text-[#8b949e] hover:text-[#e6edf3] transition-colors"
          >
            <RefreshCw size={13} />
          </button>
        </div>
      </header>

      {/* Filter bar */}
      <div className="px-5 py-3 border-b border-[#21262d] bg-[#0d1117] flex-shrink-0">
        <FilterBar
          filter={filter}
          onChange={setFilter}
          epicOptions={epics.map(e => ({ id: e.id, name: e.name }))}
          counts={counts}
        />
      </div>

      {/* Board scroll area */}
      <div className="flex-1 overflow-auto">
        {epics.length === 0 ? (
          <EmptyState />
        ) : (
          <div className="min-w-max">
            {/* Column headers — sticky */}
            <div className="sticky top-0 z-10 bg-[#0d1117] border-b border-[#21262d] flex">
              {/* Row label spacer */}
              <div className="w-64 flex-shrink-0 px-4 py-3 border-r border-[#21262d]" />
              {COLUMNS.map(col => (
                <div
                  key={col.state}
                  className="flex-1 min-w-[220px] px-4 py-3 border-r border-[#21262d] last:border-r-0 flex items-center gap-2"
                >
                  <span className={`text-xs font-semibold uppercase tracking-wider ${col.headerColor}`}>
                    {col.label}
                  </span>
                  <span className={`text-xs px-1.5 py-0.5 rounded-full font-medium ${col.countColor}`}>
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

              // Hide completed epics when toggle is on (unless a filter would reveal them)
              if (hideCompleted && isDone && !hasFilter) return null

              // Skip epics with no matching tasks when filtering
              if (hasFilter && epicTasks.filter(t => filteredTaskIds.has(t.id)).length === 0) return null

              return (
                <div key={epic.id} className="border-b border-[#21262d]">
                   {/* Epic header row */}
                   <div
                     className="flex items-center cursor-pointer group hover:bg-[#161b22] transition-colors border-b border-[#21262d]"
                     onClick={() => toggleEpic(epic.id)}
                   >
                      <div className="w-64 flex-shrink-0 px-4 py-3 border-r border-[#21262d] flex items-center gap-2 relative">
                        <span className="text-[#484f58] group-hover:text-[#8b949e] transition-colors">
                          {isEpicCollapsed ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                        </span>
                        <div className="min-w-0">
                          <div className="text-xs font-semibold text-[#e6edf3] truncate">{epic.name}</div>
                          <div className="text-xs text-[#484f58]">
                            {epicStories.length} stor{epicStories.length !== 1 ? 'ies' : 'y'} · {epicTasks.length} task{epicTasks.length !== 1 ? 's' : ''}
                          </div>
                        </div>
                        <button
                          onClick={e => {
                            e.stopPropagation()
                            setStoryForm({ name: '', description: '', acceptance_criteria: '', priority: '1' })
                            setStoryModal({ mode: 'create', epicId: epic.id })
                          }}
                          className="p-0.5 opacity-0 group-hover:opacity-100 hover:bg-[#21262d] rounded transition-all"
                          title="Add story"
                        >
                          <Plus size={11} className="text-[#8b949e]" />
                        </button>
                        <button
                          onClick={e => { e.stopPropagation(); setOpenEpicMenu(openEpicMenu === epic.id ? null : epic.id) }}
                          className="p-0.5 opacity-0 group-hover:opacity-100 hover:bg-[#21262d] rounded transition-all"
                        >
                          <MoreHorizontal size={12} className="text-[#8b949e]" />
                        </button>
                       {openEpicMenu === epic.id && (
                         <div className="absolute left-52 top-8 z-20 bg-[#161b22] border border-[#30363d] rounded-md shadow-lg py-1 min-w-[100px]">
                           <button
                             onClick={e => {
                               e.stopPropagation()
                               setEpicForm({ name: epic.name, description: epic.description || '', priority: epic.priority || '1' })
                               setEpicModal({ mode: 'edit', epic })
                               setOpenEpicMenu(null)
                             }}
                             className="w-full text-left px-3 py-1.5 text-xs text-[#e6edf3] hover:bg-[#21262d] transition-colors"
                           >
                             Edit
                           </button>
                           <button
                             onClick={e => {
                               e.stopPropagation()
                               setEpicDeleteTarget(epic)
                               setOpenEpicMenu(null)
                             }}
                             className="w-full text-left px-3 py-1.5 text-xs text-[#f85149] hover:bg-[#21262d] transition-colors"
                           >
                             Delete
                           </button>
                         </div>
                       )}
                     </div>
                     {/* Epic task counts per column */}
                     {COLUMNS.map(col => {
                       let n = 0
                       if (col.state === 'scheduled') {
                         n = epicTasks.filter(t => isScheduledTask(t)).length
                       } else if (col.state === 'blocked') {
                         n = epicTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t)).length
                       } else {
                         n = epicTasks.filter(t => t.state === col.state).length
                       }
                       return (
                         <div key={col.state} className="flex-1 min-w-[220px] px-4 py-3 border-r border-[#21262d] last:border-r-0">
                           {n > 0 && (
                             <span className={`text-xs px-1.5 py-0.5 rounded-full font-medium ${col.countColor}`}>{n}</span>
                           )}
                         </div>
                       )
                     })}
                  </div>

                  {/* Stories */}
                  {!isEpicCollapsed && epicStories.map(story => {
                    const storyTasks = tasks.filter(t => t.story_id === story.id)
                    const isStoryCollapsed = collapsedStories.has(story.id)

                    // Skip stories with no matching tasks when filtering
                    if (hasFilter && storyTasks.filter(t => filteredTaskIds.has(t.id)).length === 0) return null

                    return (
                      <div key={story.id} className="border-b border-[#21262d]/60 last:border-b-0">
                         {/* Story header row */}
                         <div
                           className="flex items-center cursor-pointer group hover:bg-[#0d1117] transition-colors"
                           onClick={() => toggleStory(story.id)}
                         >
                           <div className="w-64 flex-shrink-0 px-4 py-2.5 pl-8 border-r border-[#21262d] flex items-center gap-2 relative">
                             <span className="text-[#30363d] group-hover:text-[#484f58] transition-colors">
                               {isStoryCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
                             </span>
                            <div className="min-w-0">
                                <div className="text-xs text-[#8b949e] truncate">{story.name}</div>
                                <div className="text-xs text-[#484f58]">{storyTasks.length} task{storyTasks.length !== 1 ? 's' : ''}</div>
                              </div>
                              <button
                                onClick={e => {
                                  e.stopPropagation()
                                  setTaskForm({ name: '', description: '', acceptance_criteria: '', assigned_to: 'amp-worker', priority: '1' })
                                  setTaskCreateModal({ story })
                                }}
                                className="p-0.5 opacity-0 group-hover:opacity-100 hover:bg-[#21262d] rounded transition-all"
                                title="Add task"
                              >
                                <Plus size={11} className="text-[#8b949e]" />
                              </button>
                              <button
                                onClick={e => { e.stopPropagation(); setOpenStoryMenu(openStoryMenu === story.id ? null : story.id) }}
                                className="p-0.5 opacity-0 group-hover:opacity-100 hover:bg-[#21262d] rounded transition-all"
                              >
                                <MoreHorizontal size={11} className="text-[#8b949e]" />
                              </button>
                             {openStoryMenu === story.id && (
                               <div className="absolute left-48 top-7 z-20 bg-[#161b22] border border-[#30363d] rounded-md shadow-lg py-1 min-w-[100px]">
                                 <button
                                   onClick={e => {
                                     e.stopPropagation()
                                     setStoryForm({
                                       name: story.name,
                                       description: story.description || '',
                                       acceptance_criteria: story.acceptance_criteria || '',
                                       priority: story.priority || '1',
                                     })
                                     setStoryModal({ mode: 'edit', story })
                                     setOpenStoryMenu(null)
                                   }}
                                   className="w-full text-left px-3 py-1.5 text-xs text-[#e6edf3] hover:bg-[#21262d] transition-colors"
                                 >
                                   Edit
                                 </button>
                                 <button
                                   onClick={e => {
                                     e.stopPropagation()
                                     setStoryDeleteTarget(story)
                                     setOpenStoryMenu(null)
                                   }}
                                   className="w-full text-left px-3 py-1.5 text-xs text-[#f85149] hover:bg-[#21262d] transition-colors"
                                 >
                                   Delete
                                 </button>
                               </div>
                             )}
                           </div>
                          {/* Story task counts per column */}
                          {COLUMNS.map(col => {
                            let n = 0
                            if (col.state === 'scheduled') {
                              n = storyTasks.filter(t => isScheduledTask(t)).length
                            } else if (col.state === 'blocked') {
                              n = storyTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t)).length
                            } else {
                              n = storyTasks.filter(t => t.state === col.state).length
                            }
                            return (
                              <div key={col.state} className="flex-1 min-w-[220px] px-4 py-2.5 border-r border-[#21262d] last:border-r-0">
                                {n > 0 && (
                                  <span className={`text-xs px-1.5 py-0.5 rounded-full ${col.countColor}`}>{n}</span>
                                )}
                              </div>
                            )
                          })}
                        </div>

                        {/* Task rows */}
                        {!isStoryCollapsed && (
                          <div className="flex">
                            {/* Label gutter */}
                            <div className="w-64 flex-shrink-0 border-r border-[#21262d]" />
                            {/* Task columns */}
                            {COLUMNS.map(col => {
                              let colTasks: Task[] = []
                              if (col.state === 'scheduled') {
                                colTasks = storyTasks.filter(t => isScheduledTask(t))
                              } else if (col.state === 'blocked') {
                                colTasks = storyTasks.filter(t => t.state === 'blocked' && !isScheduledTask(t))
                              } else {
                                colTasks = storyTasks.filter(t => t.state === col.state)
                              }
                              return (
                                <div
                                  key={col.state}
                                  className="flex-1 min-w-[220px] p-2 border-r border-[#21262d] last:border-r-0 bg-[#0d1117]/30"
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
                                    {colTasks.length === 0 && (
                                      <div className="h-8" /> // empty column spacer
                                    )}
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
         onClose={() => setSelectedTask(null)}
         onRefresh={refresh}
       />

       {/* Epic CRUD modals */}
       {epicModal && (
         <CrudModal
           title={epicModal.mode === 'create' ? 'New Epic' : 'Edit Epic'}
           onClose={() => setEpicModal(null)}
           onSave={handleEpicSave}
           saving={epicSaving}
         >
           <div className="space-y-3">
             <div>
               <label className="text-xs text-[#8b949e] block mb-1">Name *</label>
               <input
                 autoFocus
                 value={epicForm.name}
                 onChange={e => setEpicForm(f => ({ ...f, name: e.target.value }))}
                 className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]"
                 placeholder="Epic name"
               />
             </div>
             <div>
               <label className="text-xs text-[#8b949e] block mb-1">Description</label>
               <textarea
                 value={epicForm.description}
                 onChange={e => setEpicForm(f => ({ ...f, description: e.target.value }))}
                 rows={3}
                 className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff] resize-none"
                 placeholder="Optional description"
               />
             </div>
             <div>
               <label className="text-xs text-[#8b949e] block mb-1">Priority</label>
               <select
                 value={epicForm.priority}
                 onChange={e => setEpicForm(f => ({ ...f, priority: e.target.value }))}
                 className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]"
               >
                 <option value="0">Low</option>
                 <option value="1">Normal</option>
                 <option value="2">High</option>
                 <option value="3">Critical</option>
               </select>
             </div>
           </div>
         </CrudModal>
       )}

        {epicDeleteTarget && (
          <ConfirmDeleteModal
            title={`Delete "${epicDeleteTarget.name}"`}
            description="This will permanently delete the epic and all its stories and tasks."
            onClose={() => setEpicDeleteTarget(null)}
            onConfirm={handleEpicDelete}
            deleting={epicSaving}
          />
        )}

        {/* Story CRUD modals */}
        {storyModal && (
          <CrudModal
            title={storyModal.mode === 'create' ? 'New Story' : 'Edit Story'}
            onClose={() => setStoryModal(null)}
            onSave={handleStorySave}
            saving={storySaving}
          >
            <div className="space-y-3">
              <div>
                <label className="text-xs text-[#8b949e] block mb-1">Name *</label>
                <input
                  autoFocus
                  value={storyForm.name}
                  onChange={e => setStoryForm(f => ({ ...f, name: e.target.value }))}
                  className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]"
                  placeholder="Story name"
                />
              </div>
              <div>
                <label className="text-xs text-[#8b949e] block mb-1">Description</label>
                <textarea
                  value={storyForm.description}
                  onChange={e => setStoryForm(f => ({ ...f, description: e.target.value }))}
                  rows={2}
                  className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff] resize-none"
                />
              </div>
              <div>
                <label className="text-xs text-[#8b949e] block mb-1">Acceptance Criteria</label>
                <textarea
                  value={storyForm.acceptance_criteria}
                  onChange={e => setStoryForm(f => ({ ...f, acceptance_criteria: e.target.value }))}
                  rows={3}
                  className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff] resize-none"
                />
              </div>
              <div>
                <label className="text-xs text-[#8b949e] block mb-1">Priority</label>
                <select
                  value={storyForm.priority}
                  onChange={e => setStoryForm(f => ({ ...f, priority: e.target.value }))}
                  className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]"
                >
                  <option value="0">Low</option>
                  <option value="1">Normal</option>
                  <option value="2">High</option>
                  <option value="3">Critical</option>
                </select>
              </div>
            </div>
          </CrudModal>
        )}

         {storyDeleteTarget && (
           <ConfirmDeleteModal
             title={`Delete "${storyDeleteTarget.name}"`}
             description="This will permanently delete the story and all its tasks."
             onClose={() => setStoryDeleteTarget(null)}
             onConfirm={handleStoryDelete}
             deleting={storySaving}
           />
         )}

         {/* Task create modal */}
         {taskCreateModal && (
           <CrudModal
             title="New Task"
             onClose={() => setTaskCreateModal(null)}
             onSave={handleTaskCreate}
             saving={taskSaving}
           >
             <div className="space-y-3">
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Name *</label>
                 <input
                   autoFocus
                   value={taskForm.name}
                   onChange={e => setTaskForm(f => ({ ...f, name: e.target.value }))}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]"
                   placeholder="Task name"
                 />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Description</label>
                 <textarea
                   value={taskForm.description}
                   onChange={e => setTaskForm(f => ({ ...f, description: e.target.value }))}
                   rows={3}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff] resize-none"
                 />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Acceptance Criteria</label>
                 <textarea
                   value={taskForm.acceptance_criteria}
                   onChange={e => setTaskForm(f => ({ ...f, acceptance_criteria: e.target.value }))}
                   rows={3}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff] resize-none"
                 />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Assigned To</label>
                 <input
                   value={taskForm.assigned_to}
                   onChange={e => setTaskForm(f => ({ ...f, assigned_to: e.target.value }))}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]"
                   placeholder="amp-worker"
                 />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Priority</label>
                 <select
                   value={taskForm.priority}
                   onChange={e => setTaskForm(f => ({ ...f, priority: e.target.value }))}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]"
                 >
                   <option value="0">Low</option>
                   <option value="1">Normal</option>
                   <option value="2">High</option>
                   <option value="3">Critical</option>
                 </select>
               </div>
             </div>
           </CrudModal>
         )}
       </div>
     )
   }

function EmptyState() {
  return (
    <div className="flex flex-col items-center justify-center h-64 gap-3">
      <div className="w-12 h-12 rounded-full bg-[#161b22] border border-[#30363d] flex items-center justify-center">
        <span className="text-2xl">📋</span>
      </div>
      <div className="text-center">
        <p className="text-sm font-medium text-[#e6edf3]">No epics yet</p>
        <p className="text-xs text-[#8b949e] mt-1">Ask the amp-manager to plan your project and tasks will appear here in real time.</p>
      </div>
    </div>
  )
}
