import { useEffect, useState } from 'react'
import { Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { Panel } from './ui/PageHeader'
import { Avatar } from './ui/Avatar'
import { Markdown } from './ui/Markdown'
import type { DayReport } from '../types'

const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone
const ymd = (d: Date) => d.toLocaleDateString('en-CA', { timeZone: TZ })

type Row = { user_id: number; email: string; name: string; report?: DayReport }

/**
 * The project's standup sheet: every person on the project and the day report
 * they saved (this project's tickets only), for yesterday by default — and who
 * hasn't written one.
 */
export function StandupSheet({ projectId }: { projectId: number }) {
  const [date, setDate] = useState(() => ymd(new Date(Date.now() - 86_400_000)))
  // Rows for the date they were loaded for, so changing the date shows a spinner.
  const [loaded, setLoaded] = useState<{ date: string; rows: Row[] } | null>(null)
  useEffect(() => {
    api.projectDayReports(projectId, date, TZ).then(r => setLoaded({ date, rows: r.people })).catch(() => setLoaded({ date, rows: [] }))
  }, [projectId, date])
  const rows = loaded?.date === date ? loaded.rows : null
  const missing = rows?.filter(r => !r.report) ?? []
  return (
    <Panel label="Day reports" title="What each person said" right={
      <span className="inline-flex items-center gap-2">
        <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => setDate(ymd(new Date(Date.now() - 86_400_000)))}>Yesterday</button>
        <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => setDate(ymd(new Date()))}>Today</button>
        <input type="date" value={date} onChange={e => setDate(e.target.value)} className="amp-input !w-auto !py-1 !text-[12px]" aria-label="Day" />
      </span>}>
      {!rows ? <div className="px-4 py-4"><Loader2 size={14} className="animate-spin text-ink-3" /></div> : rows.length === 0 ? (
        <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Nobody on this project yet.</p>
      ) : (
        <div>
          {rows.filter(r => r.report).map(r => {
            const st = r.report!.data.stats
            return (
              <div key={r.user_id} className="grid grid-cols-[auto_1fr] gap-x-3 px-4 py-3 border-t border-line first:border-t-0">
                <Avatar name={r.name || r.email} size={24} />
                <div className="min-w-0 grid gap-1.5">
                  <div className="flex items-baseline gap-3 flex-wrap">
                    <b className="text-[13px] font-semibold text-ink">{r.name || r.email}</b>
                    <span className="amp-num text-[11px] text-ink-3">{st.completed} done · {st.in_progress} in progress · {st.blocked} blocked{st.created ? ` · ${st.created} created` : ''}</span>
                  </div>
                  {r.report!.summary
                    ? <Markdown size="compact">{r.report!.summary}</Markdown>
                    : <span className="text-[12.5px] text-ink-3">Saved without a summary.</span>}
                </div>
              </div>
            )
          })}
          {missing.length > 0 && (
            <p className="m-0 px-4 py-3 border-t border-line text-[12.5px] text-ink-2">
              No report for {date}: {missing.map(m => m.name || m.email).join(', ')}.
            </p>
          )}
        </div>
      )}
    </Panel>
  )
}
