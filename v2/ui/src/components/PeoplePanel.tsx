import { useMemo } from 'react'
import { Panel } from './ui/PageHeader'
import { Avatar } from './ui/Avatar'
import { peopleRows } from '../lib/standup'
import type { Person } from '../lib/people'
import type { ActivityLog, Task } from '../types'

function ago(ms: number): string {
  if (!ms) return '—'
  const m = Math.round((Date.now() - ms) / 60_000)
  return m < 60 ? `${m}m ago` : m < 48 * 60 ? `${Math.round(m / 60)}h ago` : `${Math.round(m / 1440)}d ago`
}

export function PeoplePanel({ tasks, activity, people, since, rangeLabel, onOpen }: {
  tasks: Task[]; activity: ActivityLog[]; people: Map<number, Person>; since: string; rangeLabel: string; onOpen?: (id: number) => void
}) {
  const rows = useMemo(() => peopleRows(tasks, activity, people, since), [tasks, activity, people, since])
  const link = (t: Task) => onOpen
    ? <button key={t.id} type="button" onClick={() => onOpen(t.id)} className="block max-w-full truncate text-left hover:underline cursor-pointer">#{t.id} {t.name}</button>
    : <span key={t.id} className="block truncate">#{t.id} {t.name}</span>
  return (
    <Panel label="Standup" title="Who's doing what" right={<span>done in {rangeLabel.toLowerCase()} · now · next</span>}>
      {rows.length === 0 ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Nobody owns work on this project yet.</p> : (
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-[13px]">
            <thead><tr className="bg-surface-2">
              {['Person', 'Done', 'Now', 'Waiting on them', 'Ready next', 'Last activity'].map(h =>
                <th key={h} className="amp-label text-left px-3 py-2.5 border-b border-line whitespace-nowrap">{h}</th>)}
            </tr></thead>
            <tbody>
              {rows.map(r => (
                <tr key={r.id ?? 'none'} className="align-top">
                  <td className="px-3 py-2.5 border-b border-line whitespace-nowrap"><span className="inline-flex items-center gap-2"><Avatar name={r.name} size={20} />{r.name}</span></td>
                  <td className="px-3 py-2.5 border-b border-line max-w-[260px] text-ink-2">
                    <span className="amp-num text-ink">{r.done.length}</span>
                    {r.done.slice(0, 2).map(link)}{r.done.length > 2 && <span className="text-ink-3 text-[12px]">+{r.done.length - 2} more</span>}
                  </td>
                  <td className="px-3 py-2.5 border-b border-line max-w-[260px] text-ink-2">
                    {r.flying.length ? r.flying.slice(0, 3).map(link) : <span className="text-ink-3">—</span>}
                  </td>
                  <td className="px-3 py-2.5 border-b border-line max-w-[260px]" style={r.questions.length ? { color: 'var(--danger-ink)' } : undefined}>
                    {r.questions.length ? r.questions.slice(0, 3).map(link) : <span className="text-ink-3">—</span>}
                  </td>
                  <td className="px-3 py-2.5 border-b border-line amp-num whitespace-nowrap">{r.ready}{r.waiting > 0 && <span className="text-ink-3"> · {r.waiting} waiting</span>}</td>
                  <td className="px-3 py-2.5 border-b border-line amp-num text-ink-3 whitespace-nowrap">{ago(r.lastAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  )
}
