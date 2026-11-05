/**
 * Trace — from code back to the work. Paste a commit SHA, a branch, or a PR
 * URL; get the tickets whose agent runs produced it, and for each the story
 * of how it got there: the ticket as planned, the worker's comments, every
 * run (model, tokens, branch, commit), and the state changes.
 *
 * In the URL (v2/docs/url-state.md): `?q=<sha|branch|PR URL>` runs on load,
 * and `&open=<task id>[,<task id>]` expands those stories, so a pasted Trace
 * link shows what the sender saw.
 */
import { useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { Check, Link2, Loader2, Search } from 'lucide-react'
import { idListParam, useSearchParamState, usePatchSearchParams } from '../hooks/useSearchParamState'
import { api } from '../api/client'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { StateTag } from '../components/ui/StateTag'
import { Markdown } from '../components/ui/Markdown'
import { RunsPanel } from '../components/RunsPanel'
import { displayState } from '../lib/ticketState'
import type { ActivityLog, Comment, TraceHit } from '../types'

/** A pasted value as a trace query: a URL is a PR, 7–40 hex chars a commit, anything else a branch. */
function asQuery(v: string): { commit?: string; branch?: string; pr?: string } {
  const s = v.trim()
  if (/^https?:\/\//.test(s)) return { pr: s }
  if (/^[0-9a-f]{7,40}$/i.test(s)) return { commit: s }
  return { branch: s }
}

/** The absolute link that reopens this trace with one story expanded. */
function traceLink(q: string, openId?: number): string {
  const p = new URLSearchParams({ q })
  if (openId != null) p.set('open', String(openId))
  return `${window.location.origin}/trace?${p}`
}

function CopyLink({ url }: { url: string }) {
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard?.writeText(url).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    }).catch(() => { /* no clipboard (insecure context): nothing to show */ })
  }
  return (
    <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={copy} title={url}>
      {copied ? <Check size={11} /> : <Link2 size={11} />}{copied ? 'Copied' : 'Copy link'}
    </button>
  )
}

function Story({ hit, q, open, onToggle }: { hit: TraceHit; q: string; open: boolean; onToggle: () => void }) {
  const [history, setHistory] = useState<ActivityLog[] | null>(null)
  const [comments, setComments] = useState<Comment[]>([])
  // Load the story the first time it's open (on a click, or from ?open= on arrival).
  const id = hit.task.id
  const needed = open && history == null
  useEffect(() => {
    if (!needed) return
    let live = true
    Promise.all([api.getHistory(id), api.getComments(id)])
      .then(([h, c]) => { if (live) { setHistory(h); setComments(c) } })
      .catch(() => { if (live) setHistory([]) })
    return () => { live = false }
  }, [needed, id])
  const events = [
    ...(history ?? []).map(h => ({ key: `event-${h.id}`, at: h.created_at, who: h.actor, what: h.action.replace('_', ' '), body: h.detail })),
    ...comments.map(c => ({ key: `comment-${c.id}`, at: c.created_at, who: c.author, what: 'comment', body: c.body })),
  ].sort((a, b) => a.at.localeCompare(b.at))
  return (
    <Panel id={`trace-${hit.task.id}`} label={<Link to={`/t/${hit.ref}`} className="hover:underline underline-offset-2" title="This ticket's link">{hit.ref}</Link>} title={hit.task.name} right={<span>{hit.project}</span>} top>
      <div className="px-4 py-3 grid gap-3">
        <div className="flex items-center gap-3 flex-wrap text-[12.5px]">
          <StateTag state={displayState(hit.task)} />
          <span className="amp-num text-ink-2">{hit.runs.length} matching run{hit.runs.length === 1 ? '' : 's'}</span>
          <Link to={`/t/${hit.ref}`} className="amp-btn amp-btn-sm">Open ticket</Link>
          <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={onToggle} aria-expanded={open}>{open ? 'Hide' : 'Show'} how it got here</button>
          <CopyLink url={traceLink(q, hit.task.id)} />
        </div>
        {open && (
          <div className="grid gap-4">
            {hit.task.description && <div className="bg-surface-2 border border-line p-3"><span className="amp-label">As planned</span><div className="mt-2"><Markdown size="compact">{hit.task.description}</Markdown></div></div>}
            <RunsPanel taskId={hit.task.id} />
            <div>
              <span className="amp-label">Timeline</span>
              {!history ? <Loader2 size={14} className="animate-spin text-ink-3 mt-2" /> : (
                <ol className="m-0 mt-2 p-0 list-none border-l border-line">
                  {events.map(e => (
                    <li key={e.key} id={`trace-${hit.task.id}-${e.key}`} className="pl-4 pb-3 relative">
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
  const [params] = useSearchParams()
  const patch = usePatchSearchParams()
  const q = params.get('q')?.trim() ?? ''
  const [openIds, setOpenIds] = useSearchParamState('open', idListParam)
  const [value, setValue] = useState(q)
  // Follow the URL when it changes underneath (Back, a pasted link).
  const [shownQ, setShownQ] = useState(q)
  if (shownQ !== q) { setShownQ(q); setValue(q) }

  // The result for one query, so a stale answer never shows for a newer one.
  const [result, setResult] = useState<{ q: string; hits?: TraceHit[]; error?: string } | null>(null)
  const [retry, setRetry] = useState(0)
  useEffect(() => {
    if (!q) return
    let live = true
    api.trace(asQuery(q))
      .then(hits => { if (live) setResult({ q, hits }) })
      .catch(e => { if (live) setResult({ q, error: e instanceof Error ? e.message : 'Trace failed' }) })
    return () => { live = false }
  }, [q, retry])
  const current = result?.q === q ? result : null
  const busy = !!q && !current
  const hits = current?.hits ?? null
  const error = current?.error ?? null

  // A new query is a new page: push it, and drop the old one's open stories.
  const run = (v: string) => {
    const next = v.trim()
    if (!next) return
    if (next === q) { setRetry(n => n + 1); return }
    patch({ q: next, open: null }, { push: true })
  }
  const toggle = (id: number) =>
    setOpenIds(openIds.includes(id) ? openIds.filter(x => x !== id) : [...openIds, id], { push: true })
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
        {hits?.map(h => <Story key={h.task.id} hit={h} q={q} open={openIds.includes(h.task.id)} onToggle={() => toggle(h.task.id)} />)}
      </div>
    </div>
  )
}
