/**
 * Portfolio — an org's projects side by side for leads, finance and execs:
 * progress, open questions, and agent token spend split by the
 * capitalizable metadata (capex / opex / unclassified), with the Jira board
 * and cost centre each project rolls up to. Exports CSV for finance.
 */
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Download, Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { PageHeader, Panel, Readouts } from '../components/ui/PageHeader'
import { tokens, usd } from '../lib/usage'
import type { Portfolio as PortfolioData, PortfolioRow, Usage } from '../types'

const RANGES = { '7d': 7, '30d': 30, '90d': 90, '365d': 365 } as const
type Range = keyof typeof RANGES

const tk = (u?: Usage) => (u ? u.input_tokens + u.output_tokens : 0)

function csv(pf: PortfolioData): string {
  const head = ['project', 'code', 'jira_board', 'cost_center', 'tickets', 'done', 'questions', 'tokens', 'cost_usd',
    'capex_tokens', 'capex_usd', 'opex_tokens', 'opex_usd', 'unclassified_tokens', 'unclassified_usd', 'runs', 'runs_without_usage']
  const row = (p: PortfolioRow) => [p.name, p.code, p.metadata?.jira_board ?? '', p.metadata?.cost_center ?? '', p.tickets, p.states.done,
    p.states.flagged, tk(p.usage), p.usage.cost_usd.toFixed(4), tk(p.capex), p.capex.cost_usd.toFixed(4), tk(p.opex), p.opex.cost_usd.toFixed(4),
    tk(p.unclassified), p.unclassified.cost_usd.toFixed(4), p.usage.runs, p.usage.unreported]
  const esc = (v: unknown) => /[",\n]/.test(String(v)) ? `"${String(v).replace(/"/g, '""')}"` : String(v)
  return [head, ...pf.projects.map(row), row(pf.total)].map(r => r.map(esc).join(',')).join('\n')
}

export function Portfolio() {
  const { me } = useAuth()
  const orgs = me?.orgs ?? []
  const [orgId, setOrgId] = useState<number | null>(null)
  const org = orgId ?? orgs[0]?.id ?? null
  const [range, setRange] = useState<Range>('30d')
  const [now] = useState(Date.now)
  const since = useMemo(() => new Date(now - RANGES[range] * 86_400_000).toISOString(), [range, now])
  const [loaded, setLoaded] = useState<{ key: string; data: PortfolioData | null; error?: string } | null>(null)
  const key = `${org}:${since}`
  useEffect(() => {
    if (org == null) return
    api.portfolio(org, since).then(d => setLoaded({ key, data: d })).catch(e => setLoaded({ key, data: null, error: e instanceof Error ? e.message : 'Failed to load' }))
  }, [org, since, key])
  const pf = loaded?.key === key ? loaded.data : null
  const error = loaded?.key === key ? loaded.error : undefined

  const download = () => {
    if (!pf) return
    const url = URL.createObjectURL(new Blob([csv(pf)], { type: 'text/csv' }))
    const a = document.createElement('a')
    a.href = url; a.download = `amp-portfolio-${org}-${range}.csv`; a.click()
    URL.revokeObjectURL(url)
  }

  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Overview" title="Portfolio" meta="Every project in the org: progress, open questions, and agent spend split capex / opex."
        actions={<>
          {orgs.length > 1 && (
            <select className="amp-input !w-auto" value={org ?? ''} onChange={e => setOrgId(Number(e.target.value))} aria-label="Organization">
              {orgs.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
            </select>
          )}
          <div className="flex border border-line" role="group" aria-label="Range">
            {(Object.keys(RANGES) as Range[]).map(r => (
              <button key={r} type="button" onClick={() => setRange(r)} aria-pressed={range === r}
                className={`font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] px-2.5 py-2 border-l border-line first:border-l-0 cursor-pointer ${range === r ? 'bg-ink text-on-ink' : 'bg-surface text-ink-2 hover:text-ink'}`}>{r}</button>
            ))}
          </div>
          <button type="button" className="amp-btn" onClick={download} disabled={!pf}><Download size={12} />CSV</button>
        </>} />
      <div className="p-7 pt-5 grid gap-4">
        {org == null && <div className="amp-panel px-4 py-4 text-[13px] text-ink-2">You're not in an organization yet.</div>}
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {org != null && !pf && !error && <div className="flex justify-center py-16"><Loader2 size={20} className="animate-spin text-ink-3" /></div>}
        {pf && <>
          <Readouts items={[
            { label: 'Projects', value: pf.projects.length, sub: `${pf.total.tickets} tickets` },
            { label: 'Done', value: pf.total.completed, sub: `in the last ${RANGES[range]} days` },
            { label: 'Questions', value: pf.total.states.flagged, sub: 'waiting on a person', alarm: pf.total.states.flagged > 0 },
            { label: 'Agent spend', value: usd(pf.total.usage.cost_usd), sub: `${tokens(tk(pf.total.usage))} tokens · ${pf.total.usage.runs} runs` },
            { label: 'Capex', value: usd(pf.total.capex.cost_usd), sub: `${tokens(tk(pf.total.capex))} tokens · ${pf.total.capex_tickets} tickets` },
            { label: 'Opex', value: usd(pf.total.opex.cost_usd), sub: `${tokens(tk(pf.total.opex))} tokens · ${pf.total.opex_tickets} tickets` },
          ]} />
          {pf.total.usage.unreported > 0 && (
            <p className="m-0 amp-panel px-4 py-3 text-[12.5px] text-ink-2">{pf.total.usage.unreported} of {pf.total.usage.runs} runs reported no token usage (their harness couldn't tell them), so spend is a floor, not a total.</p>
          )}
          <Panel label="Projects" title="Side by side" right={<span>sorted by spend</span>}>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse text-[13px]">
                <thead><tr className="bg-surface-2">
                  {['Project', 'Jira', 'Cost centre', 'Tickets', 'Done', 'Questions', 'Tokens', 'Spend', 'Capex', 'Opex', 'Unclassified'].map((h, i) => (
                    <th key={h} className={`amp-label px-3 py-2.5 border-b border-line whitespace-nowrap ${i > 2 ? 'text-right' : 'text-left'}`}>{h}</th>))}
                </tr></thead>
                <tbody>
                  {pf.projects.map(p => (
                    <tr key={p.project_id}>
                      <td className="px-3 h-[38px] border-b border-line"><Link to={`/project/${p.project_id}/report`} className="font-semibold text-ink hover:underline">{p.name}</Link>{p.state === 'archived' && <span className="amp-num text-[11px] text-ink-3"> · archived</span>}</td>
                      <td className="px-3 border-b border-line amp-num">{String(p.metadata?.jira_board ?? '—')}</td>
                      <td className="px-3 border-b border-line amp-num">{String(p.metadata?.cost_center ?? '—')}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{p.tickets}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{p.tickets ? Math.round(p.states.done / p.tickets * 100) : 0}%</td>
                      <td className="px-3 border-b border-line amp-num text-right" style={p.states.flagged ? { color: 'var(--danger-ink)' } : undefined}>{p.states.flagged}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{tokens(tk(p.usage))}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{usd(p.usage.cost_usd)}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{usd(p.capex.cost_usd)}</td>
                      <td className="px-3 border-b border-line amp-num text-right">{usd(p.opex.cost_usd)}</td>
                      <td className="px-3 border-b border-line amp-num text-right" title={p.unclassified_tickets ? `${p.unclassified_tickets} tickets have no capitalizable value` : undefined}>{usd(p.unclassified.cost_usd)}</td>
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
