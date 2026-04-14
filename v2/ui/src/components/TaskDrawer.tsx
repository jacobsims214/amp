import { useEffect, useState } from 'react'
import { X, Clock, User, UserCheck, GitBranch, CheckCircle2, AlertCircle, Loader2, Pencil, Trash2, CalendarClock } from 'lucide-react'
import { api } from '../api/client'
import { StatusBadge } from './StatusBadge'
import { ConfirmDeleteModal } from './ConfirmDeleteModal'
import type { Task, Comment, ActivityLog } from '../types'

interface Props {
  task: Task | null
  onClose: () => void
  onRefresh?: () => void
}

function relativeTime(iso: string) {
  const diff = Date.now() - new Date(iso).getTime()
  if (diff < 0) {
    const abs = Math.abs(diff)
    const mins = Math.floor(abs / 60000)
    const hrs = Math.floor(mins / 60)
    const days = Math.floor(hrs / 24)
    if (days > 0) return `in ${days}d`
    if (hrs > 0) return `in ${hrs}h`
    if (mins > 0) return `in ${mins}m`
    return 'soon'
  }
  const mins = Math.floor(diff / 60000)
  const hrs = Math.floor(mins / 60)
  const days = Math.floor(hrs / 24)
  if (days > 0) return `${days}d ago`
  if (hrs > 0) return `${hrs}h ago`
  if (mins > 0) return `${mins}m ago`
  return 'just now'
}

const ACTION_ICONS: Record<string, React.ReactNode> = {
  created:    <span className="w-2 h-2 rounded-full bg-[#8b949e] mt-1.5 flex-shrink-0" />,
  dispatched: <span className="w-2 h-2 rounded-full bg-[#58a6ff] mt-1.5 flex-shrink-0" />,
  completed:  <span className="w-2 h-2 rounded-full bg-[#3fb950] mt-1.5 flex-shrink-0" />,
  blocked:    <span className="w-2 h-2 rounded-full bg-[#f85149] mt-1.5 flex-shrink-0" />,
  unblocked:  <span className="w-2 h-2 rounded-full bg-[#e3b341] mt-1.5 flex-shrink-0" />,
  comment:    <span className="w-2 h-2 rounded-full bg-[#8b949e]/50 mt-1.5 flex-shrink-0 border border-[#8b949e]" />,
}

export function TaskDrawer({ task, onClose, onRefresh }: Props) {
  const [history, setHistory] = useState<ActivityLog[]>([])
  const [comments, setComments] = useState<Comment[]>([])
  const [tab, setTab] = useState<'history' | 'details'>('history')
  const [loading, setLoading] = useState(false)
  const [editing, setEditing] = useState(false)
  const [editForm, setEditForm] = useState({ name: '', description: '', acceptance_criteria: '', assigned_to: '', priority: '1' })
  const [editSaving, setEditSaving] = useState(false)
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false)
  const [deleting, setDeleting] = useState(false)

  useEffect(() => {
    if (!task) return
    setLoading(true)
    Promise.all([api.getHistory(task.id), api.getComments(task.id)])
      .then(([h, c]) => { setHistory(h); setComments(c) })
      .finally(() => setLoading(false))
  }, [task?.id])

  const handleEditSave = async () => {
    setEditSaving(true)
    try {
      await api.updateTask(task!.id, editForm)
      setEditing(false)
      onRefresh?.()
      onClose()
    } finally {
      setEditSaving(false)
    }
  }

  const handleDelete = async () => {
    setDeleting(true)
    try {
      await api.deleteTask(task!.id)
      setShowDeleteConfirm(false)
      onClose()
      onRefresh?.()
    } finally {
      setDeleting(false)
    }
  }

  if (!task) return null

  return (
    <>
      {/* Backdrop */}
      <div className="fixed inset-0 bg-black/50 z-40" onClick={onClose} />

      {/* Drawer */}
      <div className="fixed right-0 top-0 h-full w-[520px] bg-[#161b22] border-l border-[#30363d] z-50 flex flex-col shadow-2xl">
       {/* Header */}
         <div className="flex items-start justify-between gap-3 p-5 border-b border-[#30363d]">
           <div className="flex-1 min-w-0">
             <div className="flex items-center gap-2 mb-1.5">
               <span className="text-xs text-[#8b949e] font-mono">#{task.id}</span>
               <StatusBadge state={task.state} />
               {task.priority === '2' && <span className="text-xs text-[#e3b341] font-medium">High</span>}
               {task.priority === '3' && <span className="text-xs text-[#f85149] font-medium">Critical</span>}
             </div>
             <h2 className="text-base font-semibold text-[#e6edf3] leading-snug">{task.name}</h2>
           </div>
           <div className="flex items-center gap-1 flex-shrink-0">
             <button
               onClick={() => {
                 setEditForm({
                   name: task.name,
                   description: task.description || '',
                   acceptance_criteria: task.acceptance_criteria || '',
                   assigned_to: task.assigned_to || '',
                   priority: task.priority || '1',
                 })
                 setEditing(true)
               }}
               className="p-1.5 hover:bg-[#21262d] rounded-md transition-colors"
               title="Edit task"
             >
               <Pencil size={14} className="text-[#8b949e]" />
             </button>
             <button
               onClick={() => setShowDeleteConfirm(true)}
               className="p-1.5 hover:bg-[#21262d] rounded-md transition-colors"
               title="Delete task"
             >
               <Trash2 size={14} className="text-[#f85149]" />
             </button>
             <button onClick={onClose} className="p-1.5 hover:bg-[#21262d] rounded-md transition-colors">
               <X size={16} className="text-[#8b949e]" />
             </button>
           </div>
         </div>

         {/* Meta strip */}
         <div className="flex items-center gap-4 px-5 py-2.5 border-b border-[#21262d] bg-[#0d1117]/30 flex-wrap">
           {/* Planned assignee — shown before dispatch */}
           {task.assigned_to && !task.agent_id && (
             <div className="flex items-center gap-1.5 text-xs text-[#388bfd]" title="Planned assignee — set at planning time">
               <UserCheck size={11} />
               <span className="font-medium">{task.assigned_to}</span>
               <span className="text-[#484f58] font-normal">(planned)</span>
             </div>
           )}
           {/* Active agent — shown once dispatched */}
           {task.agent_id && (
             <div className="flex items-center gap-1.5 text-xs text-[#3fb950]" title="Currently working this task">
               <User size={11} />
               <span className="font-medium">{task.agent_id}</span>
               {task.assigned_to && task.assigned_to !== task.agent_id && (
                 <span className="text-[#484f58] font-normal">(planned: {task.assigned_to})</span>
               )}
             </div>
           )}
           {task.dispatched_at && (
             <div className="flex items-center gap-1.5 text-xs text-[#8b949e]">
               <Clock size={11} />
               <span>Started {relativeTime(task.dispatched_at)}</span>
             </div>
           )}
           {task.start_at && (
             <div className="flex items-center gap-1.5 text-xs text-[#d29922]" title={new Date(task.start_at).toLocaleString()}>
               <CalendarClock size={11} />
               <span>Starts {relativeTime(task.start_at)}</span>
             </div>
           )}
           {task.dependency_ids?.length > 0 && (
             <div className="flex items-center gap-1.5 text-xs text-[#8b949e]">
               <GitBranch size={11} />
               <span>{task.dependency_ids.length} dep{task.dependency_ids.length > 1 ? 's' : ''}</span>
             </div>
           )}
           {task.blocked_by_ids?.length ? (
             <div className="flex items-center gap-1.5 text-xs text-[#f85149]">
               <AlertCircle size={11} />
               <span>Blocked by #{task.blocked_by_ids.join(', #')}</span>
             </div>
           ) : null}
           {task.state === 'completed' && task.completed_at && (
             <div className="flex items-center gap-1.5 text-xs text-[#3fb950]">
               <CheckCircle2 size={11} />
               <span>Done {relativeTime(task.completed_at)}</span>
             </div>
           )}
         </div>

        {/* Tabs */}
        <div className="flex border-b border-[#30363d]">
          {(['history', 'details'] as const).map(t => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`px-5 py-2.5 text-xs font-medium capitalize transition-colors border-b-2 -mb-px ${
                tab === t
                  ? 'border-[#58a6ff] text-[#58a6ff]'
                  : 'border-transparent text-[#8b949e] hover:text-[#e6edf3]'
              }`}
            >
              {t}
            </button>
          ))}
        </div>

         {/* Content */}
         <div className="flex-1 overflow-y-auto p-5">
           {editing ? (
             <div className="flex-1 overflow-y-auto space-y-4">
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Name</label>
                 <input value={editForm.name} onChange={e => setEditForm(f => ({ ...f, name: e.target.value }))}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]" />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Description</label>
                 <textarea value={editForm.description} onChange={e => setEditForm(f => ({ ...f, description: e.target.value }))}
                   rows={4} className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff] resize-none" />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Acceptance Criteria</label>
                 <textarea value={editForm.acceptance_criteria} onChange={e => setEditForm(f => ({ ...f, acceptance_criteria: e.target.value }))}
                   rows={3} className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff] resize-none" />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Assigned To</label>
                 <input value={editForm.assigned_to} onChange={e => setEditForm(f => ({ ...f, assigned_to: e.target.value }))}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]" />
               </div>
               <div>
                 <label className="text-xs text-[#8b949e] block mb-1">Priority</label>
                 <select value={editForm.priority} onChange={e => setEditForm(f => ({ ...f, priority: e.target.value }))}
                   className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]">
                   <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
                 </select>
               </div>
               <div className="flex gap-2 pt-2">
                 <button onClick={handleEditSave} disabled={editSaving}
                   className="flex items-center gap-1.5 px-3 py-1.5 bg-[#238636] hover:bg-[#2ea043] disabled:opacity-50 text-white text-xs rounded-md transition-colors">
                   {editSaving && <Loader2 size={11} className="animate-spin" />}
                   Save
                 </button>
                 <button onClick={() => setEditing(false)}
                   className="px-3 py-1.5 text-xs text-[#8b949e] hover:text-[#e6edf3] transition-colors">
                   Cancel
                 </button>
               </div>
             </div>
           ) : loading ? (
             <div className="flex justify-center py-8">
               <Loader2 size={20} className="text-[#8b949e] animate-spin" />
             </div>
           ) : tab === 'details' ? (
             <DetailsTab task={task} onClose={onClose} onRefresh={onRefresh} />
           ) : (
             <HistoryTab history={history} comments={comments} />
           )}
         </div>
       </div>

       {showDeleteConfirm && (
         <ConfirmDeleteModal
           title={`Delete task #${task!.id}`}
           description={`"${task!.name}" will be permanently deleted.`}
           onClose={() => setShowDeleteConfirm(false)}
           onConfirm={handleDelete}
           deleting={deleting}
         />
       )}
     </>
   )
 }

function DetailsTab({ task, onClose, onRefresh }: { task: Task; onClose: () => void; onRefresh?: () => void }) {
  const [editingSchedule, setEditingSchedule] = useState(false)
  const [scheduleValue, setScheduleValue] = useState('')
  const [savingSchedule, setSavingSchedule] = useState(false)

  const handleSaveSchedule = async () => {
    setSavingSchedule(true)
    try {
      const iso = scheduleValue ? new Date(scheduleValue).toISOString() : null
      await api.setTaskStartAt(task.id, iso)
      setEditingSchedule(false)
      onClose()
      onRefresh?.()
    } finally {
      setSavingSchedule(false)
    }
  }

  return (
    <div className="space-y-5">
      {task.description && (
        <div>
          <div className="text-xs font-medium text-[#8b949e] uppercase tracking-wide mb-2">Description</div>
          <div className="text-sm text-[#e6edf3] whitespace-pre-wrap leading-relaxed bg-[#0d1117] rounded-lg p-3 border border-[#21262d]">
            {task.description}
          </div>
        </div>
      )}
      {task.acceptance_criteria && (
        <div>
          <div className="text-xs font-medium text-[#8b949e] uppercase tracking-wide mb-2">Acceptance Criteria</div>
          <div className="text-sm text-[#e6edf3] whitespace-pre-wrap leading-relaxed bg-[#0d1117] rounded-lg p-3 border border-[#21262d]">
            {task.acceptance_criteria}
          </div>
        </div>
      )}
      <div className="grid grid-cols-2 gap-3">
        <MetaItem label="Task ID" value={`#${task.id}`} />
        <MetaItem label="State" value={task.state} />
        <MetaItem label="Created" value={relativeTime(task.created_at)} />
        <MetaItem label="Updated" value={relativeTime(task.updated_at)} />
        {task.dependency_ids?.length > 0 && (
          <MetaItem label="Depends on" value={task.dependency_ids.map(id => `#${id}`).join(', ')} />
        )}
      </div>

      {/* Scheduled Start */}
      <div>
        <div className="flex items-center justify-between mb-2">
          <div className="text-xs font-medium text-[#8b949e] uppercase tracking-wide">Scheduled Start</div>
          <button onClick={() => { setScheduleValue(task.start_at ? task.start_at.slice(0, 16) : ''); setEditingSchedule(true) }}
            className="text-xs text-[#58a6ff] hover:text-[#79c0ff] flex items-center gap-1 transition-colors">
            <Pencil size={10} />
            {task.start_at ? 'Edit' : 'Set schedule'}
          </button>
        </div>
        {!editingSchedule ? (
          task.start_at ? (
            <div className="text-sm text-[#e6edf3] bg-[#0d1117] rounded-lg p-3 border border-[#21262d] flex items-center gap-2">
              <CalendarClock size={14} className="text-[#d29922] flex-shrink-0" />
              <div>
                <div>{new Date(task.start_at).toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</div>
                <div className="text-xs text-[#8b949e] mt-0.5">{relativeTime(task.start_at)}</div>
              </div>
            </div>
          ) : (
            <div className="text-sm text-[#484f58] italic">No schedule set</div>
          )
        ) : (
          <div className="bg-[#0d1117] rounded-lg p-3 border border-[#30363d] space-y-3">
            <div>
              <label className="text-xs text-[#8b949e] block mb-1">Date & Time (local timezone)</label>
              <input type="datetime-local" value={scheduleValue} onChange={e => setScheduleValue(e.target.value)}
                className="w-full bg-[#161b22] border border-[#30363d] rounded-md px-3 py-1.5 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]" />
            </div>
            <div className="flex items-center gap-2">
              <button onClick={handleSaveSchedule} disabled={savingSchedule}
                className="flex items-center gap-1.5 px-3 py-1.5 bg-[#238636] hover:bg-[#2ea043] text-white text-xs rounded-md disabled:opacity-50 transition-colors">
                {savingSchedule && <Loader2 size={11} className="animate-spin" />}
                Save
              </button>
              {task.start_at && (
                <button onClick={async () => { setSavingSchedule(true); try { await api.setTaskStartAt(task.id, null); setEditingSchedule(false); onClose(); onRefresh?.() } finally { setSavingSchedule(false) } }}
                  disabled={savingSchedule} className="px-3 py-1.5 text-xs text-[#f85149] hover:text-[#ff7b72] transition-colors">
                  Clear schedule
                </button>
              )}
              <button onClick={() => setEditingSchedule(false)} className="px-3 py-1.5 text-xs text-[#8b949e] hover:text-[#e6edf3] transition-colors">
                Cancel
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

function HistoryTab({ history, comments }: { history: ActivityLog[]; comments: Comment[] }) {
  // Merge history + comments into a unified timeline (history already contains comments as entries)
  if (history.length === 0 && comments.length === 0) {
    return <p className="text-sm text-[#8b949e] text-center py-8">No activity yet</p>
  }

  return (
    <div className="space-y-0">
      {history.map((entry, i) => (
        <div key={entry.id} className="flex gap-3">
          <div className="flex flex-col items-center">
            <div className="mt-1">{ACTION_ICONS[entry.action] ?? ACTION_ICONS.comment}</div>
            {i < history.length - 1 && <div className="w-px flex-1 bg-[#21262d] mt-1" />}
          </div>
          <div className="pb-5 flex-1 min-w-0">
            <div className="flex items-center gap-2 mb-1">
              <span className="text-xs font-medium text-[#e6edf3]">{entry.actor}</span>
              <span className="text-xs text-[#484f58]">·</span>
              <span className="text-xs text-[#8b949e] capitalize">{entry.action.replace('_', ' ')}</span>
              {entry.from_state && entry.to_state && (
                <span className="text-xs text-[#8b949e]">
                  {entry.from_state} → {entry.to_state}
                </span>
              )}
              <span className="text-xs text-[#484f58] ml-auto flex-shrink-0">{relativeTime(entry.created_at)}</span>
            </div>
            {entry.detail && (
              <div className={`text-xs rounded-md p-3 leading-relaxed whitespace-pre-wrap ${
                entry.action === 'comment'
                  ? 'bg-[#0d1117] border border-[#21262d] text-[#e6edf3]'
                  : 'text-[#8b949e]'
              }`}>
                {entry.detail}
              </div>
            )}
          </div>
        </div>
      ))}
    </div>
  )
}

function MetaItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-[#0d1117] rounded-md p-3 border border-[#21262d]">
      <div className="text-xs text-[#8b949e] mb-0.5">{label}</div>
      <div className="text-sm text-[#e6edf3] font-medium truncate">{value}</div>
    </div>
  )
}
