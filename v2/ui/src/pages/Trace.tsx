/**
 * Trace — from code back to the work. Paste a commit SHA, a branch, or a PR
 * URL; get the tickets whose agent runs produced it, and for each the story
 * of how it got there: the ticket as planned, the worker's comments, every
 * run (model, tokens, branch, commit), and the state changes.
 */
import { useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { Loader2, Search } from 'lucide-react'
import { api } from '../api/client'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { StateTag } from '../components/ui/StateTag'
import { Markdown } from '../components/ui/Markdown'
import { RunsPanel } from '../components/RunsPanel'
import { displayState } from '../lib/ticketState'
import type { ActivityLog, Comment, TraceHit } from '../types'
import { projectLink } from '../lib/projectLink'

/** A pasted value as a trace query: a URL is a PR, 7–40 hex chars a commit, anything else a branch. */
function asQuery(v: string): { commit?: string; branch?: string; pr?: string } {
  const s = v.trim()
  if (/^https?:\/\//.test(s)) return { pr: s }
  if (/^[0-9a-f]{7,40}$/i.test(s)) return { commit: s }
  return { branch: s }
}

function Story({ hit }: { hit: TraceHit }) {
  const [open, setOpen] = useState(false)
  const [history, setHistory] = useState<ActivityLog[] | null>(null)
  const [comments, setComments] = useState<Comment[]>([])
  const toggle = () => {
    setOpen(o => !o)
    if (!history) {
      Promise.all([api.getHistory(hit.task.id), api.getComments(hit.task.id)])
        .then(([h, c]) => { setHistory(h); setComments(c) }).catch(() => setHistory([]))
    }
  }
  const events = [
    ...(history ?? []).map(h => ({ at: h.created_at, who: h.actor, what: h.action.replace('_', ' '), body: h.detail })),
    ...comments.map(c => ({ at: c.created_at, who: c.author, what: 'comment', body: c.body })),
  ].sort((a, b) => a.at.localeCompare(b.at))
  return (
    <Panel label={hit.ref} title={hit.task.name} right={<span>{hit.project}</span>} top>
      <div className="px-4 py-3 grid gap-3">
        <div className="flex items-center gap-3 flex-wrap text-[12.5px]">
          <StateTag state={displayState(hit.task)} />
          <span className="amp-num text-ink-2">{hit.runs.length} matching run{hit.runs.length === 1 ? '' : 's'}</span>
          <Link to={projectLink(hit.task.project_id, 'mission', hit.task.id)} className="amp-btn amp-btn-sm">Open ticket</Link>
          <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={toggle} aria-expanded={open}>{open ? 'Hide' : 'Show'} how it got here</button>
        </div>
        {open && (
          <div className="grid gap-4">
            {hit.task.description && <div className="bg-surface-2 border border-line p-3"><span className="amp-label">As planned</span><div className="mt-2"><Markdown size="compact">{hit.task.description}</Markdown></div></div>}
            <RunsPanel taskId={hit.task.id} />
            <div>
              <span className="amp-label">Timeline</span>
              {!history ? <Loader2 size={14} className="animate-spin text-ink-3 mt-2" /> : (
                <ol className="m-0 mt-2 p-0 list-none border-l border-line">
                  {events.map((e, i) => (
                    <li key={i} className="pl-4 pb-3 relative">
                      <i className="absolute -left-[4px] top-1.5 w-[7px] h-[7px] bg-ink" aria-hidden="true" />
                      <div className="text-[12px]"><b className="font-semibold">{e.who}</b> <span className="text-ink-2">{e.what}</span> <span className="amp-num text-[11px] text-ink-3">{new Date(e.at).toLocaleString()}</span></div>
                      {e.body && <div className="text-[12.5px] text-ink-2 mt-1 whitespace-pre-wrap break-words">{e.body.length > 1200 ? e.body.slice(0, 1200) + '…' : e.body}</div>}
                    </li>
                  ))}
                </ol>
              )}
            </div>
          </div>
        )}
      </div>
    </Panel>
  )
}

export function Trace() {
  const [params, setParams] = useSearchParams()
  const [value, setValue] = useState(params.get('q') ?? '')
  const [hits, setHits] = useState<TraceHit[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const run = (q: string) => {
    if (!q.trim()) return
    setParams({ q: q.trim() }, { replace: true })
    setBusy(true); setError(null)
    api.trace(asQuery(q)).then(setHits).catch(e => setError(e instanceof Error ? e.message : 'Trace failed')).finally(() => setBusy(false))
  }
  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Overview" title="Trace" meta="From a commit, branch, or pull request back to the tickets, decisions, and agent runs that produced it." />
      <div className="p-7 pt-5 grid gap-4 max-w-5xl">
        <form className="flex gap-2" onSubmit={e => { e.preventDefault(); run(value) }}>
          <label className="sr-only" htmlFor="trace-q">Commit SHA, branch, or PR URL</label>
          <input id="trace-q" className="amp-input flex-1" placeholder="Commit SHA (7+ chars), branch name, or PR URL" value={value} onChange={e => setValue(e.target.value)} />
          <button type="submit" className="amp-btn amp-btn-primary" disabled={busy}>{busy ? <Loader2 size={12} className="animate-spin" /> : <Search size={12} />}Trace</button>
        </form>
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {hits && hits.length === 0 && <div className="amp-panel px-4 py-4 text-[13px] text-ink-2">No ticket recorded that. Agents record their branch, commit and PR when they complete a ticket; work from before that, or from outside AMP, won't trace.</div>}
        {hits?.map(h => <Story key={h.task.id} hit={h} />)}
      </div>
    </div>
  )
}
