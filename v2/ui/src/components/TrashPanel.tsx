import { useEffect, useState } from 'react'
import { Loader2, RotateCcw } from 'lucide-react'
import { api } from '../api/client'
import type { DeletedTask } from '../types'

/** Tickets deleted in the last 30 days, restorable with their comments, history, and links. */
export function TrashPanel({ projectId }: { projectId: number }) {
  const [items, setItems] = useState<DeletedTask[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<string | null>(null)
  const load = () => api.projectTrash(projectId).then(setItems).catch(() => setItems([]))
  useEffect(() => { load() }, [projectId]) // eslint-disable-line react-hooks/exhaustive-deps

  const restore = async (ids: number[]) => {
    setBusy(true); setMsg(null)
    try {
      const r = await api.restoreTasks(projectId, ids)
      const ok = r.results.filter(x => x.restored).length
      const failed = r.results.filter(x => x.error)
      setMsg(`${ok} restored${failed.length ? ` · ${failed.length} not: ${failed.map(f => `#${f.task_id} ${f.error}`).join('; ')}` : ''}`)
      await load()
    } catch (e) {
      setMsg(e instanceof Error ? e.message : 'Restore failed')
    } finally { setBusy(false) }
  }

  if (!items) return <Loader2 size={14} className="animate-spin text-ink-3" />
  if (items.length === 0) return <p className="m-0 text-[13px] text-ink-2">Nothing deleted in the last 30 days.{msg ? ` ${msg}` : ''}</p>
  const restorable = items.filter(i => i.story_exists).map(i => i.task_id)
  return (
    <div className="grid gap-3">
      <div className="flex items-center gap-3 flex-wrap">
        <span className="text-[13px] text-ink-2">{items.length} deleted ticket{items.length === 1 ? '' : 's'}</span>
        {restorable.length > 1 && <button type="button" className="amp-btn amp-btn-sm" disabled={busy} onClick={() => restore(restorable)}><RotateCcw size={11} />Restore all {restorable.length}</button>}
        {msg && <span className="text-[12.5px] text-ink-2">{msg}</span>}
      </div>
      <div className="border border-line max-h-[320px] overflow-auto">
        {items.map(d => (
          <div key={d.task_id} className="grid grid-cols-[auto_1fr_auto] gap-3 items-center px-3 py-2 border-t border-line first:border-t-0 text-[12.5px]">
            <span className="amp-num text-ink-3">#{d.task_id}</span>
            <span className="min-w-0 truncate" title={d.name}>{d.name}
              <span className="amp-num text-[11px] text-ink-3"> · {new Date(d.deleted_at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })}</span>
              {!d.story_exists && <span className="text-[11px] text-danger-ink"> · its story was deleted too</span>}</span>
            <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" disabled={busy || !d.story_exists} onClick={() => restore([d.task_id])}>Restore</button>
          </div>
        ))}
      </div>
    </div>
  )
}
