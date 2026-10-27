/**
 * My work (`/`): a summary across your projects (KB decisions/human-work-ux.md §3).
 * It only informs. Nothing on it is a button: every row is a link into that
 * project's My work tab with the item focused (`?focus=<ticket id>`), where
 * the action lives. The counts come from the server's one "waiting on you"
 * classifier (GET /api/my-work, KB architecture/waiting-on-you.md).
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { ChevronRight } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { BarChart } from '../components/ui/BarChart'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { QUIET_MS, silentFor } from '../lib/ticketState'
import type { Artifact, DayReport, MyWork as MyWorkData, ProjectRef, ProjectWait, VelocityDay, WaitCounts, WaitItem, WaitKind, WorkTask } from '../types'
import { BandLoader } from '../components/brand/BandLoader'
import { isHuman, reasonLabel } from '../lib/human'
import { expiresIn } from '../lib/artifacts'
import { MySessionsPanel } from '../components/remote/MySessionsPanel'
import { remoteAvailable, useRemote } from '../remote/remote-context'
import { projectLink } from '../lib/projectLink'

const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone
const ymd = (d: Date) => d.toLocaleDateString('en-CA', { timeZone: TZ })
/** How many projects with nothing for you are listed before "N more". */
const CLEAR_SHOWN = 8

/** The four kinds, in display order. Color is always next to its label. */
const KINDS: { kind: WaitKind; label: string; color: string; unit: (n: number) => string }[] = [
  { kind: 'approve', label: 'Approve', color: 'var(--violet)', unit: n => (n === 1 ? 'plan' : 'plans') },
  { kind: 'answer', label: 'Answer', color: 'var(--st-flag)', unit: n => (n === 1 ? 'question' : 'questions') },
  { kind: 'merge', label: 'Merge', color: 'var(--teal)', unit: n => (n === 1 ? 'PR' : 'PRs') },
  { kind: 'do', label: 'Do', color: 'var(--ink)', unit: n => (n === 1 ? 'your ticket' : 'your tickets') },
]
const kindOf = (k: string) => KINDS.find(x => x.kind === k)

const ZERO: WaitCounts = { waiting: 0, approve: 0, answer: 0, merge: 0, do: 0, coming_up: 0 }

function ago(iso?: string | null): string {
  if (!iso) return ''
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 3_600_000) return `${Math.max(1, Math.round(ms / 60_000))}m`
  if (ms < 172_800_000) return `${Math.round(ms / 3_600_000)}h`
  return `${Math.round(ms / 86_400_000)}d`
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

/** The ticket a row focuses: the ticket itself, or an approval's first ticket. */
function itemFocus(it: WaitItem): number | undefined {
  return it.task_id || it.task_ids?.[0] || undefined
}

/** The link a row opens: the project's My work with the item focused. */
function itemLink(it: WaitItem): string {
  return projectLink(it.project_id, 'my-work', itemFocus(it))
}

function prNumber(url?: string): string | null {
  const m = url?.match(/\/pull\/(\d+)/)
  return m ? `PR #${m[1]}` : null
}

/** A row's title and the small line under it. */
function describe(it: WaitItem): { title: string; sub: string } {
  const waitOn = (n?: number) => (n ? ` · ${plural(n, 'ticket')} wait${n === 1 ? 's' : ''}` : '')
  switch (it.kind) {
    case 'approve':
      return { title: `Approve the plan: ${it.name || `epic ${it.epic_id}`} (${plural(it.count ?? it.task_ids.length, 'ticket')})`,
        sub: `Epic ${it.epic_id} · planned ${ago(it.since)} ago` }
    case 'answer':
      return { title: `#${it.task_id} ${it.name}`, sub: `Question · waiting ${ago(it.since)}` }
    case 'merge': {
      const pr = prNumber(it.pr_url)
      return { title: `#${it.task_id} ${pr ? `Merge ${pr}: ` : 'Merge: '}${it.name}`,
        sub: `${it.merge_state === 'abandoned' ? 'PR closed without merging' : `Done ${ago(it.completed_at ?? it.since)} ago`}${waitOn(it.holding_up?.length)}` }
    }
    default:
      return { title: `#${it.task_id} ${it.name}`,
        sub: `${it.state === 'in_progress' ? 'Started' : 'Ready'} ${ago(it.since)} · ${reasonLabel(it.human_reason)}` }
  }
}

function KindMark({ kind, children }: { kind: string; children?: React.ReactNode }) {
  const k = kindOf(kind)
  return (
    <span className="inline-flex items-center gap-1.5 amp-label" style={{ color: k?.color }}>
      <i aria-hidden="true" className="block w-2 h-2" style={{ background: k?.color }} />
      {children ?? k?.label}
    </span>
  )
}

/** Plain link row with a chevron: never styled as a button. */
function LinkRow({ to, lead, title, sub, label, wide = true }: { to: string; lead?: React.ReactNode; title: React.ReactNode; sub?: React.ReactNode; label?: string; wide?: boolean }) {
  return (
    <li className="border-t border-line-sub first:border-t-0">
      <Link to={to} aria-label={label} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3 gap-y-0.5 px-4 py-2.5 hover:bg-surface-2 group">
        <span className={`row-span-2 self-start pt-0.5 ${wide ? 'min-w-[68px]' : 'min-w-[16px]'}`}>{lead}</span>
        <span className="text-[13px] text-ink truncate group-hover:underline">{title}</span>
        <ChevronRight size={15} className="row-span-2 text-ink-3" aria-hidden="true" />
        {sub && <span className="text-[11.5px] text-ink-3 truncate">{sub}</span>}
      </Link>
    </li>
  )
}

function ProjectCard({ p }: { p: ProjectWait }) {
  const myWork = projectLink(p.project_id, 'my-work')
  return (
    <article className="amp-panel amp-panel-top" aria-label={`${p.project_name}: ${p.counts.waiting} waiting on you`}>
      <Link to={myWork} aria-label={`Open My work in ${p.project_name}, ${p.counts.waiting} waiting`} className="flex items-center gap-3 px-4 py-3.5 border-b border-line hover:bg-surface-2 flex-wrap">
        <h2 className="m-0 text-[16px] font-semibold text-ink">{p.project_name}</h2>
        <span className="amp-num text-[10.5px] text-ink-3 border border-line px-1">{p.project_code}</span>
        <span className="ml-auto amp-label inline-flex items-center gap-1.5" style={{ color: 'var(--link)' }}>
          <span className="hidden sm:inline">Open My work in {p.project_name}</span><span className="sm:hidden">My work</span>
          <span className="amp-num" style={{ letterSpacing: 0 }}>{p.counts.waiting}</span> ›
        </span>
      </Link>
      <div className="flex flex-wrap gap-x-[18px] gap-y-1.5 px-4 py-2.5 border-b border-line-sub bg-surface-2">
        {KINDS.map(k => (
          <span key={k.kind} className={p.counts[k.kind] ? '' : 'opacity-60'}>
            <KindMark kind={k.kind}>{k.label} <span className="amp-num text-[13px]" style={{ letterSpacing: 0 }}>{p.counts[k.kind]}</span></KindMark>
          </span>
        ))}
        {p.counts.coming_up > 0 && <span className="ml-auto text-[12px] text-ink-3">{p.counts.coming_up} coming up</span>}
      </div>
      <ul className="list-none m-0 p-0">
        {p.rows.map(it => {
          const { title, sub } = describe(it)
          return <LinkRow key={it.key} to={itemLink(it)} lead={<KindMark kind={it.kind} />} title={title} sub={sub} />
        })}
        {p.more > 0 && (
          <li className="border-t border-line-sub">
            <Link to={myWork} className="flex items-center gap-1 px-4 py-2.5 text-[12.5px] hover:underline" style={{ color: 'var(--link)' }}>
              +{p.more} more in {p.project_name} ›
            </Link>
          </li>
        )}
      </ul>
    </article>
  )
}

function SectionRule({ children }: { children: React.ReactNode }) {
  return <div className="flex items-center gap-3 mt-2"><span className="amp-label">{children}</span><span className="flex-1 h-px bg-line" /></div>
}

function Velocity({ days }: { days: VelocityDay[] }) {
  const short = (d: string) => new Date(d + 'T12:00').toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  return <BarChart ariaLabel="Tickets you completed per day" bars={days.map((d, i) => {
    const today = i === days.length - 1
    return { key: d.day, value: d.completed, title: `${d.day}: ${d.completed} completed`, highlight: today,
      label: today ? 'today' : i === 0 ? short(d.day) : '' }
  })} />
}

export function MyWork() {
  const { me } = useAuth()
  const remote = useRemote()
  const [work, setWork] = useState<MyWorkData | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [velocity, setVelocity] = useState<VelocityDay[]>([])
  const [draft, setDraft] = useState<DayReport | null>(null)
  const [expiring, setExpiring] = useState<Artifact[]>([])

  useEffect(() => {
    api.myWork().then(setWork).catch(e => setError(e instanceof Error ? e.message : 'Failed to load'))
    const to = new Date(), from = new Date(Date.now() - 13 * 86_400_000)
    api.velocity({ from: ymd(from), to: ymd(to), tz: TZ }).then(setVelocity).catch(() => {})
    api.dayReportDraft(ymd(to), TZ).then(setDraft).catch(() => {})
    api.expiringArtifacts(14).then(setExpiring).catch(() => {})
  }, [])

  const counts = work?.counts ?? ZERO
  const cards = work?.projects ?? []
  const waitingCards = cards.filter(p => p.counts.waiting > 0)
  const comingOnly = cards.filter(p => p.counts.waiting === 0)
  const clear = work?.clear_projects ?? []
  const totalProjects = (work?.projects_total ?? cards.length) + clear.length

  // Your agents, per project: your agent tickets in progress (the older
  // `owned` list) plus agent accounts' tickets.
  const owned = work?.owned ?? []
  const flying: WorkTask[] = [
    ...owned.filter(t => t.state === 'in_progress' && !isHuman(t)),
    ...(work?.agents ?? []).flatMap(a => a.working),
  ]
  const silent = (t: WorkTask) => silentFor([t.last_activity_at, t.dispatched_at, t.updated_at])
  const agentsByProject = new Map<number, { ref: ProjectRef; n: number; quiet: number }>()
  for (const t of flying) {
    const e = agentsByProject.get(t.project_id) ?? { ref: { id: t.project_id, code: t.project_code, name: t.project_name }, n: 0, quiet: 0 }
    e.n++
    if (silent(t) > QUIET_MS) e.quiet++
    agentsByProject.set(t.project_id, e)
  }
  for (const p of cards) {
    const e = agentsByProject.get(p.project_id)
    if (p.agents_in_progress > (e?.n ?? 0)) {
      agentsByProject.set(p.project_id, { ref: { id: p.project_id, code: p.project_code, name: p.project_name }, n: p.agents_in_progress, quiet: e?.quiet ?? 0 })
    }
  }
  const agentsTotal = work?.agents_in_progress ?? flying.length
  const agentsIn = (id: number) => agentsByProject.get(id)?.n ?? 0
  const done24 = (work?.done_24h ?? 0) + (work?.agents ?? []).reduce((a, x) => a + x.done_24h, 0)
  const clearSub = (id: number) => (agentsIn(id) ? `${plural(agentsIn(id), 'agent')} running` : 'Quiet')

  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Overview" title="My work"
        meta="A summary across your projects. Each row opens the item in that project's My work." />
      <div className="p-4 sm:p-7 sm:pt-5 grid gap-4">
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {!work && !error && <div className="flex justify-center py-16"><BandLoader label="Loading your work" /></div>}
        {work && <>
          <div className="amp-panel amp-panel-top grid grid-cols-3 md:grid-cols-6" aria-label="Waiting on you, across your projects">
            <div className="col-span-3 md:col-span-1 px-4 py-3 grid gap-1.5 border-b md:border-b-0 border-line"
              style={counts.waiting ? { background: 'var(--st-flag)', color: '#fff' } : undefined}>
              <span className="amp-label" style={counts.waiting ? { color: '#fff' } : undefined}>Waiting on you</span>
              <span className="amp-num text-[26px] leading-none" style={counts.waiting ? undefined : { color: 'var(--success-ink)' }}>{counts.waiting}</span>
              <span className="text-[11.5px]" style={{ color: counts.waiting ? '#fff' : 'var(--ink-2)' }}>
                {counts.waiting ? `in ${waitingCards.length} of ${totalProjects} project${totalProjects === 1 ? '' : 's'}` : `in all ${totalProjects} project${totalProjects === 1 ? '' : 's'}`}
              </span>
            </div>
            {KINDS.map((k, i) => (
              <div key={k.kind} className={`px-4 py-3 grid gap-1.5 border-line ${i % 3 ? 'border-l' : ''} md:border-l ${i < 3 ? 'border-b md:border-b-0' : ''} ${counts[k.kind] ? '' : 'opacity-60'}`}>
                <KindMark kind={k.kind} />
                <span className="amp-num text-[26px] leading-none text-ink">{counts[k.kind]}</span>
                {counts[k.kind] > 0 && <span className="text-[11.5px] text-ink-2">{k.unit(counts[k.kind])}</span>}
              </div>
            ))}
            <div className="col-span-2 md:col-span-1 px-4 py-3 grid gap-1.5 border-l border-line">
              <span className="amp-label">Your agents</span>
              <span className="amp-num text-[26px] leading-none" style={{ color: 'var(--st-prog)' }}>{agentsTotal}</span>
              <span className="text-[11.5px] text-ink-2">in progress</span>
            </div>
          </div>

          <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
            <section className="grid gap-4 min-w-0" aria-label="By project">
              {waitingCards.length > 0 ? <>
                <SectionRule>By project · most waiting first</SectionRule>
                {waitingCards.map(p => <ProjectCard key={p.project_id} p={p} />)}
                {work.projects_more && (
                  <p className="m-0 text-[12px] text-ink-3">
                    Showing {cards.length} of {work.projects_total} projects with something for you.
                  </p>
                )}
              </> : (
                <div className="amp-panel px-7 py-9 grid gap-2.5">
                  {done24 > 0
                    ? <span className="amp-label" style={{ color: 'var(--success-ink)' }}>All clear</span>
                    : <span className="amp-label">Nothing waits on you</span>}
                  <h2 className="m-0 text-[20px] font-semibold text-ink">
                    {done24 > 0 ? `Nothing waits on you. ${plural(done24, 'ticket')} done in the last 24h.` : 'No project needs you right now.'}
                  </h2>
                  <p className="m-0 text-ink-2 max-w-[56ch] leading-normal text-[13.5px]">
                    Approvals, questions, merges and your own tickets appear here, grouped by project. Open a project to act on them.
                  </p>
                  <div className="flex gap-3.5 flex-wrap mt-1.5">{KINDS.map(k => <KindMark key={k.kind} kind={k.kind} />)}</div>
                </div>
              )}

              {(comingOnly.length > 0 || clear.length > 0) && <>
                <SectionRule>{waitingCards.length ? 'Nothing waiting on you' : 'Your projects'}</SectionRule>
                <div className="amp-panel">
                  <ul className="list-none m-0 p-0">
                    {comingOnly.map(p => (
                      <LinkRow key={p.project_id} to={projectLink(p.project_id, 'my-work')} title={p.project_name}
                        lead={<span className="amp-num text-[10.5px] text-ink-3">{p.project_code}</span>}
                        sub={`${p.counts.coming_up} coming up${agentsIn(p.project_id) ? ` · ${plural(agentsIn(p.project_id), 'agent')} running` : ''}`} />
                    ))}
                    {clear.slice(0, CLEAR_SHOWN).map(r => (
                      <LinkRow key={r.id} to={projectLink(r.id, 'my-work')} title={r.name}
                        lead={<span className="amp-num text-[10.5px] text-ink-3">{r.code}</span>} sub={clearSub(r.id)} />
                    ))}
                    {clear.length > CLEAR_SHOWN && (
                      <li className="border-t border-line-sub">
                        <Link to="/projects" className="flex px-4 py-2.5 text-[12.5px] text-ink-3 hover:underline">
                          {plural(clear.length - CLEAR_SHOWN, 'more project')} with nothing for you ›
                        </Link>
                      </li>
                    )}
                  </ul>
                </div>
              </>}
            </section>

            <aside className="grid gap-4 min-w-0">
              <Panel label="Your agents" right={<span>{agentsTotal} running</span>}>
                {agentsByProject.size === 0
                  ? <p className="m-0 px-4 py-3.5 text-[13px] text-ink-2">Nothing running. When your harness dispatches one of your tickets, it shows up here.</p>
                  : <ul className="list-none m-0 p-0">{[...agentsByProject.values()].map(({ ref, n, quiet }) => (
                    <LinkRow key={ref.id} wide={false} to={projectLink(ref.id, 'board')} title={ref.name}
                      lead={<span className="amp-num text-[12px]" style={{ color: 'var(--st-prog)' }}>{n}</span>}
                      sub={quiet ? `${quiet} quiet for over ${Math.round(QUIET_MS / 60_000)}m, may have stalled` : undefined} />
                  ))}</ul>}
              </Panel>

              {remoteAvailable(me, remote) && <MySessionsPanel />}

              {expiring.length > 0 && (
                <Panel label="Artifacts" right={<span>{expiring.length} expiring soon</span>}>
                  <ul className="list-none m-0 p-0">{expiring.slice(0, 5).map(a => (
                    <LinkRow key={a.id} to={`/artifacts/${a.id}`} title={a.title} sub={`Expires ${expiresIn(a.expires_at)}${a.task_ref ? ` · ${a.task_ref}` : ''}`} />
                  ))}</ul>
                  {expiring.length > 5 && <p className="m-0 px-4 py-2 border-t border-line-sub text-[12px] text-ink-3">+{expiring.length - 5} more</p>}
                </Panel>
              )}

              <Panel label="Done in the last 24h" right={<span>{done24}</span>}>
                <p className="m-0 px-4 py-3 text-[13px] text-ink-2">{done24 ? `${plural(done24, 'ticket')} by you and your agents.` : 'Nothing finished yet.'}</p>
              </Panel>

              <Panel label="Velocity" right={<span>last 14 days</span>}>
                <div className="px-4 py-3">{velocity.length ? <Velocity days={velocity} /> : <p className="m-0 text-[13px] text-ink-2">No completions yet.</p>}</div>
              </Panel>

              <Panel label="Day report" right={<Link to="/reports" className="underline underline-offset-2 hover:text-ink">{draft?.saved ? 'view' : 'write'} ›</Link>}>
                {draft ? (
                  <div className="px-4 py-3 grid gap-2 text-[13px]">
                    <div className="flex flex-wrap gap-x-5 gap-y-1">
                      {([['completed', 'done'], ['started', 'started'], ['in_progress', 'in progress'], ['blocked', 'blocked']] as const).map(([k, l]) => (
                        <span key={k}><b className="amp-num text-[16px] font-normal text-ink">{draft.data.stats[k]}</b> <span className="text-ink-2">{l}</span></span>
                      ))}
                    </div>
                    {draft.summary && <p className="m-0 text-ink-2 leading-relaxed">{draft.summary}</p>}
                  </div>
                ) : <p className="m-0 px-4 py-3 text-[13px] text-ink-2">No activity yet today.</p>}
              </Panel>

              <p className="m-0 px-0.5 text-[12px] leading-normal text-ink-3">
                Nothing on this page is a button. To act, open the project: the item opens focused, with its action.
              </p>
            </aside>
          </div>
        </>}
      </div>
    </div>
  )
}
