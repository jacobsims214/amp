/**
 * Portfolio — an org's projects side by side for leads, finance and execs:
 * progress, open questions, and agent token spend. Nothing assumes a tracker
 * or a finance scheme: the columns are whatever metadata the projects carry
 * (jira_board, linear_team, ado_area_path, cost_center, …), and spend splits
 * by any ticket metadata key the viewer picks (capitalizable, cost_type, …).
 * Exports CSV with every key.
 */
import { useEffect, useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { Building2, Download } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { PageHeader, Panel, Readouts } from '../components/ui/PageHeader'
import { tokens, usd } from '../lib/usage'
import type { Portfolio as PortfolioData, PortfolioRow, Usage } from '../types'
import { BandLoader } from '../components/brand/BandLoader'

const RANGES = { '7d': 7, '30d': 30, '90d': 90, '365d': 365 } as const
type Range = keyof typeof RANGES

const tk = (u?: Usage) => (u ? u.input_tokens + u.output_tokens : 0)
const SPLIT_KEY = 'amp.portfolio.split'
const COLS_KEY = 'amp.portfolio.columns'
const ORG_KEY = 'amp.portfolio.org'
const load = (k: string) => { try { return localStorage.getItem(k) } catch { return null } }
const save = (k: string, v: string) => { try { localStorage.setItem(k, v) } catch { /* per-viewer preference only */ } }

function csv(pf: PortfolioData): string {
  const keys = pf.project_keys
  const vals = pf.split_values
  const head = ['project', 'code', ...keys, 'tickets', 'done', 'questions', 'tokens', 'cost_usd', 'runs', 'runs_without_usage', 'abandoned_runs',
    ...vals.flatMap(v => [`${pf.split_by}=${v} tickets`, `${pf.split_by}=${v} tokens`, `${pf.split_by}=${v} cost_usd`])]
  const row = (p: PortfolioRow) => [p.name, p.code, ...keys.map(k => p.metadata?.[k] ?? ''), p.tickets, p.states.done, p.states.flagged,
    tk(p.usage), p.usage.cost_usd.toFixed(4), p.usage.runs, p.usage.unreported, p.usage.abandoned ?? 0,
    ...vals.flatMap(v => { const g = p.split?.[v]; return [g?.tickets ?? 0, tk(g?.usage), (g?.usage.cost_usd ?? 0).toFixed(4)] })]
  const esc = (v: unknown) => /[",\n]/.test(String(v)) ? `"${String(v).replace(/"/g, '""')}"` : String(v)
  return [head, ...pf.projects.map(row), row(pf.total)].map(r => r.map(esc).join(',')).join('\n')
}

export function Portfolio() {
  const { me } = useAuth()
  const orgs = useMemo(() => me?.orgs ?? [], [me])
  // Which org: ?org= (shareable), else the one you last looked at, else your first.
  const [params, setParams] = useSearchParams()
  const pick = (id: number | null) => (id != null && orgs.some(o => o.id === id) ? id : null)
  const org = pick(Number(params.get('org')) || null) ?? pick(Number(load(ORG_KEY)) || null) ?? orgs[0]?.id ?? null
  const orgName = orgs.find(o => o.id === org)?.name
  const switchOrg = (id: number) => { save(ORG_KEY, String(id)); setParams(p => { p.set('org', String(id)); return p }, { replace: true }) }
  const [range, setRange] = useState<Range>('30d')
  const [splitBy, setSplitBy] = useState<string>(() => load(SPLIT_KEY) ?? 'capitalizable')
  const [hidden, setHidden] = useState<Set<string>>(() => new Set((load(COLS_KEY) ?? '').split(',').filter(Boolean)))
  const [now] = useState(Date.now)
  const since = useMemo(() => new Date(now - RANGES[range] * 86_400_000).toISOString(), [range, now])
  const [loaded, setLoaded] = useState<{ key: string; data: PortfolioData | null; error?: string } | null>(null)
  const key = `${org}:${since}:${splitBy}`
  useEffect(() => {
    if (org == null) return
    api.portfolio(org, since, splitBy).then(d => setLoaded({ key, data: d }))
      .catch(e => setLoaded({ key, data: null, error: e instanceof Error ? e.message : 'Failed to load' }))
  }, [org, since, splitBy, key])
  const pf = loaded?.key === key ? loaded.data : null
  const error = loaded?.key === key ? loaded.error : undefined
  const cols = (pf?.project_keys ?? []).filter(k => !hidden.has(k))
  const vals = pf?.split_values ?? []

  const pickSplit = (k: string) => { setSplitBy(k); save(SPLIT_KEY, k) }
  const toggleCol = (k: string) => {
    const next = new Set(hidden)
    if (next.has(k)) next.delete(k); else next.add(k)
    setHidden(next); save(COLS_KEY, [...next].join(','))
  }
  const download = () => {
    if (!pf) return
    const url = URL.createObjectURL(new Blob([csv(pf)], { type: 'text/csv' }))
    const a = document.createElement('a')
    a.href = url; a.download = `amp-portfolio-${(orgName ?? String(org)).toLowerCase().replace(/[^a-z0-9]+/g, '-')}-${range}.csv`; a.click()
    URL.revokeObjectURL(url)
  }

  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Overview · Portfolio" title={orgName ?? 'Portfolio'}
        meta={`Every project you can see in ${orgName ?? 'your org'}: progress, open questions, and agent spend, split by any metadata you track.`}
        actions={<>
          <div className="flex border border-line" role="group" aria-label="Range">
            {(Object.keys(RANGES) as Range[]).map(r => (
              <button key={r} type="button" onClick={() => setRange(r)} aria-pressed={range === r}
                className={`font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] px-2.5 py-2 border-l border-line first:border-l-0 cursor-pointer ${range === r ? 'bg-ink text-on-ink' : 'bg-surface text-ink-2 hover:text-ink'}`}>{r}</button>
            ))}
          </div>
          <button type="button" className="amp-btn" onClick={download} disabled={!pf}><Download size={12} />CSV</button>
        </>} />
      <div className="p-7 pt-5 grid gap-4">
        {orgs.length > 1 && (
          <div className="flex items-center gap-2 flex-wrap" role="tablist" aria-label="Organization">
            <span className="amp-label flex items-center gap-1.5"><Building2 size={12} />Org</span>
            {orgs.map(o => (
              <button key={o.id} type="button" role="tab" aria-selected={o.id === org} onClick={() => switchOrg(o.id)}
                className={`px-2.5 py-1.5 text-[12.5px] border cursor-pointer ${o.id === org ? 'bg-ink text-on-ink border-ink' : 'bg-surface text-ink-2 border-line hover:text-ink'}`}>
                {o.name}
              </button>
            ))}
          </div>
        )}
        {org == null && <div className="amp-panel px-4 py-4 text-[13px] text-ink-2">You're not in an organization yet.</div>}
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {org != null && !pf && !error && <div className="flex justify-center py-16"><BandLoader label="Loading the portfolio" /></div>}
        {pf && <>
          <Readouts items={[
            { label: 'Projects', value: pf.projects.length, sub: `${pf.total.tickets} tickets` },
            { label: 'Done', value: pf.total.completed, sub: `in the last ${RANGES[range]} days` },
            { label: 'Questions', value: pf.total.states.flagged, sub: 'waiting on a person', alarm: pf.total.states.flagged > 0 },
            { label: 'Agent spend', value: usd(pf.total.usage.cost_usd), sub: `${tokens(tk(pf.total.usage))} tokens · ${pf.total.usage.runs} runs${pf.total.usage.abandoned ? ` · ${pf.total.usage.abandoned} abandoned` : ''}` },
            ...vals.slice(0, 2).map(v => ({ label: `${pf.split_by}: ${v}`, value: usd(pf.total.split[v]?.usage.cost_usd ?? 0),
              sub: `${tokens(tk(pf.total.split[v]?.usage))} tokens · ${pf.total.split[v]?.tickets ?? 0} tickets` })),
          ]} />
          {pf.total.usage.unreported > 0 && (
            <p className="m-0 amp-panel px-4 py-3 text-[12.5px] text-ink-2">{pf.total.usage.unreported} of {pf.total.usage.runs} runs reported no token usage (their harness couldn't tell them), so spend is a floor, not a total.</p>
          )}
          <div className="amp-panel px-4 py-3 grid gap-2.5">
            <div className="flex items-center gap-2 flex-wrap">
              <span className="amp-label w-[110px]">Split spend by</span>
              {pf.ticket_keys.length === 0 ? <span className="text-[12.5px] text-ink-3">No metadata on tickets yet.</span> : pf.ticket_keys.map(k => (
                <button key={k.key} type="button" onClick={() => pickSplit(k.key)} aria-pressed={splitBy === k.key}
                  title={`${Math.round(k.coverage * 100)}% of tickets carry it`}
                  className={`amp-num text-[11.5px] px-2 py-1 border cursor-pointer ${splitBy === k.key ? 'border-ink text-ink shadow-[inset_0_-2px_0_var(--accent)]' : 'border-line text-ink-2 hover:border-line-str'}`}>
                  {k.key}</button>
              ))}
            </div>
            {pf.project_keys.length > 0 && (
              <div className="flex items-center gap-2 flex-wrap">
                <span className="amp-label w-[110px]">Columns</span>
                {pf.project_keys.map(k => (
                  <label key={k} className="amp-num text-[11.5px] inline-flex items-center gap-1.5 px-2 py-1 border border-line cursor-pointer">
                    <input type="checkbox" checked={!hidden.has(k)} onChange={() => toggleCol(k)} />{k}</label>
                ))}
              </div>
            )}
          </div>
          <Panel label="Projects" title="Side by side" right={<span>sorted by spend</span>}>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse text-[13px]">
                <thead><tr className="bg-surface-2">
                  {['Project', ...cols, 'Tickets', 'Done', 'Questions', 'Tokens', 'Spend', ...vals.map(v => `${pf.split_by}: ${v}`)].map((h, i) => (
                    <th key={h} className={`amp-label px-3 py-2.5 border-b border-line whitespace-nowrap ${i > cols.length ? 'text-right' : 'text-left'}`}>{h}</th>))}
                </tr></thead>
                <tbody>
                  {pf.projects.length === 0 && (
                    <tr><td colSpan={6 + cols.length + vals.length} className="px-3 py-6 text-center text-[13px] text-ink-2">
                      No projects you can see in {orgName ?? 'this org'} yet.
                    </td></tr>
                  )}
                  {pf.projects.map(p => (
                    <tr key={p.project_id}>
                      <td className="px-3 h-[38px] border-b border-line"><Link to={`/project/${p.project_id}/report`} className="font-semibold text-ink hover:underline">{p.name}</Link>{p.state === 'archived' && <span className="amp-num text-[11px] text-ink-3"> · archived</span>}</td>
                      {cols.map(k => <td key={k} className="px-3 border-b border-line amp-num">{p.metadata?.[k] != null ? String(p.metadata[k]) : '—'}</td>)}
                      <td className="px-3 border-b border-line amp-num text-right">{p.tickets}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{p.tickets ? Math.round(p.states.done / p.tickets * 100) : 0}%</td>
                      <td className="px-3 border-b border-line amp-num text-right" style={p.states.flagged ? { color: 'var(--danger-ink)' } : undefined}>{p.states.flagged}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{tokens(tk(p.usage))}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{usd(p.usage.cost_usd)}</td>
                      {vals.map(v => <td key={v} className="px-3 border-b border-line amp-num text-right" title={`${p.split?.[v]?.tickets ?? 0} tickets`}>{usd(p.split?.[v]?.usage.cost_usd ?? 0)}</td>)}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Panel>
          {pf.usage_by_model.length > 0 && (
            <Panel label="Models" title="Spend by model">
              <table className="w-full border-collapse text-[13px]">
                <thead><tr className="bg-surface-2">{['Model', 'Runs', 'Input', 'Output', 'Cost'].map((h, i) => <th key={h} className={`amp-label px-3 py-2.5 border-b border-line ${i ? 'text-right' : 'text-left'}`}>{h}</th>)}</tr></thead>
                <tbody>{pf.usage_by_model.map(m => (
                  <tr key={m.model}><td className="px-3 h-[38px] border-b border-line amp-num">{m.model}</td>
                    <td className="px-3 border-b border-line amp-num text-right">{m.runs}</td>
                    <td className="px-3 border-b border-line amp-num text-right">{tokens(m.input_tokens)}</td>
                    <td className="px-3 border-b border-line amp-num text-right">{tokens(m.output_tokens)}</td>
                    <td className="px-3 border-b border-line amp-num text-right">{usd(m.cost_usd)}</td></tr>
                ))}</tbody>
              </table>
            </Panel>
          )}
        </>}
      </div>
    </div>
  )
}
