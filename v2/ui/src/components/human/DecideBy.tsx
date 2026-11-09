/**
 * DecideBy: a person's ticket's decide-by date (#146) on its decision card.
 * It reads "Decide by Thu, Nov 12, 5:00 PM EST · in 2d", in red once the date
 * has passed ("2d overdue"). Its person sets, changes or removes the date
 * here; an agent may have suggested it. The date is the viewer's wall-clock
 * time, stored as an instant, so it reads right in every time zone.
 */
import { useState } from 'react'
import { CalendarClock, Check, Loader2, X } from 'lucide-react'
import { api } from '../../api/client'
import { describeDecideBy, fromLocalInput, toLocalInput } from '../../lib/decideBy'
import type { Task } from '../../types'

export interface DecideByProps {
  task: Task
  /** the person may change it */
  canEdit: boolean
  onChanged?: (t: Task) => void
  /** now (tests) */
  now?: () => number
}

export function DecideBy({ task, canEdit, onChanged, now = Date.now }: DecideByProps) {
  const [value, setValue] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const view = describeDecideBy(task.decide_by, now())
  const inputId = `decide-by-${task.id}`

  const save = async (clear: boolean) => {
    let body: { decide_by?: string; clear?: string[] }
    if (clear) {
      body = { clear: ['decide_by'] }
    } else {
      const at = fromLocalInput(value ?? '')
      if (!at) { setError('Pick a date and time'); return }
      body = { decide_by: at }
    }
    setBusy(true); setError(null)
    try {
      const updated = await api.updateTask(task.id, body)
      setValue(null)
      onChanged?.(updated)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'That did not work')
    } finally {
      setBusy(false)
    }
  }

  if (value !== null) {
    return (
      <form className="grid gap-1.5" data-testid="decide-by-editor" onSubmit={e => { e.preventDefault(); save(false) }}>
        <label className="amp-label" htmlFor={inputId}>Decide by</label>
        <div className="flex gap-2 items-center flex-wrap">
          <input id={inputId} type="datetime-local" className="amp-input" value={value}
            onChange={e => { setValue(e.target.value); setError(null) }} />
          <button type="submit" className="amp-btn amp-btn-primary amp-btn-sm" disabled={busy}>
            {busy ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Check size={12} aria-hidden="true" />}Save</button>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={busy} onClick={() => { setValue(null); setError(null) }}>
            <X size={12} aria-hidden="true" />Cancel</button>
        </div>
        <span className="text-[12px] text-ink-3">Your time zone. My work flags it once the date passes.</span>
        {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
      </form>
    )
  }

  if (!view && !canEdit) return null
  return (
    <div className="flex gap-2 items-center flex-wrap text-[12.5px]" data-testid="decide-by">
      <CalendarClock size={12} aria-hidden="true" className={view?.overdue ? 'text-danger-ink' : 'text-ink-3'} />
      {view ? (
        <span className={view.overdue ? 'text-danger-ink font-medium' : 'text-ink-2'} data-overdue={view.overdue || undefined}>
          Decide by <time dateTime={task.decide_by}>{view.absolute}</time> · {view.relative}
        </span>
      ) : <span className="text-ink-3">No decide-by date</span>}
      {canEdit && <>
        <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={busy}
          onClick={() => setValue(toLocalInput(task.decide_by) || toLocalInput(new Date(now() + 2 * 86400000).toISOString()))}>
          {view ? 'Change' : 'Set a date'}</button>
        {view && (
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={busy} onClick={() => save(true)}>Remove</button>
        )}
      </>}
      {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
    </div>
  )
}
