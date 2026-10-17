import { useEffect, useState } from 'react'
import { Loader2, RotateCcw } from 'lucide-react'
import { api } from '../api/client'
import type { DeletedGroup, DeletedTask, ProjectTrash } from '../types'

const when = (at: string) =>
  new Date(at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })

/**
 * Tickets, stories and epics deleted in the last 30 days, restorable with
 * their comments, history, and links. A deleted epic or story is listed once,
 * with the tickets that went with it.
 */
export function TrashPanel({ projectId }: { projectId: number }) {
  const [trash, setTrash] = useState<ProjectTrash | null>(null)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<string | null>(null)
  const load = () => api.projectTrash(projectId).then(setTrash).catch(() => setTrash({ tickets: [], groups: [] }))
  useEffect(() => { load() }, [projectId]) // eslint-disable-line react-hooks/exhaustive-deps

  const restore = async (taskIds: number[], epicIds: number[] = [], storyIds: number[] = []) => {
    setBusy(true); setMsg(null)
    try {
      const r = await api.restoreTasks(projectId, taskIds, { epicIds, storyIds })
      const groups = r.groups ?? []
      const tickets = [...r.results, ...groups.flatMap(g => g.tickets ?? [])]
      const failed = [
        ...groups.filter(g => g.error).map(g => `${g.kind} #${g.id} ${g.error}`),
        ...tickets.filter(t => t.error).map(t => `#${t.task_id} ${t.error}`),
      ]
      const parts = [
        groups.some(g => g.restored) && `${groups.filter(g => g.restored).length} epic/story`,
        `${tickets.filter(t => t.restored).length} ticket${tickets.length === 1 ? '' : 's'} restored`,
      ].filter(Boolean)
      setMsg(`${parts.join(' · ')}${failed.length ? ` · not restored: ${failed.join('; ')}` : ''}`)
      await load()
    } catch (e) {
      setMsg(e instanceof Error ? e.message : 'Restore failed')
    } finally { setBusy(false) }
  }

  if (!trash) return <Loader2 size={14} className="animate-spin text-ink-3" />
  const { groups } = trash
  // Tickets that went with a deleted epic or story are listed under it.
  const inGroup = new Set(groups.flatMap(g => g.task_ids))
  const loose = trash.tickets.filter(t => !inGroup.has(t.task_id))
  const deletedEpics = new Set(groups.filter(g => g.kind === 'epic').map(g => g.id))
  // A story can come back once its epic exists, or with it.
  const storyOK = (g: DeletedGroup) => g.epic_exists || deletedEpics.has(g.epic_id)
  const ticketOK = (t: DeletedTask) => t.story_exists || groups.some(g => g.kind === 'story' && g.id === t.story_id)

  if (groups.length === 0 && loose.length === 0) {
    return <p className="m-0 text-[13px] text-ink-2">Nothing deleted in the last 30 days.{msg ? ` ${msg}` : ''}</p>
  }
  const all = {
    epics: groups.filter(g => g.kind === 'epic').map(g => g.id),
    stories: groups.filter(g => g.kind === 'story' && storyOK(g)).map(g => g.id),
    tickets: loose.filter(ticketOK).map(t => t.task_id),
  }
  const total = all.epics.length + all.stories.length + all.tickets.length
  const counts = [
    groups.length > 0 && `${groups.length} epic${groups.length === 1 ? '' : 's'} / stor${groups.length === 1 ? 'y' : 'ies'}`,
    loose.length > 0 && `${loose.length} ticket${loose.length === 1 ? '' : 's'}`,
  ].filter(Boolean).join(' · ')

  return (
    <div className="grid gap-3">
      <div className="flex items-center gap-3 flex-wrap">
        <span className="text-[13px] text-ink-2">Deleted: {counts}</span>
        {total > 1 && (
          <button type="button" className="amp-btn amp-btn-sm" disabled={busy}
            onClick={() => restore(all.tickets, all.epics, all.stories)}>
            <RotateCcw size={11} />Restore all
          </button>
        )}
        {msg && <span className="text-[12.5px] text-ink-2">{msg}</span>}
      </div>
      <div className="border border-line max-h-[360px] overflow-auto">
        {groups.map(g => {
          const ok = g.kind === 'epic' || storyOK(g)
          return (
            <div key={`${g.kind}-${g.id}`} className="grid grid-cols-[auto_1fr_auto] gap-3 items-center px-3 py-2 border-t border-line first:border-t-0 text-[12.5px]">
              <span className="text-[10.5px] font-semibold uppercase tracking-wider text-ink-3 w-10">{g.kind}</span>
              <span className="min-w-0 truncate" title={g.name}>{g.name}
                <span className="amp-num text-[11px] text-ink-3">
                  {' · '}{g.kind === 'epic' ? `${g.stories} stor${g.stories === 1 ? 'y' : 'ies'}, ` : ''}
                  {g.task_ids.length} ticket{g.task_ids.length === 1 ? '' : 's'} · {when(g.deleted_at)}
                </span>
                {!ok && <span className="text-[11px] text-danger-ink"> · its epic was deleted and isn't in the trash</span>}</span>
              <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" disabled={busy || !ok}
                onClick={() => g.kind === 'epic'
                  ? restore([], [g.id])
                  : restore([], deletedEpics.has(g.epic_id) && !g.epic_exists ? [g.epic_id] : [], [g.id])}>
                Restore
              </button>
            </div>
          )
        })}
        {loose.map(d => (
          <div key={d.task_id} className="grid grid-cols-[auto_1fr_auto] gap-3 items-center px-3 py-2 border-t border-line first:border-t-0 text-[12.5px]">
            <span className="amp-num text-ink-3 w-10">#{d.task_id}</span>
            <span className="min-w-0 truncate" title={d.name}>{d.name}
              <span className="amp-num text-[11px] text-ink-3"> · {when(d.deleted_at)}</span>
              {!ticketOK(d) && <span className="text-[11px] text-danger-ink"> · its story was deleted and isn't in the trash</span>}</span>
            <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" disabled={busy || !ticketOK(d)}
              onClick={() => restore([d.task_id], [], groups.some(g => g.kind === 'story' && g.id === d.story_id) && !d.story_exists ? [d.story_id] : [])}>
              Restore
            </button>
          </div>
        ))}
      </div>
    </div>
  )
}
