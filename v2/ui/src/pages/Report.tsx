/**
 * Reports — what a project's agents and people got done in a range, in
 * totals rather than ticket lists: completions per day, each agent, each
 * epic, and a breakdown by any metadata key (each ticket counted under its
 * effective, inherited value). The completed tickets and the activity log
 * are below for detail. Exports to Markdown.
 */
import { BarChart } from '../components/ui/BarChart'
import { useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { Download, Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { useBoardData } from '../hooks/useBoardData'
import { useProjectPeople } from '../hooks/useProjectPeople'
import { PeoplePanel } from '../components/PeoplePanel'
import { StandupSheet } from '../components/StandupSheet'
import { peopleRows } from '../lib/standup'
import { tokens, usd } from '../lib/usage'
import { activityActor } from '../lib/people'
import { ProjectNav } from '../components/ProjectNav'
import { Panel, Readouts } from '../components/ui/PageHeader'
import { Avatar } from '../components/ui/Avatar'
import { StateBar } from '../components/ui/StateTag'
import type { DisplayState } from '../lib/ticketState'
import type { ActivityLog, ProjectReport, ReportGroup, ReportStates } from '../types'

type Range = '1d' | '7d' | '30d' | 'all'
const RANGE_LABELS: Record<Range, string> = { '1d': '24h', '7d': '7 days', '30d': '30 days', all: 'All time' }
const RANGE_DAYS: Record<Range, number> = { '1d': 1, '7d': 7, '30d': 30, all: 3650 }

function hours(h?: number | null): string {
  if (h == null) return '—'
  if (h < 1) return `${Math.round(h * 60)}m`
  if (h < 48) return `${h.toFixed(1)}h`
  return `${(h / 24).toFixed(1)}d`
}

/** A report's counts in the UI's display-state shape. */
function asCounts(s: ReportStates): Record<DisplayState, number> {
  return { ready: s.ready, prog: s.in_progress, wait: s.waiting, flag: s.flagged, sched: s.scheduled, done: s.done }
}

function GroupTable({ rows, first, label }: { rows: ReportGroup[]; first: string; label?: (g: ReportGroup) => React.ReactNode }) {
  return (
    <table className="w-full border-collapse text-[13px]">
      <thead><tr className="bg-surface-2">
        {[first, 'Tickets', 'Done', 'In progress', 'Questions', 'Complete', 'Median time', 'Tokens', 'Cost', 'Mix'].map((h, i) => (
          <th key={h} className={`amp-label px-3 py-2.5 border-b border-line whitespace-nowrap ${i ? 'text-right' : 'text-left'}`}>{h}</th>))}
      </tr></thead>
      <tbody>
        {rows.map(g => (
          <tr key={g.key}>
            <td className="px-3 h-[38px] border-b border-line font-semibold text-ink max-w-[280px] truncate">{label ? label(g) : g.label ?? g.key}</td>
            <td className="px-3 border-b border-line amp-num text-right">{g.tickets}</td>
            <td className="px-3 border-b border-line amp-num text-right">{g.states.done}</td>
            <td className="px-3 border-b border-line amp-num text-right">{g.states.in_progress}</td>
            <td className="px-3 border-b border-line amp-num text-right" style={g.states.flagged ? { color: 'var(--danger-ink)' } : undefined}>{g.states.flagged}</td>
            <td className="px-3 border-b border-line amp-num text-right">{g.tickets ? Math.round(g.states.done / g.tickets * 100) : 0}%</td>
            <td className="px-3 border-b border-line amp-num text-right">{hours(g.median_cycle_hours)}</td>
            <td className="px-3 border-b border-line amp-num text-right" title={g.usage ? `${g.usage.input_tokens} in · ${g.usage.output_tokens} out · ${g.usage.runs} runs` : undefined}>{g.usage ? tokens(g.usage.input_tokens + g.usage.output_tokens) : '—'}</td>
            <td className="px-3 border-b border-line amp-num text-right">{g.usage?.cost_usd ? usd(g.usage.cost_usd) : '—'}</td>
            <td className="px-3 border-b border-line w-[150px]"><StateBar counts={asCounts(g.states)} /></td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function DailyChart({ days }: { days: { day: string; n: number }[] }) {
  const labelEvery = Math.ceil(days.length / 8)
  const fmt: Intl.DateTimeFormatOptions = days.length > 8 ? { month: 'short', day: 'numeric' } : { weekday: 'short' }
  return <BarChart ariaLabel="Tickets completed per day" bars={days.map((d, i) => ({
    key: d.day, value: d.n, title: `${d.day}: ${d.n} done`,
    label: i % labelEvery === 0 ? new Date(d.day + 'T12:00').toLocaleDateString(undefined, fmt) : '',
  }))} />
}

export function Report() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)
  const [range, setRangeState] = useState<Range>('7d')
  // The range ends when it was picked, not at each render.
  const [now, setNow] = useState(Date.now)
  const setRange = (r: Range) => { setRangeState(r); setNow(() => Date.now()) }
  const [groupBy, setGroupBy] = useState<string>('')
  const [report, setReport] = useState<ProjectReport | null>(null)
  const [activity, setActivity] = useState<ActivityLog[]>([])
  const [error, setError] = useState<string | null>(null)
  const [actionFilter, setActionFilter] = useState('all')
  const { tasks } = useBoardData(pid)
  const { people } = useProjectPeople(pid)
  const navigate = useNavigate()
  const taskById = useMemo(() => new Map(tasks.map(t => [t.id, t])), [tasks])

  const since = useMemo(() => new Date(now - RANGE_DAYS[range] * 86_400_000).toISOString(), [range, now])
  useEffect(() => {
    api.projectReport(pid, { since, groupBy })
      .then(r => {
        setReport(r); setError(null)
        // Default the breakdown to the best-covered key the first time.
        if (!groupBy && r.metadata_keys.length) setGroupBy(r.metadata_keys[0].key)
      })
      .catch(e => setError(e instanceof Error ? e.message : 'Failed to load the report'))
  }, [pid, since, groupBy])
  useEffect(() => {
    api.getProjectActivity(pid, since).then(setActivity).catch(() => {})
  }, [pid, since])

  const perDay = useMemo(() => {
    const days = Math.min(RANGE_DAYS[range], 30)
    const counts = new Map<string, number>()
    for (let i = days - 1; i >= 0; i--) counts.set(new Date(now - i * 86_400_000).toLocaleDateString('en-CA'), 0)
    for (const a of activity) if (a.action === 'completed') {
      const d = new Date(a.created_at).toLocaleDateString('en-CA')
      if (counts.has(d)) counts.set(d, counts.get(d)! + 1)
    }
    return [...counts.entries()].map(([day, n]) => ({ day, n }))
  }, [activity, range, now])

  const completed = useMemo(() => tasks.filter(t => t.state === 'completed' && t.completed_at && t.completed_at >= since)
    .sort((a, b) => b.completed_at!.localeCompare(a.completed_at!)), [tasks, since])
  const filtered = actionFilter === 'all' ? activity : activity.filter(a => a.action === actionFilter)

  const exportMarkdown = () => {
    if (!report) return
    const lines = [
      `# Report · project ${pid} · ${RANGE_LABELS[range]}`, '',
      `${report.completed} done, ${report.dispatched} started, ${report.created} created. Median time to done ${hours(report.median_cycle_hours)}.`, '',
      '## Standup', '', '| Person | Done | Now | Waiting on them | Ready next |', '|---|---:|---|---|---:|',
      ...peopleRows(tasks, activity, people, since).map(r => `| ${r.name} | ${r.done.length} | ${r.flying.map(t => `#${t.id} ${t.name}`).join('; ') || '—'} | ${r.questions.map(t => `#${t.id} ${t.block_reason}`).join('; ') || '—'} | ${r.ready} |`), '',
      '## Token spend', '', `${tokens((report.usage?.input_tokens ?? 0) + (report.usage?.output_tokens ?? 0))} tokens, ${usd(report.usage?.cost_usd ?? 0)} over ${report.usage?.runs ?? 0} runs.`, '',
      '| Model | Runs | Input | Output | Cost |', '|---|---:|---:|---:|---:|',
      ...(report.usage_by_model ?? []).map(m => `| ${m.model} | ${m.runs} | ${m.input_tokens} | ${m.output_tokens} | ${m.cost_usd.toFixed(4)} |`), '',
      '## Agents', '', '| Agent | Done | Median |', '|---|---:|---:|',
      ...report.agents.map(a => `| ${a.key} | ${a.completed_in_range} | ${hours(a.median_cycle_hours)} |`), '',
      '## Epics', '', '| Epic | Done | Tickets |', '|---|---:|---:|',
      ...report.epics.map(e => `| ${e.label} | ${e.states.done} | ${e.tickets} |`), '',
      ...(report.breakdown ? [`## By ${report.group_by}`, '', '| Value | Tickets | Done | Questions | Input tokens | Output tokens | Cost |', '|---|---:|---:|---:|---:|---:|---:|',
        ...report.breakdown.map(g => `| ${g.key} | ${g.tickets} | ${g.states.done} | ${g.states.flagged} | ${g.usage?.input_tokens ?? 0} | ${g.usage?.output_tokens ?? 0} | ${(g.usage?.cost_usd ?? 0).toFixed(4)} |`), ''] : []),
      '## Completed', '', ...completed.map(t => `- #${t.id} ${t.name}${t.agent_id ? ` (${t.agent_id})` : ''}`),
    ]
    const url = URL.createObjectURL(new Blob([lines.join('\n')], { type: 'text/markdown' }))
    const a = document.createElement('a')
    a.href = url; a.download = `amp-report-${pid}-${range}.md`; a.click()
    URL.revokeObjectURL(url)
  }

  return (
    <div className="flex flex-col min-h-full">
      <ProjectNav projectId={pid} currentView="report" rightSlot={<>
        <div className="flex border border-line" role="group" aria-label="Range">
          {(Object.keys(RANGE_LABELS) as Range[]).map(r => (
            <button key={r} type="button" onClick={() => setRange(r)} aria-pressed={range === r}
              className={`font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] px-2.5 py-2 border-l border-line first:border-l-0 cursor-pointer ${range === r ? 'bg-ink text-on-ink' : 'bg-surface text-ink-2 hover:text-ink'}`}>
              {RANGE_LABELS[r]}</button>
          ))}
        </div>
        <button type="button" className="amp-btn" onClick={exportMarkdown} disabled={!report}><Download size={12} />Markdown</button>
      </>} />
      <div className="p-7 pt-5 grid gap-4">
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {!report && !error && <div className="flex justify-center py-16"><Loader2 size={20} className="animate-spin text-ink-3" /></div>}
        {report && <>
          <Readouts items={[
            { label: 'Done', value: report.completed, sub: `completed in ${RANGE_LABELS[range].toLowerCase()}` },
            { label: 'Started', value: report.dispatched, sub: 'dispatched', color: 'var(--st-prog)' },
            { label: 'Created', value: report.created, sub: 'new tickets' },
            { label: 'Median time', value: hours(report.median_cycle_hours), sub: 'dispatch → done' },
            { label: 'Waiting on answers', value: hours(report.flag_hours), sub: `${report.states.flagged} open question${report.states.flagged === 1 ? '' : 's'}`, color: report.states.flagged ? 'var(--danger-ink)' : undefined },
            { label: 'Tokens', value: tokens((report.usage?.input_tokens ?? 0) + (report.usage?.output_tokens ?? 0)),
              sub: report.usage?.runs ? `${usd(report.usage.cost_usd)} · ${report.usage.runs} run${report.usage.runs === 1 ? '' : 's'}${report.usage.abandoned ? `, ${report.usage.abandoned} abandoned` : ''}` : 'no runs reported' },
            { label: 'Plan', value: <>{report.states.done}<span className="text-[13px] text-ink-3">/{report.tickets}</span></>, sub: report.tickets ? `${Math.round(report.states.done / report.tickets * 100)}% done` : 'no tickets' },
          ]} />

          <PeoplePanel tasks={tasks} activity={activity} people={people} since={since} rangeLabel={RANGE_LABELS[range]}
            usageByPerson={new Map((report.people ?? []).map(g => [g.key, g.usage]))}
            onOpen={id => navigate(`/project/${pid}?ticket=${id}`)} />

          <StandupSheet projectId={pid} />

          <Panel label="Spend" title="Tokens by model" right={<span>runs recorded in {RANGE_LABELS[range].toLowerCase()}</span>}>
            {!report.usage_by_model?.length ? (
              <p className="m-0 px-4 py-4 text-[13px] text-ink-2">No runs recorded yet. Agents report their model and token usage when they complete or stop a ticket; harnesses that can't tell them their usage still report the model.</p>
            ) : (
              <table className="w-full border-collapse text-[13px]">
                <thead><tr className="bg-surface-2">
                  {['Model', 'Runs', 'Input', 'Output', 'Cache read', 'Cost'].map((h, i) => <th key={h} className={`amp-label px-3 py-2.5 border-b border-line ${i ? 'text-right' : 'text-left'}`}>{h}</th>)}
                </tr></thead>
                <tbody>
                  {report.usage_by_model.map(m => (
                    <tr key={m.model}>
                      <td className="px-3 h-[38px] border-b border-line amp-num">{m.model}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{m.runs}{m.unreported ? <span className="text-ink-3"> ({m.unreported} no usage)</span> : ''}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{tokens(m.input_tokens)}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{tokens(m.output_tokens)}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{tokens(m.cache_read_tokens)}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{m.cost_usd ? usd(m.cost_usd) : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-[minmax(0,1.1fr)_minmax(0,1fr)]">
            <Panel label="Velocity" title="Done per day" right={<span>{perDay.length} days</span>}>
              <div className="px-4 py-3"><DailyChart days={perDay} /></div>
            </Panel>
            <Panel label="Agents" title="Who did the work" right={<span>by agent id</span>}>
              {report.agents.length === 0 ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">No agent has worked a ticket here yet.</p> : (
                <table className="w-full border-collapse text-[13px]">
                  <thead><tr className="bg-surface-2">
                    {['Agent', 'Done', '', 'Median', 'Active', 'Questions', 'Tokens / done'].map((h, i) => <th key={i} className={`amp-label px-3 py-2.5 border-b border-line ${i && i !== 2 ? 'text-right' : 'text-left'}`}>{h}</th>)}
                  </tr></thead>
                  <tbody>
                    {report.agents.map(a => {
                      const max = Math.max(1, ...report.agents.map(x => x.completed_in_range))
                      return (
                        <tr key={a.key}>
                          <td className="px-3 h-[38px] border-b border-line"><span className="inline-flex items-center gap-2"><Avatar name={a.key} kind="agent" size={20} />{a.key}</span></td>
                          <td className="px-3 border-b border-line amp-num text-right">{a.completed_in_range}</td>
                          <td className="px-3 border-b border-line w-[110px]"><div className="h-[6px] bg-surface-3"><div className="h-full bg-ink" style={{ width: `${a.completed_in_range / max * 100}%` }} /></div></td>
                          <td className="px-3 border-b border-line amp-num text-right">{hours(a.median_cycle_hours)}</td>
                          <td className="px-3 border-b border-line amp-num text-right">{a.states.in_progress}</td>
                          <td className="px-3 border-b border-line amp-num text-right" style={a.states.flagged ? { color: 'var(--danger-ink)' } : undefined}>{a.states.flagged}</td>
                          <td className="px-3 border-b border-line amp-num text-right" title={a.usage ? `${a.usage.runs} runs · ${usd(a.usage.cost_usd)}` : 'no usage reported'}>
                            {a.usage && a.completed_in_range ? tokens(Math.round((a.usage.input_tokens + a.usage.output_tokens) / a.completed_in_range)) : '—'}</td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              )}
            </Panel>
          </div>

          <Panel label="Metadata" title={report.group_by ? `By ${report.group_by}` : 'By metadata'}
            right={<span>effective values: project → epic → story → ticket</span>}>
            {report.metadata_keys.length === 0 ? (
              <p className="m-0 px-4 py-4 text-[13px] text-ink-2">No metadata on this project's tickets yet. Add keys like <span className="amp-num">initiative</span> or <span className="amp-num">jira_epic</span> to a project, epic, story, or ticket and they show up here.</p>
            ) : <>
              <div className="flex flex-wrap gap-1.5 px-4 py-3 border-b border-line" role="group" aria-label="Metadata key">
                {report.metadata_keys.map(k => (
                  <button key={k.key} type="button" onClick={() => setGroupBy(k.key)} aria-pressed={groupBy === k.key}
                    className={`amp-num text-[11.5px] px-2 py-1.5 border cursor-pointer ${groupBy === k.key ? 'border-ink text-ink shadow-[inset_0_-2px_0_var(--accent)]' : 'border-line text-ink-2 hover:border-line-str'}`}
                    title={`${Math.round(k.coverage * 100)}% of tickets have it`}>
                    {k.key} <span className="text-ink-3">{Math.round(k.coverage * 100)}%</span></button>
                ))}
              </div>
              {report.breakdown && <GroupTable rows={report.breakdown} first={report.group_by ?? 'Value'} />}
            </>}
          </Panel>

          <Panel label="Epics" title="Progress" right={<span>{report.epics.length} epics</span>}>
            <GroupTable rows={report.epics} first="Epic" />
          </Panel>

          <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-2">
            <Panel label="Completed" title={`${completed.length} tickets`}>
              <div className="max-h-[420px] overflow-auto">
                <table className="w-full border-collapse text-[13px]">
                  <tbody>
                    {completed.map(t => (
                      <tr key={t.id}>
                        <td className="px-3 h-[34px] border-b border-line amp-num text-ink-3 w-[56px]">#{t.id}</td>
                        <td className="px-3 border-b border-line max-w-[300px] truncate">{t.name}</td>
                        <td className="px-3 border-b border-line amp-num text-[11.5px] text-ink-2">{t.agent_id || '—'}</td>
                        <td className="px-3 border-b border-line amp-num text-[11.5px] text-ink-3 text-right whitespace-nowrap">{new Date(t.completed_at!).toLocaleString(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit' })}</td>
                      </tr>
                    ))}
                    {completed.length === 0 && <tr><td className="px-4 py-4 text-ink-2">Nothing completed in this range.</td></tr>}
                  </tbody>
                </table>
              </div>
            </Panel>
            <Panel label="Activity" title={`${activity.length} events`} right={
              <select value={actionFilter} onChange={e => setActionFilter(e.target.value)} aria-label="Filter activity"
                className="amp-input !w-auto !py-1 text-[12px]">
                {['all', 'created', 'dispatched', 'completed', 'blocked', 'unblocked', 'comment', 'state_change'].map(a => <option key={a} value={a}>{a.replace('_', ' ')}</option>)}
              </select>}>
              <div className="max-h-[420px] overflow-auto">
                {filtered.slice().sort((a, b) => b.created_at.localeCompare(a.created_at)).slice(0, 200).map(a => (
                  <div key={a.id} className="grid grid-cols-[96px_1fr] gap-3 px-4 py-1.5 border-b border-line text-[12.5px]">
                    <span className="amp-num text-[11px] text-ink-3">{new Date(a.created_at).toLocaleString(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit' })}</span>
                    <span className="min-w-0 truncate"><b className="font-semibold">{activityActor(a, people)}</b> {a.action.replace('_', ' ')} <span className="amp-num text-ink-3">#{a.task_id}</span> <span className="text-ink-2">{taskById.get(a.task_id)?.name ?? ''}</span></span>
                  </div>
                ))}
              </div>
            </Panel>
          </div>
        </>}
      </div>
    </div>
  )
}
