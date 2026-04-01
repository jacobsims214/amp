import { useState, useMemo } from 'react'
import { useParams, Link } from 'react-router-dom'
import { ArrowLeft, RefreshCw, Loader2, Wifi, WifiOff, ChevronDown, ChevronRight, EyeOff, Eye, GitBranch, BarChart2, BookOpen } from 'lucide-react'
import { useBoardData } from '../hooks/useBoardData'
import { useSSE } from '../hooks/useSSE'
import { TaskCard } from '../components/TaskCard'
import { TaskDrawer } from '../components/TaskDrawer'
import { FilterBar, type FilterState } from '../components/FilterBar'
import type { Task, TaskState } from '../types'

const COLUMNS: { state: TaskState; label: string; headerColor: string; countColor: string }[] = [
  { state: 'backlog',     label: 'Backlog',      headerColor: 'text-[#8b949e]',  countColor: 'bg-[#8b949e]/15 text-[#8b949e]' },
  { state: 'in_progress', label: 'In Progress',  headerColor: 'text-[#58a6ff]',  countColor: 'bg-[#58a6ff]/15 text-[#58a6ff]' },
  { state: 'blocked',     label: 'Blocked',      headerColor: 'text-[#f85149]',  countColor: 'bg-[#f85149]/15 text-[#f85149]' },
  { state: 'completed',   label: 'Done',          headerColor: 'text-[#3fb950]',  countColor: 'bg-[#3fb950]/15 text-[#3fb950]' },
]

export function Board() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)

  const { epics, stories, tasks, loading, error, refresh } = useBoardData(pid)
  const [selectedTask, setSelectedTask] = useState<Task | null>(null)
  const [filter, setFilter] = useState<FilterState>({ query: '', state: '', epicId: null })
  const [collapsedEpics, setCollapsedEpics] = useState<Set<number>>(new Set())
  const [collapsedStories, setCollapsedStories] = useState<Set<number>>(new Set())
  const [hideCompleted, setHideCompleted] = useState(false)
  const [liveStatus, setLiveStatus] = useState<'connected' | 'disconnected'>('disconnected')

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

  // Filter logic
  const q = filter.query.toLowerCase()
  const filteredTasks = useMemo(() => {
    return tasks.filter(t => {
      if (filter.state && t.state !== filter.state) return false
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
    backlog:     filteredTasks.filter(t => t.state === 'backlog').length,
    in_progress: filteredTasks.filter(t => t.state === 'in_progress').length,
    completed:   filteredTasks.filter(t => t.state === 'completed').length,
    blocked:     filteredTasks.filter(t => t.state === 'blocked').length,
    total:       filteredTasks.length,
  }), [filteredTasks])

  const toggleEpic = (id: number) =>
    setCollapsedEpics(prev => { const s = new Set(prev); s.has(id) ? s.delete(id) : s.add(id); return s })
  const toggleStory = (id: number) =>
    setCollapsedStories(prev => { const s = new Set(prev); s.has(id) ? s.delete(id) : s.add(id); return s })

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
                    <div className="w-64 flex-shrink-0 px-4 py-3 border-r border-[#21262d] flex items-center gap-2">
                      <span className="text-[#484f58] group-hover:text-[#8b949e] transition-colors">
                        {isEpicCollapsed ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                      </span>
                      <div className="min-w-0">
                        <div className="text-xs font-semibold text-[#e6edf3] truncate">{epic.name}</div>
                        <div className="text-xs text-[#484f58]">
                          {epicStories.length} stor{epicStories.length !== 1 ? 'ies' : 'y'} · {epicTasks.length} task{epicTasks.length !== 1 ? 's' : ''}
                        </div>
                      </div>
                    </div>
                    {/* Epic task counts per column */}
                    {COLUMNS.map(col => {
                      const n = epicTasks.filter(t => t.state === col.state).length
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
                          <div className="w-64 flex-shrink-0 px-4 py-2.5 pl-8 border-r border-[#21262d] flex items-center gap-2">
                            <span className="text-[#30363d] group-hover:text-[#484f58] transition-colors">
                              {isStoryCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
                            </span>
                            <div className="min-w-0">
                              <div className="text-xs text-[#8b949e] truncate">{story.name}</div>
                              <div className="text-xs text-[#484f58]">{storyTasks.length} task{storyTasks.length !== 1 ? 's' : ''}</div>
                            </div>
                          </div>
                          {/* Story task counts per column */}
                          {COLUMNS.map(col => {
                            const n = storyTasks.filter(t => t.state === col.state).length
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
                              const colTasks = storyTasks.filter(t => t.state === col.state)
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
      />
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
