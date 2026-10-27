import { useEffect, useMemo, useState } from 'react'
import { X, Clock, Bot, GitBranch, CheckCircle2, AlertCircle, Loader2, Pencil, Trash2, CalendarClock, Hand, UserMinus, UserRound, ShieldCheck, ShieldAlert, ArrowRight, GitMerge, Check } from 'lucide-react'
import { Link } from 'react-router-dom'
import { HUMAN_REASONS, isHuman, reasonHint, reasonLabel } from '../lib/human'
import { approvalOf, approvalOwner, formatWhen, isRelayed, isYours, relayedFor } from '../lib/approval'
import { projectLink } from '../lib/projectLink'
import { HumanActions } from './human/HumanActions'
import { mergeStatusText } from '../lib/humanActions'
import { effectiveMetadata, integrationBranch, mergedIntoIntegration, waitsForMerge } from '../lib/merge'
import { api } from '../api/client'
import { TicketArtifacts } from './artifacts/ArtifactBits'
import { withClears } from '../lib/clear'
import { StatusBadge } from './StatusBadge'
import { PriorityMark } from './ui/PriorityMark'
import { displayState } from '../lib/ticketState'
import { ConfirmDeleteModal } from './ConfirmDeleteModal'
import { OwnerAvatar, UnassignedPill } from './OwnerAvatar'
import { activityActor, canPlanTask, canTakeOverTask, canWorkTask, lookupPerson, peopleById, personName, type Person } from '../lib/people'
import type { Task, Comment, ActivityLog, ProjectMember, ProjectAccess, Metadata, Epic, Story, HumanReason, Run } from '../types'
import { DependencyPicker } from './DependencyPicker'
import { MetadataEditor } from './MetadataEditor'
import { metadataPatch } from '../lib/metadata'
import { Markdown } from './ui/Markdown'
import { RunsPanel } from './RunsPanel'

interface Props {
  task: Task | null
  onClose: () => void
  onRefresh?: () => void
  /** project members, for owner names and the assign select */
  members?: ProjectMember[]
  /** caller's user id; undefined in local-dev mode */
  meId?: number
  /** caller's project access (my_access); gates working others' tickets */
  myAccess?: ProjectAccess | null
  /** the project's open-reassign setting */
  openReassign?: boolean
  /** called with the updated task after claim/assign/release */
  onTaskUpdated?: (task: Task) => void
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
  created:    'bg-ink-2',
  dispatched: 'bg-link',
  started:    'bg-link',
  completed:  'bg-success',
  blocked:    'bg-danger',
  unblocked:  'bg-warning-ink',
  comment:    'bg-line-str ring-1 ring-ink-2/40',
  plan_approved: 'bg-success',
}

const inputCls =
  'w-full bg-bg border border-line rounded-lg px-3 py-2 text-sm text-ink placeholder-ink-3 ' +
  'focus:outline-none focus:border-primary focus:ring-2 focus:ring-primary/20 transition-all font-[inherit]'

export function TaskDrawer({ task, onClose, onRefresh, members = [], meId, myAccess = null, openReassign = false, onTaskUpdated }: Props) {
  const [history, setHistory] = useState<ActivityLog[]>([])
  const [comments, setComments] = useState<Comment[]>([])
  const [tab, setTab] = useState<'history' | 'details'>('details')
  const [loading, setLoading] = useState(false)
  const [editing, setEditing] = useState(false)
  const [editForm, setEditForm] = useState({ name: '', description: '', acceptance_criteria: '', assigned_to: '', priority: '1',
    performer: 'agent' as 'agent' | 'human', human_reason: 'other' as HumanReason })
  const [editSaving, setEditSaving] = useState(false)
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [deleteError, setDeleteError] = useState<string | null>(null)
  const [editError, setEditError] = useState<string | null>(null)
  const people = useMemo(() => peopleById(members), [members])

  useEffect(() => {
    if (!task) return
    setLoading(true)
    setEditing(false)
    Promise.all([api.getHistory(task.id), api.getComments(task.id)])
      .then(([h, c]) => { setHistory(h); setComments(c) })
      .finally(() => setLoading(false))
    // Reload when ownership or state changes too, so the history shows the
    // assignment or transition that just happened.
  }, [task?.id, task?.owner_user_id, task?.state])

  // Escape closes the drawer, unless a confirm dialog or an edit is open.
  useEffect(() => {
    if (!task) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !editing && !showDeleteConfirm) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [task, editing, showDeleteConfirm, onClose])

  // A ticket nobody has started opens on what it asks for; one in flight or
  // done opens on what happened.
  useEffect(() => {
    if (task) setTab(task.state === 'backlog' ? 'details' : 'history')
  }, [task?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  const handleEditSave = async () => {
    setEditSaving(true)
    setEditError(null)
    try {
      const { performer, human_reason, ...text } = editForm
      const changes: Record<string, unknown> = withClears(text, ['description', 'acceptance_criteria', 'assigned_to'])
      // Who does it: only sent when it changed, so a ticket in progress can still be edited.
      if (performer !== (task!.performer ?? 'agent') || (performer === 'human' && human_reason !== task!.human_reason)) {
        changes.performer = performer
        if (performer === 'human') changes.human_reason = human_reason
      }
      await api.updateTask(task!.id, changes)
      setEditing(false)
      onRefresh?.()
      onClose()
    } catch (e) {
      setEditError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setEditSaving(false)
    }
  }

  const handleDelete = async () => {
    setDeleting(true)
    setDeleteError(null)
    try {
      await api.deleteTask(task!.id)
      setShowDeleteConfirm(false)
      onClose()
      onRefresh?.()
    } catch (e) {
      setDeleteError(e instanceof Error ? e.message : 'Delete failed')
    } finally {
      setDeleting(false)
    }
  }

  if (!task) return null
  const canWork = canWorkTask(task, meId, myAccess)
  // Editing the plan (text, dependencies, story) is wider than working it.
  const canPlan = canPlanTask(task, meId, myAccess)

  return (
    <>
      {/* Backdrop */}
      <div
        className="fixed inset-0 z-40"
        style={{ background: 'var(--scrim)' }}
        onClick={onClose}
      />

      {/* Drawer */}
      <div
        className="fixed right-0 top-0 h-full w-[520px] z-50 flex flex-col"
        style={{
          background: 'var(--surface)',
          borderLeft: '1px solid var(--line)',
          boxShadow: '-12px 0 32px color-mix(in srgb, var(--ink) 10%, transparent)',
        }}
      >
        {/* Header */}
        <div className="flex items-start justify-between gap-3 px-6 pt-5 pb-4 border-b border-line" style={{ boxShadow: 'inset 0 3px 0 var(--ink)' }}>
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2.5 mb-2.5 flex-wrap">
              <span className="amp-num text-[13px] text-ink-3">#{task.id}</span>
              <StatusBadge state={task.state} blockReason={task.block_reason} />
              <PriorityMark priority={task.priority} showLabel={task.priority === '2' || task.priority === '3'} />
            </div>
            <h2 className="text-[17px] font-semibold text-ink leading-snug m-0">{task.name}</h2>
          </div>
          <div className="flex items-center gap-0.5 flex-shrink-0">
            {canPlan && <button
              onClick={() => {
                setEditForm({
                  name: task.name,
                  description: task.description || '',
                  acceptance_criteria: task.acceptance_criteria || '',
                  assigned_to: task.assigned_to || '',
                  priority: task.priority || '1',
                  performer: task.performer ?? 'agent',
                  human_reason: task.human_reason ?? 'other',
                })
                setEditError(null)
                setEditing(true)
              }}
              className="amp-btn amp-btn-ghost amp-btn-sm"
              title="Edit task" aria-label="Edit task"
            >
              <Pencil size={13} className="text-ink-2" />
            </button>}
            <button
              onClick={() => { setDeleteError(null); setShowDeleteConfirm(true) }}
              className="amp-btn amp-btn-ghost amp-btn-sm"
              title="Delete task" aria-label="Delete task"
            >
              <Trash2 size={13} className="text-danger/70 hover:text-danger" />
            </button>
            <button onClick={onClose} className="amp-btn amp-btn-ghost amp-btn-sm" aria-label="Close">
              <X size={15} className="text-ink-2" />
            </button>
          </div>
        </div>

        {/* Meta strip */}
        <div className="flex items-center gap-3 px-6 py-2.5 border-b border-line bg-surface-2 flex-wrap amp-num">
          {isHuman(task) && (
            <div className="flex items-center gap-1.5 text-xs text-ink" title={reasonHint(task.human_reason)}>
              <UserRound size={11} />
              <span className="text-ink-3">Done by</span>
              <span className="font-medium">a person</span>
              <span className="text-ink-3">· {reasonLabel(task.human_reason).toLowerCase()}</span>
            </div>
          )}
          {!isHuman(task) && task.assigned_to && !task.agent_id && (
            <div className="flex items-center gap-1.5 text-xs text-link" title="Planned agent">
              <Bot size={11} />
              <span className="text-ink-3">Agent</span>
              <span className="font-medium">{task.assigned_to}</span>
              <span className="text-ink-3">(planned)</span>
            </div>
          )}
          {task.agent_id && !isHuman(task) && (
            <div className="flex items-center gap-1.5 text-xs text-success" title="Active agent">
              <Bot size={11} />
              <span className="text-ink-3">Agent</span>
              <span className="font-medium">{task.agent_id}</span>
            </div>
          )}
          {task.dispatched_at && (
            <div className="flex items-center gap-1.5 text-xs text-ink-2">
              <Clock size={11} />
              <span>Started {relativeTime(task.dispatched_at)}</span>
            </div>
          )}
          {task.start_at && (
            <div className="flex items-center gap-1.5 text-xs text-warning-ink" title={new Date(task.start_at).toLocaleString()}>
              <CalendarClock size={11} />
              <span>Starts {relativeTime(task.start_at)}</span>
            </div>
          )}
          {task.dependency_ids?.length > 0 && (
            <div className="flex items-center gap-1.5 text-xs text-ink-2">
              <GitBranch size={11} />
              <span>{task.dependency_ids.length} dep{task.dependency_ids.length > 1 ? 's' : ''}</span>
            </div>
          )}
          {task.blocked_by_ids?.length ? (
            <div className="flex items-center gap-1.5 text-xs text-danger-ink">
              <AlertCircle size={11} />
              <span>Waiting on #{task.blocked_by_ids.join(', #')}</span>
            </div>
          ) : null}
          {task.state === 'completed' && task.completed_at && (
            <div className="flex items-center gap-1.5 text-xs text-success">
              <CheckCircle2 size={11} />
              <span>Done {relativeTime(task.completed_at)}</span>
            </div>
          )}
          {mergeStatusText(task) && (
            <div className="flex items-center gap-1.5 text-xs" data-testid="merge-status"
              style={{ color: task.merge_state === 'merged' ? 'var(--success-ink)' : 'var(--teal)' }}>
              <GitMerge size={11} aria-hidden="true" />
              <span>{mergeStatusText(task)}</span>
            </div>
          )}
          <MergeGateNote task={task} />
        </div>

        {/* An agent's question, answered in place (the same HumanActions as My work) */}
        {displayState(task) === 'flag' && (
          <div className="px-6 py-4 border-b border-line" style={{ background: 'color-mix(in srgb, var(--st-flag) 6%, var(--surface))', boxShadow: 'inset 3px 0 0 var(--st-flag)' }}>
            <HumanActions key={`flag-${task.id}`} task={task} kind="answer" canAct={canWork}
              onChanged={t => { if (t) { if (onTaskUpdated) onTaskUpdated(t); else onRefresh?.() } else onRefresh?.(); api.getComments(task.id).then(setComments).catch(() => {}) }} />
          </div>
        )}

        {/* Done with an unmerged PR: Mark merged / Not needed (or Reopen when the PR was closed) */}
        {task.state === 'completed' && (task.merge_state === 'needed' || task.merge_state === 'abandoned') && (
          <MergePanel key={`merge-${task.id}`} task={task} canWork={canWork}
            onChanged={t => { if (t && onTaskUpdated) onTaskUpdated(t); else onRefresh?.() }} />
        )}

        {/* A person's own ticket: they start and finish it here */}
        {isHuman(task) && task.state !== 'completed' && displayState(task) !== 'flag' && (
          <HumanWork key={`human-${task.id}`} task={task} meId={meId} people={people}
            onChanged={t => { if (onTaskUpdated) onTaskUpdated(t); else onRefresh?.() }} />
        )}

        {/* Mockups, diagrams and documents published to this ticket */}
        <TicketArtifacts projectId={task.project_id} taskId={task.id} />

        {/* Owner */}
        <OwnerSection
          key={task.id}
          task={task}
          people={people}
          members={members}
          meId={meId}
          myAccess={myAccess}
          openReassign={openReassign}
          canWork={canWork}
          onUpdated={t => (onTaskUpdated ? onTaskUpdated(t) : onRefresh?.())}
        />

        {/* Tabs */}
        <div className="flex border-b border-line">
          {(['history', 'details'] as const).map(t => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`px-6 py-3 text-xs font-semibold capitalize transition-all border-b-2 -mb-px cursor-pointer ${
                tab === t
                  ? 'border-primary text-link'
                  : 'border-transparent text-ink-2 hover:text-ink'
              }`}
            >
              {t}
            </button>
          ))}
        </div>

        {/* Content */}
        <div className="flex-1 overflow-y-auto p-6">
          {editing && canPlan ? (
            <div className="space-y-4">
              <div>
                <label className="text-xs font-medium text-ink-2 block mb-1.5">Name</label>
                <input value={editForm.name} onChange={e => setEditForm(f => ({ ...f, name: e.target.value }))}
                  className={inputCls} />
              </div>
              <div>
                <label className="text-xs font-medium text-ink-2 block mb-1.5">Description</label>
                <textarea value={editForm.description} onChange={e => setEditForm(f => ({ ...f, description: e.target.value }))}
                  rows={4} className={`${inputCls} resize-none`} />
              </div>
              <div>
                <label className="text-xs font-medium text-ink-2 block mb-1.5">Acceptance Criteria</label>
                <textarea value={editForm.acceptance_criteria} onChange={e => setEditForm(f => ({ ...f, acceptance_criteria: e.target.value }))}
                  rows={3} className={`${inputCls} resize-none`} />
              </div>
              <div>
                <label htmlFor="edit-performer" className="text-xs font-medium text-ink-2 block mb-1.5">Done by</label>
                <select id="edit-performer" value={editForm.performer} disabled={task.state === 'in_progress'}
                  onChange={e => setEditForm(f => ({ ...f, performer: e.target.value as 'agent' | 'human' }))} className={inputCls}>
                  <option value="agent">An agent</option>
                  <option value="human">A person: its owner does it in AMP</option>
                </select>
                {task.state === 'in_progress' && <p className="text-[11.5px] text-ink-3 mt-1">Finish or stop the work before changing who does it.</p>}
              </div>
              {editForm.performer === 'human' ? (
                <div>
                  <label htmlFor="edit-reason" className="text-xs font-medium text-ink-2 block mb-1.5">Why a person</label>
                  <select id="edit-reason" value={editForm.human_reason}
                    onChange={e => setEditForm(f => ({ ...f, human_reason: e.target.value as HumanReason }))} className={inputCls}>
                    {HUMAN_REASONS.map(r => <option key={r.value} value={r.value}>{r.label}: {r.hint.toLowerCase()}</option>)}
                  </select>
                </div>
              ) : (
                <div>
                  <label className="text-xs font-medium text-ink-2 block mb-1.5">Agent</label>
                  <input value={editForm.assigned_to} onChange={e => setEditForm(f => ({ ...f, assigned_to: e.target.value }))}
                    className={inputCls} placeholder="amp-worker" />
                </div>
              )}
              <div>
                <label className="text-xs font-medium text-ink-2 block mb-1.5">Priority</label>
                <select value={editForm.priority} onChange={e => setEditForm(f => ({ ...f, priority: e.target.value }))}
                  className={inputCls}>
                  <option value="0">Low</option><option value="1">Normal</option><option value="2">High</option><option value="3">Critical</option>
                </select>
              </div>
              {editError && <p className="text-xs text-danger-ink">{editError}</p>}
              <div className="flex gap-2 pt-1">
                <button onClick={handleEditSave} disabled={editSaving}
                  className="amp-btn amp-btn-primary">
                  {editSaving && <Loader2 size={11} className="animate-spin" />}
                  Save changes
                </button>
                <button onClick={() => setEditing(false)}
                  className="px-4 py-2 text-xs font-medium text-ink-2 hover:text-ink hover:bg-surface-3 rounded-lg transition-colors cursor-pointer">
                  Cancel
                </button>
              </div>
            </div>
          ) : loading ? (
            <div className="flex justify-center py-10">
              <Loader2 size={20} className="text-ink-2 animate-spin" />
            </div>
          ) : tab === 'details' ? (
            <DetailsTab task={task} onClose={onClose} onRefresh={onRefresh} canWork={canWork} canPlan={canPlan}
              onUpdated={t => (onTaskUpdated ? onTaskUpdated(t) : onRefresh?.())} />
          ) : (
            <HistoryTab history={history} comments={comments} people={people} />
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
          error={deleteError}
        />
      )}
    </>
  )
}

function OwnerSection({ task, people, members, meId, myAccess, openReassign, canWork, onUpdated }: {
  task: Task
  people: Map<number, Person>
  members: ProjectMember[]
  meId?: number
  myAccess: ProjectAccess | null
  openReassign: boolean
  canWork: boolean
  onUpdated: (task: Task) => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const owner = lookupPerson(people, task.owner_user_id)
  const creator = lookupPerson(people, task.created_by)
  const isMine = meId != null && task.owner_user_id === meId
  const ownedByOther = !!owner && meId != null && !isMine
  // Taking over / reassigning someone else's ticket needs lead access or
  // above, or the project's open-reassign setting while nobody is working it.
  const canTakeOver = canTakeOverTask(task, myAccess, openReassign)
  const canAssign = canWork || canTakeOver

  const assign = async (user: string | number) => {
    setBusy(true)
    setError(null)
    try {
      onUpdated(await api.assignTask(task.id, user))
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Assignment failed')
    } finally {
      setBusy(false)
    }
  }


  return (
    <div className="px-6 py-3 border-b border-line-sub space-y-2">
      <div className="flex items-center gap-3 flex-wrap">
        <span className="text-xs font-semibold text-ink-3 uppercase tracking-widest">Owner</span>
        {owner ? (
          <div className="flex items-center gap-1.5 min-w-0">
            <OwnerAvatar person={owner} size={20} />
            <span className="text-sm text-ink font-medium truncate" title={owner.email}>{owner.name}</span>
            {isMine && <span className="text-xs text-ink-3">(you)</span>}
          </div>
        ) : (
          <UnassignedPill />
        )}
        <div className="flex items-center gap-1.5 ml-auto">
          {busy && <Loader2 size={12} className="text-ink-2 animate-spin" />}
          {!owner && meId != null && (
            <button
              onClick={() => assign('me')}
              disabled={busy}
              className="amp-btn amp-btn-primary amp-btn-sm"
              title="Take ownership of this ticket"
            >
              <Hand size={11} />
              Claim
            </button>
          )}
          {ownedByOther && canTakeOver && (
            <button
              onClick={() => assign('me')}
              disabled={busy}
              className="amp-btn amp-btn-primary amp-btn-sm"
              title={`Take this ticket over from ${owner!.name}`}
            >
              <Hand size={11} />
              Reassign to me
            </button>
          )}
          {owner && canAssign && (
            <button
              onClick={() => assign('none')}
              disabled={busy}
              className="amp-btn amp-btn-sm"
              title="Release back to the unassigned pool"
            >
              <UserMinus size={11} />
              Unassign
            </button>
          )}
          {members.length > 0 && canAssign && (
            <select
              value=""
              disabled={busy}
              onChange={e => { if (e.target.value) assign(e.target.value) }}
              className="bg-bg border border-line rounded-lg px-2 py-1 text-xs text-ink focus:outline-none focus:border-primary cursor-pointer disabled:opacity-50"
              title="Assign to a project member"
            >
              <option value="">Assign to…</option>
              {members.filter(m => m.user_id !== task.owner_user_id).map(m => (
                <option key={m.user_id} value={m.user_id}>
                  {personName(m)}{m.user_id === meId ? ' (me)' : ''}
                </option>
              ))}
            </select>
          )}
        </div>
      </div>
      <div className="text-xs text-ink-3">
        Created by{' '}
        {creator
          ? <span className="text-ink-2" title={creator.email}>{creator.name}{task.created_by === meId ? ' (you)' : ''}</span>
          : <span className="italic">unknown</span>}
      </div>
      <ApprovalLine task={task} people={people} meId={meId} />
      {!canWork && owner && (
        <p className="text-xs text-warning-ink">
          Owned by {owner.name} — {canTakeOver
            ? 'reassign it to yourself to work on it.'
            : openReassign && task.state === 'in_progress'
              ? "it's in progress, so only they or a team lead can reassign it."
              : 'ask them or a team lead to reassign it to work on it.'}
        </p>
      )}
      {error && <p className="text-xs text-danger-ink">{error}</p>}
    </div>
  )
}

/**
 * Who approved this ticket's plan (#2198), and for whom when it wasn't their
 * own: "Approved by Lee on behalf of Bob · Oct 22". An open ticket whose plan
 * nobody approved says so. Approval happens only in My work (KB
 * decisions/human-work-ux.md §2), so the drawer never approves: for the
 * person's own ticket it links there, "Awaiting your approval → My work".
 */
export function ApprovalLine({ task, people, meId }: { task: Task; people: Map<number, Person>; meId?: number }) {
  const a = approvalOf(task)
  const name = (id: number) => (lookupPerson(people, id)?.name ?? `User #${id}`) + (id === meId ? ' (you)' : '')
  if (!a) {
    if (task.state === 'completed') return null
    const owner = approvalOwner(task)
    return (
      <div className="text-xs text-ink-3 flex items-center gap-1.5 flex-wrap">
        <ShieldAlert size={12} className="text-warning-ink" aria-hidden="true" />
        <span>Plan not approved yet{task.approved_at ? ' (changed since it was approved)' : ''}.</span>
        {isYours(task, meId)
          ? <Link to={projectLink(task.project_id, 'my-work', task.id)} className="inline-flex items-center gap-1 font-semibold text-ink underline underline-offset-2">
              Awaiting your approval<ArrowRight size={11} aria-hidden="true" />My work</Link>
          : <span>Awaits {owner != null ? name(owner) : 'its owner'}'s approval, in their My work.</span>}
      </div>
    )
  }
  return (
    <div className="text-xs text-ink-3 flex items-center gap-1.5 flex-wrap">
      <ShieldCheck size={12} className="text-success-ink" aria-hidden="true" />
      <span>
        {a.approvedBy == null
          ? <>Approved by <span className="text-ink-2">the AMP upgrade</span> (kept from an earlier approval)</>
          : <>Approved by <span className="text-ink-2">{name(a.approvedBy)}</span>
              {a.onBehalfOf != null && <> on behalf of <span className="text-ink-2">{name(a.onBehalfOf)}</span></>}</>}
        {formatWhen(a.at) && <> · <span className="tabular-nums">{formatWhen(a.at)}</span></>}
      </span>
    </div>
  )
}

// Where a ticket sits in the plan: its story (movable) and the tickets it
// waits on (editable). Changes apply immediately; the server validates them
// (no cycles, same project) and moves the ticket between ready and blocked.
function TaskPlacement({ task, canEdit, onUpdated }: {
  task: Task; canEdit: boolean; onUpdated: (t: Task) => void
}) {
  const [tasks, setTasks] = useState<Task[]>([])
  const [epics, setEpics] = useState<Epic[]>([])
  const [stories, setStories] = useState<Story[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [warnings, setWarnings] = useState<string[]>([])

  useEffect(() => {
    let cancelled = false
    Promise.all([api.listTasks(task.project_id), api.listEpics(task.project_id)])
      .then(async ([ts, es]) => {
        const ss = (await Promise.all(es.map(e => api.listStories(e.id)))).flat()
        if (!cancelled) { setTasks(ts); setEpics(es); setStories(ss) }
      })
      .catch(() => {})
    return () => { cancelled = true }
  }, [task.project_id])

  const run = async (fn: () => Promise<{ task: Task; warnings?: string[] | null }>) => {
    setBusy(true); setError(null); setWarnings([])
    try {
      const r = await fn()
      setWarnings(r.warnings ?? [])
      // The board takes the updated ticket as-is (a full refresh would
      // reload the board and close this drawer's tab).
      onUpdated(r.task)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Update failed')
    } finally {
      setBusy(false)
    }
  }

  const story = stories.find(s => s.id === task.story_id)
  const waitingOn = new Set(task.blocked_by_ids ?? [])
  return (
    <div className="space-y-4">
      <div>
        <div className="text-xs font-semibold text-ink-3 uppercase tracking-widest mb-2">Story</div>
        {canEdit && stories.length > 0 ? (
          <select value={task.story_id} disabled={busy}
            onChange={e => {
              const id = Number(e.target.value)
              run(async () => ({ task: await api.moveTask(task.id, id) }))
            }}
            className="w-full bg-bg border border-line rounded-lg px-3 py-2 text-sm text-ink focus:outline-none focus:border-primary cursor-pointer disabled:opacity-50">
            {epics.map(e => (
              <optgroup key={e.id} label={e.name}>
                {stories.filter(s => s.epic_id === e.id).map(s => <option key={s.id} value={s.id}>{s.name}</option>)}
              </optgroup>
            ))}
          </select>
        ) : (
          <div className="text-sm text-ink">
            {epics.find(e => e.id === task.epic_id)?.name ?? 'Epic'} › {story?.name ?? `Story #${task.story_id}`}
          </div>
        )}
      </div>
      <div>
        <div className="flex items-center gap-2 mb-2">
          <div className="text-xs font-semibold text-ink-3 uppercase tracking-widest">Depends on</div>
          {busy && <Loader2 size={11} className="text-ink-2 animate-spin" />}
          {waitingOn.size > 0 && <span className="text-[11px] text-danger-ink">waiting on {waitingOn.size}</span>}
        </div>
        <DependencyPicker tasks={tasks} selfId={task.id} value={task.dependency_ids ?? []} disabled={!canEdit || busy}
          onChange={ids => run(() => api.setTaskDependencies(task.id, ids))} />
        {error && <p className="text-xs text-danger-ink mt-1.5">{error}</p>}
        {warnings.map((w, i) => <p key={i} className="text-xs text-warning-ink mt-1.5">{w}</p>)}
      </div>
    </div>
  )
}

// A ticket's metadata (e.g. its jira_key), with what it inherits from its
// project, epic, and story.
function TaskMetadata({ task, canEdit, onSaved }: { task: Task; canEdit: boolean; onSaved?: () => void }) {
  const [inherited, setInherited] = useState<Metadata | undefined>()
  const [initial, setInitial] = useState<Metadata>(task.metadata ?? {})
  const [draft, setDraft] = useState<{ value: Metadata; error: string | null }>({ value: task.metadata ?? {}, error: null })
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const patch = metadataPatch(initial, draft.value)

  useEffect(() => {
    api.inheritedMetadata('task', task.id).then(setInherited).catch(() => setInherited(undefined))
  }, [task.id])

  const save = async () => {
    if (draft.error || !patch) return
    setSaving(true)
    setError(null)
    try {
      await api.updateTask(task.id, { metadata: patch })
      setInitial(draft.value)
      onSaved?.()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-2">
        <div className="text-xs font-semibold text-ink-3 uppercase tracking-widest">Metadata</div>
        {canEdit && patch && (
          <button onClick={save} disabled={saving || !!draft.error}
            className="text-xs text-link hover:text-primary flex items-center gap-1 transition-colors cursor-pointer disabled:opacity-50">
            {saving ? <Loader2 size={10} className="animate-spin" /> : <Check size={10} />}Save
          </button>
        )}
      </div>
      <MetadataEditor initial={task.metadata} inherited={inherited} readOnly={!canEdit}
        onChange={(value, error) => setDraft({ value, error })} />
      {error && <p className="text-xs text-danger-ink mt-1">{error}</p>}
    </div>
  )
}

function DetailsTab({ task, onClose, onRefresh, canWork, canPlan, onUpdated }: {
  task: Task; onClose: () => void; onRefresh?: () => void; canWork: boolean; canPlan: boolean; onUpdated: (t: Task) => void
}) {
  const [editingSchedule, setEditingSchedule] = useState(false)
  const [scheduleValue, setScheduleValue] = useState('')
  const [savingSchedule, setSavingSchedule] = useState(false)

  const inputCls =
    'w-full bg-bg border border-line rounded-lg px-3 py-2 text-sm text-ink ' +
    'focus:outline-none focus:border-primary focus:ring-2 focus:ring-primary/20 transition-all font-[inherit]'

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
          <div className="amp-label mb-2">Description</div>
          <div className="bg-surface-2 p-4 border border-line">
            <Markdown size="compact">{task.description}</Markdown>
          </div>
        </div>
      )}
      {task.acceptance_criteria && (
        <div>
          <div className="amp-label mb-2">Acceptance criteria</div>
          <div className="bg-surface-2 p-4 border border-line">
            <Markdown size="compact">{task.acceptance_criteria}</Markdown>
          </div>
        </div>
      )}
      <RunsPanel taskId={task.id} refreshKey={task.state} />
      <div className="grid grid-cols-2 gap-2.5">
        <MetaItem label="Task ID" value={`#${task.id}`} />
        <MetaItem label="State" value={task.state} />
        <MetaItem label="Created" value={relativeTime(task.created_at)} />
        <MetaItem label="Updated" value={relativeTime(task.updated_at)} />
        {task.dependency_ids?.length > 0 && (
          <MetaItem label="Depends on" value={task.dependency_ids.map(id => `#${id}`).join(', ')} />
        )}
      </div>

      <TaskPlacement key={`p-${task.id}`} task={task} canEdit={canPlan} onUpdated={onUpdated} />

      <TaskMetadata key={task.id} task={task} canEdit={canPlan} onSaved={onRefresh} />

      {/* Scheduled Start */}
      <div>
        <div className="flex items-center justify-between mb-2">
          <div className="text-xs font-semibold text-ink-3 uppercase tracking-widest">Scheduled Start</div>
          {canWork && <button
            onClick={() => { setScheduleValue(task.start_at ? task.start_at.slice(0, 16) : ''); setEditingSchedule(true) }}
            className="text-xs text-link hover:text-primary flex items-center gap-1 transition-colors cursor-pointer"
          >
            <Pencil size={10} />
            {task.start_at ? 'Edit' : 'Set schedule'}
          </button>}
        </div>
        {!editingSchedule || !canWork ? (
          task.start_at ? (
            <div className="text-sm text-ink bg-bg rounded-xl p-4 border border-line-sub flex items-center gap-2.5">
              <CalendarClock size={14} className="text-warning-ink flex-shrink-0" />
              <div>
                <div className="font-medium">{new Date(task.start_at).toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</div>
                <div className="text-xs text-ink-2 mt-0.5">{relativeTime(task.start_at)}</div>
              </div>
            </div>
          ) : (
            <div className="text-sm text-ink-3 italic">No schedule set</div>
          )
        ) : (
          <div className="bg-bg rounded-xl p-4 border border-line space-y-3">
            <div>
              <label className="text-xs font-medium text-ink-2 block mb-1.5">Date & Time (local timezone)</label>
              <input type="datetime-local" value={scheduleValue} onChange={e => setScheduleValue(e.target.value)}
                className={inputCls} />
            </div>
            <div className="flex items-center gap-2">
              <button onClick={handleSaveSchedule} disabled={savingSchedule}
                className="amp-btn amp-btn-primary">
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
                  className="px-3 py-1.5 text-xs text-danger-ink hover:text-danger transition-colors cursor-pointer"
                >
                  Clear schedule
                </button>
              )}
              <button onClick={() => setEditingSchedule(false)}
                className="px-3 py-1.5 text-xs text-ink-2 hover:text-ink transition-colors cursor-pointer">
                Cancel
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

function HistoryTab({ history, comments, people }: { history: ActivityLog[]; comments: Comment[]; people: Map<number, Person> }) {
  if (history.length === 0 && comments.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-12 gap-2">
        <div className="w-8 h-8 rounded-full bg-surface-2 flex items-center justify-center">
          <Clock size={14} className="text-ink-3" />
        </div>
        <p className="text-sm text-ink-3">No activity yet</p>
      </div>
    )
  }

  return (
    <div className="space-y-0">
      {history.map((entry, i) => {
        // A manager passing on its person's answer over MCP: shown apart from the person's own words.
        const relayed = isRelayed(entry.actor)
        return (
        <div key={entry.id} className="flex gap-3">
          <div className="flex flex-col items-center flex-shrink-0">
            <div className="mt-1.5">
              <span className={`block w-2 h-2 rounded-full ${ACTION_DOT[entry.action] ?? ACTION_DOT.comment}`} />
            </div>
            {i < history.length - 1 && <div className="w-px flex-1 bg-line-sub mt-1.5 mb-0" />}
          </div>
          <div className="pb-5 flex-1 min-w-0">
            <div className="flex items-center gap-1.5 mb-1 flex-wrap">
              <span className="text-xs font-semibold text-ink">{relayed ? (lookupPerson(people, entry.actor_user_id)?.name ?? relayedFor(entry.actor)) : activityActor(entry, people)}</span>
              {relayed && (
                <span className="inline-flex items-center gap-1 font-label text-[9.5px] font-semibold uppercase tracking-[0.1em] px-1 py-[2px] border border-dashed border-line-str text-ink-2"
                  title="An agent passed this on for the person over MCP. They didn't type it in AMP themselves.">
                  <Bot size={10} aria-hidden="true" />Relayed by agent</span>
              )}
              <span className="text-ink-3 text-xs">·</span>
              <span className="text-xs text-ink-2 capitalize">{entry.action.replace('_', ' ')}</span>
              {entry.from_state && entry.to_state && (
                <span className="text-xs text-ink-3">
                  {entry.from_state} → {entry.to_state}
                </span>
              )}
              <span className="text-xs text-ink-3 ml-auto flex-shrink-0 tabular-nums">{relativeTime(entry.created_at)}</span>
            </div>
            {entry.detail && (
              <div className={`text-xs rounded-xl px-3.5 py-2.5 leading-relaxed whitespace-pre-wrap ${
                entry.action === 'comment'
                  ? `bg-bg border text-ink ${relayed ? 'border-dashed border-line-str' : 'border-line-sub'}`
                  : 'text-ink-2'
              }`}>
                {entry.detail}
              </div>
            )}
          </div>
        </div>
        )
      })}
    </div>
  )
}

function MetaItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-bg rounded-xl p-3 border border-line-sub">
      <div className="text-xs text-ink-3 font-medium mb-0.5">{label}</div>
      <div className="text-sm text-ink font-semibold truncate">{value}</div>
    </div>
  )
}

/** A human ticket's panel: what kind of human work it is, who does it, and
 *  for its owner the same Start and Mark done… as My work (HumanActions). */
function HumanWork({ task, meId, people, onChanged }: {
  task: Task; meId?: number; people: Map<number, Person>; onChanged: (t: Task) => void
}) {
  const owner = task.owner_user_id != null ? lookupPerson(people, task.owner_user_id) : null
  // Its owner does it; an unassigned one is claimed by whoever starts it. Local dev has no ownership.
  const mine = meId === undefined || task.owner_user_id == null || task.owner_user_id === meId
  return (
    <div className="px-6 py-4 border-b border-line grid gap-2.5" style={{ background: 'var(--surface-2)', boxShadow: 'inset 3px 0 0 var(--ink)' }}>
      <span className="amp-label flex items-center gap-1.5"><UserRound size={12} />
        {owner ? `${owner.name} does this` : 'A person does this'} · {reasonLabel(task.human_reason)}</span>
      <p className="m-0 text-[13px] leading-relaxed text-ink-2">
        {reasonHint(task.human_reason)}. Agents never pick it up, and they can't mark it done: only {owner ? owner.name : 'its owner'} can, here in AMP. The work that depends on it starts then.
      </p>
      {!mine && task.state !== 'blocked'
        ? <span className="text-[12.5px] text-ink-2">Only {owner ? owner.name : 'its owner'} starts and finishes it.</span>
        : <HumanActions task={task} kind="do" canAct={mine} onChanged={t => { if (t) onChanged(t) }} />}
      {task.state === 'in_progress' && task.dispatched_at && (
        <span className="text-[12px] text-ink-3">Started {relativeTime(task.dispatched_at)}</span>
      )}
    </div>
  )
}

/** A done ticket waiting on its merge: the PR from its latest run, Mark merged / Not needed, or Reopen. */
function MergePanel({ task, canWork, onChanged }: { task: Task; canWork: boolean; onChanged: (t: Task | null) => void }) {
  const [pr, setPr] = useState<{ url?: string; branch?: string }>({})
  useEffect(() => {
    let live = true
    api.taskRuns(task.id).then(({ runs }) => {
      const r = [...runs].reverse().find(x => x.pr_url || x.branch)
      if (live && r) setPr({ url: r.pr_url, branch: r.branch })
    }).catch(() => {})
    return () => { live = false }
  }, [task.id])
  return (
    <div className="px-6 py-4 border-b border-line grid gap-2" style={{ background: 'color-mix(in srgb, var(--teal) 6%, var(--surface))', boxShadow: 'inset 3px 0 0 var(--teal)' }}>
      <span className="amp-label" style={{ color: 'var(--teal)' }}>Merge</span>
      <HumanActions task={task} kind="merge" canAct={canWork} prUrl={pr.url} branch={pr.branch} onChanged={onChanged} />
    </div>
  )
}

// The merge gate's two exceptions, read from the effective metadata (#2597):
// a done ticket whose run went to the integration branch (its dependents
// don't wait; the merge to main comes later), and a ticket that starts when its
// dependencies are done without waiting for their merge (waits_for_merge: false).
function MergeGateNote({ task }: { task: Task }) {
  const [inherited, setInherited] = useState<Metadata | undefined>()
  const [runs, setRuns] = useState<Run[]>([])
  const meta = useMemo(() => effectiveMetadata(inherited, task.metadata), [inherited, task.metadata])
  const branch = integrationBranch(meta)
  const wantRuns = task.state === 'completed' && !!branch

  useEffect(() => {
    let live = true
    api.inheritedMetadata('task', task.id).then(m => { if (live) setInherited(m) }).catch(() => {})
    return () => { live = false }
  }, [task.id])

  useEffect(() => {
    if (!wantRuns) return
    let live = true
    api.taskRuns(task.id).then(r => { if (live) setRuns(r.runs) }).catch(() => {})
    return () => { live = false }
  }, [task.id, wantRuns, task.merge_state])

  // Runs are only read while they matter; otherwise ignore any stale ones
  // (derived here rather than reset with setState inside the effect).
  const into = mergedIntoIntegration(task, meta, wantRuns ? runs : [])
  const noWait = !waitsForMerge(meta) && task.state !== 'completed' && task.dependency_ids.length > 0
  if (!into && !noWait) return null
  return (
    <>
      {into && (
        <div className="flex items-center gap-1.5 text-xs text-ink-2" data-testid="merge-integration"
          title="Tickets that depend on it don't wait for this merge.">
          <GitMerge size={11} aria-hidden="true" />
          <span>Merged into {into}, waiting on the merge to main</span>
        </div>
      )}
      {noWait && (
        <div className="flex items-center gap-1.5 text-xs text-ink-2" data-testid="merge-no-wait">
          <GitBranch size={11} aria-hidden="true" />
          <span>Starts when its dependencies are done; doesn't wait for their merge</span>
        </div>
      )}
    </>
  )
}
