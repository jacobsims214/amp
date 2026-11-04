import { PageHeader } from '../components/ui/PageHeader'
import { Avatar } from '../components/ui/Avatar'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import ReactMarkdown from 'react-markdown'
import { Check, ClipboardCopy, Loader2, RefreshCw, Save } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { personName } from '../lib/people'
import type { DayReport, DayReportItem, Member, VelocityDay } from '../types'
import { projectLink } from '../lib/projectLink'

// /reports — end-of-day reports. Your own day is built live from AMP (what
// you completed, have in progress, are blocked on, started, created, and
// otherwise worked on); you add a summary and save it. Everyone in your orgs
// can read saved reports; ticket lists only show projects the reader can see.

const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
const localDay = (d = new Date()) => d.toLocaleDateString('en-CA') // YYYY-MM-DD
const addDays = (day: string, n: number) => {
  const d = new Date(`${day}T12:00:00`)
  d.setDate(d.getDate() + n)
  return localDay(d)
}
const prettyDay = (day: string) =>
  new Date(`${day}T12:00:00`).toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' })

const card = { background: 'var(--surface)', border: '1px solid var(--line)' }
const panelHead = 'flex items-center gap-2.5 flex-wrap px-4 py-3 border-b border-line'


export function DayReports() {
  const { me } = useAuth()
  const meId = me?.user_id
  const [people, setPeople] = useState<Member[]>([])
  // Whose reports are shown: whoever was picked, else you.
  const [picked, setWho] = useState<number | null>(null)
  const who = picked ?? meId ?? null
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api.reportPeople().then(setPeople).catch(e => setError(e instanceof Error ? e.message : 'Failed to load people'))
  }, [])

  const sorted = useMemo(
    () => [...people].sort((a, b) => (a.user_id === meId ? -1 : b.user_id === meId ? 1 : personName(a).localeCompare(personName(b)))),
    [people, meId],
  )
  const selected = people.find(p => p.user_id === who) ?? null
  const isMe = who != null && who === meId

  return (
    <div className="flex flex-col h-full" style={{ background: 'var(--bg)' }}>
      <PageHeader crumb="Overview" title="Day reports" meta={<>What each person and agent did, day by day · times in <span className="amp-num">{TZ}</span></>} />

      <div className="flex flex-1 overflow-hidden">
        <aside className="flex-shrink-0 overflow-y-auto bg-surface-2 border-r border-line" style={{ width: 232 }} aria-label="People">
          <div className="amp-label px-4 pt-4 pb-2">People & agents</div>
          {sorted.map(p => (
            <button key={p.user_id} onClick={() => setWho(p.user_id)}
              aria-current={p.user_id === who ? 'true' : undefined}
              className={`w-full flex items-center gap-2.5 px-4 py-2 text-[13px] text-left cursor-pointer ${
                p.user_id === who ? 'bg-surface text-ink font-semibold shadow-[inset_3px_0_0_var(--accent),0_0_0_1px_var(--line)]' : 'text-ink-2 hover:text-ink hover:bg-surface-3'}`}>
              <Avatar name={personName(p)} kind={p.kind ?? (p.email?.endsWith('@agents.amp') ? 'agent' : 'human')} size={20} />
              <span className="truncate flex-1">{personName(p)}</span>
              {p.user_id === meId && <span className="amp-num text-[10.5px] text-ink-3">you</span>}
            </button>
          ))}
          {sorted.length === 0 && !error && <Loader2 size={14} className="text-ink-3 animate-spin mx-4" />}
        </aside>

        <main className="flex-1 overflow-y-auto">
          <div className="max-w-4xl px-7 py-5 grid gap-4">
            {error && <p className="m-0 amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</p>}
            {selected && (
              <>
                <Velocity key={`v-${selected.user_id}`} person={selected} />
                {isMe && <MyDay key={`d-${selected.user_id}`} />}
                <History key={`h-${selected.user_id}`} person={selected} isMe={isMe} />
              </>
            )}
          </div>
        </main>
      </div>
    </div>
  )
}

/* ── Velocity ─────────────────────────────────────────────────────────────── */
function Velocity({ person }: { person: Member }) {
  const [days, setDays] = useState<VelocityDay[] | null>(null)
  useEffect(() => {
    const to = localDay()
    api.velocity({ user: String(person.user_id), from: addDays(to, -29), to, tz: TZ }).then(setDays).catch(() => setDays([]))
  }, [person.user_id])
  if (!days) return <div className="h-[150px]" style={card} />
  const max = Math.max(1, ...days.map(d => d.completed))
  const total = days.reduce((n, d) => n + d.completed, 0)
  const weekly = days.slice(-7).reduce((n, d) => n + d.completed, 0)
  return (
    <section style={card}>
      <div className={panelHead}>
        <span className="amp-label">Velocity</span><h2 className="m-0 text-[14.5px] font-semibold text-ink">Completed tickets, last 30 days</h2>
        <span className="ml-auto amp-num text-[11px] text-ink-3">{total} total · {weekly} this week · {(total / 30).toFixed(1)}/day</span>
      </div>
      <div className="px-4 pt-4 pb-3">
      <div className="flex items-end gap-1 h-24">
        {days.map(d => (
          <div key={d.day} className="flex-1 flex flex-col justify-end h-full group relative"
            title={`${prettyDay(d.day)}: ${d.completed}${Object.keys(d.by_project).length ? ' (' + Object.entries(d.by_project).map(([c, n]) => `${c} ${n}`).join(', ') + ')' : ''}`}>
            <div className="transition-colors"
              style={{ height: `${(d.completed / max) * 100}%`, minHeight: d.completed ? 3 : 1, background: d.completed ? 'var(--ink)' : 'var(--line)' }} />
          </div>
        ))}
      </div>
      <div className="flex justify-between amp-num text-[10.5px] text-ink-3 mt-1.5">
        <span>{prettyDay(days[0].day)}</span><span>Today</span>
      </div>
      </div>
    </section>
  )
}

/* ── Your day: live draft, summary, save ──────────────────────────────────── */
function MyDay() {
  const [day, setDay] = useState(localDay())
  const [report, setReport] = useState<DayReport | null>(null)
  const [summary, setSummary] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [savedAt, setSavedAt] = useState<string | null>(null)

  const load = useCallback(() => {
    setLoading(true); setError(null)
    api.dayReportDraft(day, TZ)
      .then(r => { setReport(r); setSummary(r.summary); setSavedAt(r.saved ? r.updated_at ?? null : null) })
      .catch(e => setError(e instanceof Error ? e.message : 'Failed to build the report'))
      .finally(() => setLoading(false))
  }, [day])
  useEffect(() => { load() }, [load])

  const save = async () => {
    setSaving(true); setError(null)
    try {
      const r = await api.saveDayReport(day, TZ, summary)
      setReport(r); setSavedAt(r.updated_at ?? null)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const dirty = report != null && summary !== report.summary
  return (
    <section className="amp-panel-top" style={card}>
      <div className={panelHead}>
        <span className="amp-label">Day report</span><h2 className="m-0 text-[14.5px] font-semibold text-ink">Your day</h2>
        <input type="date" value={day} max={localDay()} onChange={e => e.target.value && setDay(e.target.value)}
          className="amp-input !w-auto !py-1 !text-[12px]" />
        <button onClick={load} disabled={loading} className="amp-btn amp-btn-ghost" title="Rebuild from the latest activity">
          <RefreshCw size={12} className={loading ? 'animate-spin' : ''} />Refresh
        </button>
        <span className="amp-num text-[11px] text-ink-3 ml-auto">
          {savedAt ? `Saved ${new Date(savedAt).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })}` : 'Not saved yet'}
        </span>
      </div>
      <div className="p-4 grid gap-3">
      <p className="m-0 text-[12.5px] text-ink-2 leading-relaxed">
        Built from today's AMP activity across your projects. Add a summary in your own words, then save it so your
        team can read it. Or ask your agent to "wrap up today" and it will draft and save this for you.
      </p>
      <textarea value={summary} onChange={e => setSummary(e.target.value)} rows={5}
        placeholder="Highlights, blockers, and what's next…"
        className="amp-input resize-y" />
      <div className="flex items-center gap-2">
        <button onClick={save} disabled={saving || loading}
          className="amp-btn amp-btn-primary">
          {saving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
          {report?.saved ? (dirty ? 'Save changes' : 'Save again') : 'Save report'}
        </button>
        {error && <span className="text-xs text-danger-ink">{error}</span>}
      </div>
      {loading && !report ? (
        <Loader2 size={16} className="text-ink-3 animate-spin" />
      ) : report ? (
        <ReportBody report={{ ...report, summary }} showSummary={false} />
      ) : null}
      </div>
    </section>
  )
}

/* ── Saved reports ────────────────────────────────────────────────────────── */
function History({ person, isMe }: { person: Member; isMe: boolean }) {
  const [reports, setReports] = useState<DayReport[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [open, setOpen] = useState<string | null>(null)

  useEffect(() => {
    const to = localDay()
    api.dayReports({ user: String(person.user_id), from: addDays(to, -30), to, tz: TZ })
      .then(r => { setReports(r); setOpen(r[0]?.day ?? null) })
      .catch(e => setError(e instanceof Error ? e.message : 'Failed to load reports'))
  }, [person.user_id])

  return (
    <section className="space-y-3">
      <div className="flex items-center gap-2.5"><span className="amp-label">History</span><h2 className="m-0 text-[14.5px] font-semibold text-ink">{isMe ? 'Your saved reports' : `${personName(person)}'s reports`}</h2></div>
      {error && <p className="text-xs text-danger-ink">{error}</p>}
      {reports && reports.length === 0 && (
        <p className="text-xs text-ink-3">No saved reports in the last 30 days.</p>
      )}
      {reports?.map(r => (
        <div key={r.day} style={card}>
          <button onClick={() => setOpen(open === r.day ? null : r.day)}
            aria-expanded={open === r.day}
            className="w-full flex items-center gap-3 px-4 py-3 text-left cursor-pointer hover:bg-surface-2">
            <span className="text-[13.5px] font-semibold text-ink">{prettyDay(r.day)}</span>
            <span className="amp-num text-[11.5px] text-ink-2">
              {r.data.stats.completed} completed · {r.data.stats.in_progress} in progress{r.data.stats.blocked ? ` · ${r.data.stats.blocked} blocked` : ''}
            </span>
          </button>
          {open === r.day && <div className="px-4 pb-4 pt-1 border-t border-line"><ReportBody report={r} showSummary /></div>}
        </div>
      ))}
      {!reports && !error && <Loader2 size={16} className="text-ink-3 animate-spin" />}
    </section>
  )
}

/* ── A report: summary, ticket sections, copy ─────────────────────────────── */
function ReportBody({ report, showSummary }: { report: DayReport; showSummary: boolean }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      // Copy what's on screen: the server's markdown, with the summary as edited.
      const md = withSummary(report.markdown, report.summary)
      await navigator.clipboard.writeText(md)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      setCopied(false)
    }
  }
  const d = report.data
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <button onClick={copy} className="amp-btn">
          {copied ? <Check size={12} /> : <ClipboardCopy size={12} />}{copied ? 'Copied' : 'Copy as markdown'}
        </button>
        <span className="text-xs text-ink-3">
          {d.stats.projects} project{d.stats.projects === 1 ? '' : 's'}
          {d.stats.avg_cycle_hours != null ? ` · avg cycle ${formatHours(d.stats.avg_cycle_hours)}` : ''}
        </span>
      </div>
      {showSummary && report.summary && (
        <div className="text-sm text-ink leading-relaxed [&_p]:mb-2 [&_ul]:list-disc [&_ul]:pl-5">
          <ReactMarkdown>{report.summary}</ReactMarkdown>
        </div>
      )}
      <Section title="Completed" items={d.completed} tone="var(--st-done)" />
      <Section title="In progress" items={d.in_progress} tone="var(--st-prog)" />
      <Section title="Blocked" items={d.blocked} tone="var(--st-flag)" />
      <Section title="Started" items={d.started} tone="var(--ink-2)" />
      <Section title="Created" items={d.created} tone="var(--ink-2)" />
      <Section title="Also worked on" items={d.touched} tone="var(--ink-2)" />
      {!!report.hidden_items && (
        <p className="text-xs text-ink-3 italic">{report.hidden_items} more ticket{report.hidden_items === 1 ? '' : 's'} in projects you can't see.</p>
      )}
      {[d.completed, d.in_progress, d.blocked, d.started, d.created, d.touched].every(l => l.length === 0) && (
        <p className="text-xs text-ink-3">No ticket activity this day.</p>
      )}
    </div>
  )
}

function Section({ title, items, tone }: { title: string; items: DayReportItem[]; tone: string }) {
  if (items.length === 0) return null
  const byProject = new Map<string, DayReportItem[]>()
  for (const it of items) byProject.set(it.project_name, [...(byProject.get(it.project_name) ?? []), it])
  return (
    <div>
      <div className="flex items-center gap-2 mb-2"><i className="inline-block w-2 h-2" style={{ background: tone }} aria-hidden="true" /><span className="font-label text-[11.5px] font-semibold uppercase tracking-[0.14em] text-ink">{title}</span><span className="amp-num text-[11px] text-ink-3">{items.length}</span></div>
      {[...byProject.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([project, list]) => (
        <div key={project} className="mb-2">
          <div className="amp-label mb-1.5">{project}</div>
          <ul className="space-y-1">
            {list.map(it => (
              <li key={it.task_id} className="text-sm text-ink flex items-baseline gap-2">
                <Link to={projectLink(it.project_id, 'mission', it.task_id)} className="amp-num text-[11.5px] text-ink-2 underline underline-offset-2 hover:text-ink flex-shrink-0">{it.ref}</Link>
                <span className="min-w-0">
                  {it.name}
                  <span className="text-xs text-ink-3"> — {it.epic_name} › {it.story_name}</span>
                  {it.block_reason && !it.block_reason.startsWith('scheduled:') && <span className="text-xs text-danger-ink"> · question: {it.block_reason}</span>}
                </span>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  )
}

function formatHours(h: number) {
  if (h < 1) return `${Math.round(h * 60)}m`
  if (h < 48) return `${h.toFixed(1)}h`
  return `${(h / 24).toFixed(1)}d`
}

// The server's markdown puts the summary between the title line and the
// stats line; swap in the summary as currently edited.
function withSummary(markdown: string, summary: string) {
  const lines = markdown.split('\n')
  const stats = lines.findIndex(l => /^\*\*\d+ completed · /.test(l))
  if (lines.length === 0 || stats < 0) return markdown
  const body = summary.trim() ? [summary.trim(), ''] : []
  return [lines[0], '', ...body, ...lines.slice(stats)].join('\n')
}
