import { useState } from 'react'
import { ArrowRightLeft } from 'lucide-react'
import { api } from '../api/client'

/**
 * Hand a person's open tickets to someone else (they're out, leaving, or
 * scope moved). Shown on the Board when filtering by one person. In-progress
 * tickets stay with their owner: their agent is running.
 */
export function HandoverBar({ projectId, from, fromName, taskIds, narrowed, people, onDone }: {
  projectId: number; from: number; fromName: string
  /** the open tickets on screen (the board's other filters apply) */
  taskIds: number[]; narrowed: boolean
  people: { id: number; name: string }[]; onDone: () => void
}) {
  const [open, setOpen] = useState(false)
  const [to, setTo] = useState<number | ''>('')
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<string | null>(null)
  // Keep the result in view after the last ticket moves off this filter.
  if (taskIds.length === 0) return msg ? <p className="m-0 mt-2.5 text-[12.5px] text-ink-2">{msg}</p> : null
  const what = `${narrowed ? 'these ' : ''}${taskIds.length} open ticket${taskIds.length === 1 ? '' : 's'} of ${fromName}'s`
  const go = async () => {
    if (to === '') return
    setBusy(true); setMsg(null)
    try {
      const r = await api.handover(projectId, { from: String(from), to: String(to), task_ids: taskIds, reason })
      const moved = r.results.filter(x => x.moved).length
      const kept = r.results.filter(x => !x.moved)
      setMsg(`${moved} handed over${kept.length ? ` · ${kept.length} kept: ${kept.map(k => `#${k.task_id} (${k.error})`).join('; ')}` : ''}`)
      setOpen(false); onDone()
    } catch (e) {
      setMsg(e instanceof Error ? e.message : 'Handover failed')
    } finally { setBusy(false) }
  }
  return (
    <div className="mt-2.5 flex items-center gap-2 flex-wrap text-[12.5px] text-ink-2">
      {!open ? (
        <button type="button" className="amp-btn amp-btn-sm" onClick={() => setOpen(true)}><ArrowRightLeft size={11} />Hand over {what}</button>
      ) : (
        <>
          <span>Hand {what} to</span>
          <select className="amp-input !w-auto !py-1 !text-[12px]" value={to} onChange={e => setTo(e.target.value ? Number(e.target.value) : '')} aria-label="Hand over to">
            <option value="">choose…</option>
            {people.filter(p => p.id !== from).map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
          <input className="amp-input !w-[260px] !py-1 !text-[12px]" placeholder="why (posted on each ticket)" value={reason} onChange={e => setReason(e.target.value)} />
          <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" disabled={busy || to === ''} onClick={go}>Hand over</button>
          <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => setOpen(false)}>Cancel</button>
        </>
      )}
      {msg && <span>{msg}</span>}
    </div>
  )
}
