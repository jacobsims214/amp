import { UserRound, Bot } from 'lucide-react'
import { Panel } from './ui/PageHeader'
import { lookupPerson, type Person } from '../lib/people'
import { reasonLabel, reasonHint } from '../lib/human'
import type { PerformerStats, WorkReport } from '../types'

function hours(h?: number | null): string {
  if (h == null) return '—'
  if (h < 1) return `${Math.round(h * 60)}m`
  if (h < 48) return `${h.toFixed(1)}h`
  return `${(h / 24).toFixed(1)}d`
}

const ROWS: { label: string; hint: string; get: (s: PerformerStats) => string }[] = [
  { label: 'Tickets', hint: 'in the project', get: s => String(s.tickets) },
  { label: 'Open', hint: 'not done yet', get: s => String(s.open) },
  { label: 'Done', hint: 'in range', get: s => String(s.completed_in_range) },
  { label: 'Waited to start', hint: 'median, ready → started', get: s => hours(s.median_wait_hours) },
  { label: 'Took once started', hint: 'median, started → done', get: s => hours(s.median_cycle_hours) },
]

/** People in the loop: the agent/human split of a project's work, why people
 *  do what they do, and the human tickets that held the plan up longest. */
export function HumanWorkPanel({ work, people, rangeLabel, onOpen }: {
  work?: WorkReport; people: Map<number, Person>; rangeLabel: string; onOpen: (id: number) => void
}) {
  if (!work) return null
  const { agent, human } = work
  const total = agent.tickets + human.tickets
  const share = total ? Math.round(human.tickets / total * 100) : 0
  return (
    <Panel label="People in the loop" title={human.tickets ? `${share}% of tickets are done by people` : 'All tickets are agent work'}
      right={<span>wait = ready → started · {rangeLabel.toLowerCase()}</span>}>
      {human.tickets === 0 ? (
        <p className="m-0 px-4 py-4 text-[13px] text-ink-2 leading-relaxed">
          When a step needs a person (a sign-off, access agents don't have, a decision, a review of something that isn't code),
          plan it as a ticket done by a person. The agent work after it waits for them, and this panel shows what that waiting costs.
          Merges aren't tickets: a done ticket shows Merge needed until its merge is recorded.
        </p>
      ) : <>
        <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)]">
          <table className="w-full border-collapse text-[13px] lg:border-r border-line">
            <thead><tr className="bg-surface-2">
              <th className="amp-label text-left px-3 py-2.5 border-b border-line"> </th>
              <th className="amp-label text-right px-3 py-2.5 border-b border-line"><span className="inline-flex items-center gap-1"><Bot size={12} />Agents</span></th>
              <th className="amp-label text-right px-3 py-2.5 border-b border-line"><span className="inline-flex items-center gap-1"><UserRound size={12} />People</span></th>
            </tr></thead>
            <tbody>
              {ROWS.map(r => (
                <tr key={r.label}>
                  <td className="px-3 h-[36px] border-b border-line text-ink">{r.label} <span className="text-[11.5px] text-ink-3">{r.hint}</span></td>
                  <td className="px-3 border-b border-line amp-num text-right">{r.get(agent)}</td>
                  <td className="px-3 border-b border-line amp-num text-right">{r.get(human)}</td>
                </tr>
              ))}
              <tr>
                <td className="px-3 h-[36px] border-b border-line text-ink">Hours people reported <span className="text-[11.5px] text-ink-3">the work itself</span></td>
                <td className="px-3 border-b border-line amp-num text-right text-ink-3">—</td>
                <td className="px-3 border-b border-line amp-num text-right">{human.hours_worked ? hours(human.hours_worked) : '—'}</td>
              </tr>
            </tbody>
          </table>
          <div>
            <div className="amp-label px-3 py-2.5 border-b border-line bg-surface-2">Why people do it</div>
            {work.human_by_reason.map(r => {
              const max = Math.max(1, ...work.human_by_reason.map(x => x.tickets))
              return (
                <div key={r.reason} className="grid grid-cols-[96px_1fr_auto] gap-3 items-center px-3 h-[36px] border-b border-line" title={reasonHint(r.reason)}>
                  <span className="text-[13px] text-ink">{reasonLabel(r.reason)}</span>
                  <div className="h-[6px] bg-surface-3"><div className="h-full bg-ink" style={{ width: `${r.tickets / max * 100}%` }} /></div>
                  <span className="amp-num text-[12px] text-ink-2 whitespace-nowrap">{r.tickets} · {r.open} open · {hours(r.median_cycle_hours)}</span>
                </div>
              )
            })}
          </div>
        </div>
        {work.slowest_human_tickets.length > 0 && <>
          <div className="amp-label px-3 py-2.5 border-y border-line bg-surface-2">Human tickets that held the plan up longest</div>
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-[13px]">
              <thead><tr>
                {['Ticket', 'Why', 'Who', 'Waited', 'Took', 'Reported', 'Held up'].map((h, i) => (
                  <th key={h} className={`amp-label px-3 py-2 border-b border-line whitespace-nowrap ${i > 2 ? 'text-right' : 'text-left'}`}>{h}</th>))}
              </tr></thead>
              <tbody>
                {work.slowest_human_tickets.map(t => {
                  const who = lookupPerson(people, t.owner_user_id)
                  return (
                    <tr key={t.id} className="hover:bg-surface-2 cursor-pointer" onClick={() => onOpen(t.id)}>
                      <td className="px-3 h-[36px] border-b border-line max-w-[300px] truncate">
                        <span className="amp-num text-ink-3 mr-1.5">#{t.id}</span>{t.name}
                        {t.state !== 'completed' && <span className="ml-2 font-label text-[9.5px] font-semibold uppercase tracking-[0.1em] px-1 py-[2px] border border-line-str text-ink-2">open</span>}
                      </td>
                      <td className="px-3 border-b border-line text-ink-2">{reasonLabel(t.reason)}</td>
                      <td className="px-3 border-b border-line text-ink-2 whitespace-nowrap">{who?.name ?? 'nobody'}</td>
                      <td className="px-3 border-b border-line amp-num text-right" title="ready → started (or now)">{hours(t.wait_hours)}</td>
                      <td className="px-3 border-b border-line amp-num text-right" title="started → done (or now)">{hours(t.cycle_hours)}</td>
                      <td className="px-3 border-b border-line amp-num text-right" title="hours the person reported">{hours(t.hours_worked)}</td>
                      <td className="px-3 border-b border-line amp-num text-right" title={`${t.holding_up} open ticket(s) waiting on it now, ${t.downstream} after it in all`}>
                        {t.holding_up ? <span style={{ color: 'var(--danger-ink)' }}>{t.holding_up} open</span> : `${t.downstream}`}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </>}
      </>}
    </Panel>
  )
}
