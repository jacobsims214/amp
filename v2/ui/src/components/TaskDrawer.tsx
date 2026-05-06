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

const ACTION_DOT: Record<string, string> = {
  created:    'bg-[#7E91A8]',
  dispatched: 'bg-[#818CF8]',
  completed:  'bg-[#10B981]',
  blocked:    'bg-[#EF4444]',
  unblocked:  'bg-[#FBBF24]',
  comment:    'bg-[#283A57] ring-1 ring-[#7E91A8]/40',
}

const inputCls =
  'w-full bg-[#08101F] border border-[#1E2C45] rounded-lg px-3 py-2 text-sm text-[#DDE6F0] placeholder-[#3D5068] ' +
  'focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all font-[inherit]'

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
    setEditing(false)
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
      <div
        className="fixed inset-0 z-40"
        style={{ background: 'rgba(4, 8, 18, 0.6)', backdropFilter: 'blur(2px)' }}
        onClick={onClose}
      />

      {/* Drawer */}
      <div
        className="fixed right-0 top-0 h-full w-[520px] z-50 flex flex-col"
        style={{
          background: '#0D1726',
          borderLeft: '1px solid #1E2C45',
          boxShadow: '-24px 0 64px rgba(0,0,0,0.5)',
        }}
      >
        {/* Header */}
        <div className="flex items-start justify-between gap-3 px-6 py-5 border-b border-[#1E2C45]">
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2 mb-2">
              <span className="text-xs text-[#3D5068] font-mono tabular-nums">#{task.id}</span>
              <StatusBadge state={task.state} />
              {task.priority === '2' && (
                <span className="text-xs text-[#FBBF24] font-semibold bg-[#F59E0B]/10 px-2 py-0.5 rounded-full ring-1 ring-[#F59E0B]/20">High</span>
              )}
              {task.priority === '3' && (
                <span className="text-xs text-[#F87171] font-semibold bg-[#EF4444]/10 px-2 py-0.5 rounded-full ring-1 ring-[#EF4444]/20">Critical</span>
              )}
            </div>
            <h2 className="text-base font-semibold text-[#DDE6F0] leading-snug">{task.name}</h2>
          </div>
          <div className="flex items-center gap-0.5 flex-shrink-0">
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
              className="p-2 hover:bg-[#172540] rounded-lg transition-colors cursor-pointer"
              title="Edit task"
            >
              <Pencil size={13} className="text-[#7E91A8]" />
            </button>
            <button
              onClick={() => setShowDeleteConfirm(true)}
              className="p-2 hover:bg-[#EF4444]/10 rounded-lg transition-colors cursor-pointer"
              title="Delete task"
            >
              <Trash2 size={13} className="text-[#EF4444]/70 hover:text-[#EF4444]" />
            </button>
            <button onClick={onClose} className="p-2 hover:bg-[#172540] rounded-lg transition-colors cursor-pointer">
              <X size={15} className="text-[#7E91A8]" />
            </button>
          </div>
        </div>

        {/* Meta strip */}
        <div className="flex items-center gap-3 px-6 py-2.5 border-b border-[#192238] bg-[#08101F]/50 flex-wrap">
          {task.assigned_to && !task.agent_id && (
            <div className="flex items-center gap-1.5 text-xs text-[#818CF8]" title="Planned assignee">
              <UserCheck size={11} />
              <span className="font-medium">{task.assigned_to}</span>
              <span className="text-[#3D5068]">(planned)</span>
            </div>
          )}
          {task.agent_id && (
            <div className="flex items-center gap-1.5 text-xs text-[#10B981]" title="Active agent">
              <User size={11} />
              <span className="font-medium">{task.agent_id}</span>
            </div>
          )}
          {task.dispatched_at && (
            <div className="flex items-center gap-1.5 text-xs text-[#7E91A8]">
              <Clock size={11} />
              <span>Started {relativeTime(task.dispatched_at)}</span>
            </div>
          )}
          {task.start_at && (
            <div className="flex items-center gap-1.5 text-xs text-[#FBBF24]" title={new Date(task.start_at).toLocaleString()}>
              <CalendarClock size={11} />
              <span>Starts {relativeTime(task.start_at)}</span>
            </div>
          )}
          {task.dependency_ids?.length > 0 && (
            <div className="flex items-center gap-1.5 text-xs text-[#7E91A8]">
              <GitBranch size={11} />
              <span>{task.dependency_ids.length} dep{task.dependency_ids.length > 1 ? 's' : ''}</span>
            </div>
          )}
          {task.blocked_by_ids?.length ? (
            <div className="flex items-center gap-1.5 text-xs text-[#F87171]">
              <AlertCircle size={11} />
              <span>Blocked by #{task.blocked_by_ids.join(', #')}</span>
            </div>
          ) : null}
          {task.state === 'completed' && task.completed_at && (
            <div className="flex items-center gap-1.5 text-xs text-[#10B981]">
              <CheckCircle2 size={11} />
              <span>Done {relativeTime(task.completed_at)}</span>
            </div>
          )}
        </div>

        {/* Tabs */}
        <div className="flex border-b border-[#1E2C45]">
          {(['history', 'details'] as const).map(t => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`px-6 py-3 text-xs font-semibold capitalize transition-all border-b-2 -mb-px cursor-pointer ${
                tab === t
                  ? 'border-[#6366F1] text-[#818CF8]'
                  : 'border-transparent text-[#7E91A8] hover:text-[#DDE6F0]'
              }`}
            >
              {t}
            </button>
          ))}
        </div>

        {/* Content */}
        <div className="flex-1 overflow-y-auto p-6">
          {editing ? (
            <div className="space-y-4">
              <div>
                <label className="text-xs font-medium text-[#7E91A8] block mb-1.5">Name</label>
                <input value={editForm.name} onChange={e => setEditForm(f => ({ ...f, name: e.target.value }))}
                  className={inputCls} />
              </div>
              <div>
                <label className="text-xs font-medium text-[#7E91A8] block mb-1.5">Description</label>
                <textarea value={editForm.description} onChange={e => setEditForm(f => ({ ...f, description: e.target.value }))}
                  rows={4} className={`${inputCls} resize-none`} />
              </div>
              <div>
                <label className="text-xs font-medium text-[#7E91A8] block mb-1.5">Acceptance Criteria</label>
                <textarea value={editForm.acceptance_criteria} onChange={e => setEditForm(f => ({ ...f, acceptance_criteria: e.target.value }))}
                  rows={3} className={`${inputCls} resize-none`} />
              </div>
              <div>
                <label className="text-xs font-medium text-[#7E91A8] block mb-1.5">Assigned To</label>
                <input value={editForm.assigned_to} onChange={e => setEditForm(f => ({ ...f, assigned_to: e.target.value }))}
                  className={inputCls} />
              </div>
              <div>
                <label className="text-xs font-medium text-[#7E91A8] block mb-1.5">Priority</label>
                <select value={editForm.priority} onChange={e => setEditForm(f => ({ ...f, priority: e.target.value }))}
                  className={inputCls}>
                  <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
                </select>
              </div>
              <div className="flex gap-2 pt-1">
                <button onClick={handleEditSave} disabled={editSaving}
                  className="flex items-center gap-1.5 px-4 py-2 bg-[#6366F1] hover:bg-[#4F46E5] disabled:opacity-50 text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer">
                  {editSaving && <Loader2 size={11} className="animate-spin" />}
                  Save changes
                </button>
                <button onClick={() => setEditing(false)}
                  className="px-4 py-2 text-xs font-medium text-[#7E91A8] hover:text-[#DDE6F0] hover:bg-[#172540] rounded-lg transition-colors cursor-pointer">
                  Cancel
                </button>
              </div>
            </div>
          ) : loading ? (
            <div className="flex justify-center py-10">
              <Loader2 size={20} className="text-[#7E91A8] animate-spin" />
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

  const inputCls =
    'w-full bg-[#08101F] border border-[#1E2C45] rounded-lg px-3 py-2 text-sm text-[#DDE6F0] ' +
    'focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all font-[inherit]'

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
          <div className="text-xs font-semibold text-[#3D5068] uppercase tracking-widest mb-2">Description</div>
          <div className="text-sm text-[#DDE6F0] whitespace-pre-wrap leading-relaxed bg-[#08101F] rounded-xl p-4 border border-[#192238]">
            {task.description}
          </div>
        </div>
      )}
      {task.acceptance_criteria && (
        <div>
          <div className="text-xs font-semibold text-[#3D5068] uppercase tracking-widest mb-2">Acceptance Criteria</div>
          <div className="text-sm text-[#DDE6F0] whitespace-pre-wrap leading-relaxed bg-[#08101F] rounded-xl p-4 border border-[#192238]">
            {task.acceptance_criteria}
          </div>
        </div>
      )}
      <div className="grid grid-cols-2 gap-2.5">
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
          <div className="text-xs font-semibold text-[#3D5068] uppercase tracking-widest">Scheduled Start</div>
          <button
            onClick={() => { setScheduleValue(task.start_at ? task.start_at.slice(0, 16) : ''); setEditingSchedule(true) }}
            className="text-xs text-[#818CF8] hover:text-[#6366F1] flex items-center gap-1 transition-colors cursor-pointer"
          >
            <Pencil size={10} />
            {task.start_at ? 'Edit' : 'Set schedule'}
          </button>
        </div>
        {!editingSchedule ? (
          task.start_at ? (
            <div className="text-sm text-[#DDE6F0] bg-[#08101F] rounded-xl p-4 border border-[#192238] flex items-center gap-2.5">
              <CalendarClock size={14} className="text-[#FBBF24] flex-shrink-0" />
              <div>
                <div className="font-medium">{new Date(task.start_at).toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</div>
                <div className="text-xs text-[#7E91A8] mt-0.5">{relativeTime(task.start_at)}</div>
              </div>
            </div>
          ) : (
            <div className="text-sm text-[#3D5068] italic">No schedule set</div>
          )
        ) : (
          <div className="bg-[#08101F] rounded-xl p-4 border border-[#1E2C45] space-y-3">
            <div>
              <label className="text-xs font-medium text-[#7E91A8] block mb-1.5">Date & Time (local timezone)</label>
              <input type="datetime-local" value={scheduleValue} onChange={e => setScheduleValue(e.target.value)}
                className={inputCls} />
            </div>
            <div className="flex items-center gap-2">
              <button onClick={handleSaveSchedule} disabled={savingSchedule}
                className="flex items-center gap-1.5 px-3 py-1.5 bg-[#6366F1] hover:bg-[#4F46E5] text-white text-xs font-semibold rounded-lg disabled:opacity-50 transition-all active:scale-[0.97] cursor-pointer">
                {savingSchedule && <Loader2 size={11} className="animate-spin" />}
                Save
              </button>
              {task.start_at && (
                <button
                  onClick={async () => {
                    setSavingSchedule(true)
                    try { await api.setTaskStartAt(task.id, null); setEditingSchedule(false); onClose(); onRefresh?.() }
                    finally { setSavingSchedule(false) }
                  }}
                  disabled={savingSchedule}
                  className="px-3 py-1.5 text-xs text-[#F87171] hover:text-[#EF4444] transition-colors cursor-pointer"
                >
                  Clear schedule
                </button>
              )}
              <button onClick={() => setEditingSchedule(false)}
                className="px-3 py-1.5 text-xs text-[#7E91A8] hover:text-[#DDE6F0] transition-colors cursor-pointer">
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
  if (history.length === 0 && comments.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-12 gap-2">
        <div className="w-8 h-8 rounded-full bg-[#111E30] flex items-center justify-center">
          <Clock size={14} className="text-[#3D5068]" />
        </div>
        <p className="text-sm text-[#3D5068]">No activity yet</p>
      </div>
    )
  }

  return (
    <div className="space-y-0">
      {history.map((entry, i) => (
        <div key={entry.id} className="flex gap-3">
          <div className="flex flex-col items-center flex-shrink-0">
            <div className="mt-1.5">
              <span className={`block w-2 h-2 rounded-full ${ACTION_DOT[entry.action] ?? ACTION_DOT.comment}`} />
            </div>
            {i < history.length - 1 && <div className="w-px flex-1 bg-[#192238] mt-1.5 mb-0" />}
          </div>
          <div className="pb-5 flex-1 min-w-0">
            <div className="flex items-center gap-1.5 mb-1 flex-wrap">
              <span className="text-xs font-semibold text-[#DDE6F0]">{entry.actor}</span>
              <span className="text-[#3D5068] text-xs">·</span>
              <span className="text-xs text-[#7E91A8] capitalize">{entry.action.replace('_', ' ')}</span>
              {entry.from_state && entry.to_state && (
                <span className="text-xs text-[#3D5068]">
                  {entry.from_state} → {entry.to_state}
                </span>
              )}
              <span className="text-xs text-[#3D5068] ml-auto flex-shrink-0 tabular-nums">{relativeTime(entry.created_at)}</span>
            </div>
            {entry.detail && (
              <div className={`text-xs rounded-xl px-3.5 py-2.5 leading-relaxed whitespace-pre-wrap ${
                entry.action === 'comment'
                  ? 'bg-[#08101F] border border-[#192238] text-[#DDE6F0]'
                  : 'text-[#7E91A8]'
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
    <div className="bg-[#08101F] rounded-xl p-3 border border-[#192238]">
      <div className="text-xs text-[#3D5068] font-medium mb-0.5">{label}</div>
      <div className="text-sm text-[#DDE6F0] font-semibold truncate">{value}</div>
    </div>
  )
}
