import { useEffect, useRef, useState } from 'react'
import { Check, Link2 } from 'lucide-react'
import { api } from '../api/client'
import type { Run, Usage } from '../types'
import { tokens, usd } from '../lib/usage'
import { commitURL, sessionPath } from '../lib/runLinks'
import { runPath } from '../lib/ticketRef'
import { Markdown } from './ui/Markdown'

const OUTCOME: Record<Run['outcome'], string> = {
  completed: 'var(--st-done)', blocked: 'var(--st-flag)', progress: 'var(--st-prog)', abandoned: 'var(--ink-3)',
}

/**
 * A ticket's runs: which agent and model worked it, what each run spent
 * (when its harness could tell), and where the work went — the trail from
 * a ticket to its code, and back.
 *
 * Each row is addressable: `id="run-<id>"`, and `focusRunId` (the ticket
 * URL's `?run=`) scrolls to that row and highlights it. Copy link copies the
 * run's canonical link, `/r/<id>`, which reopens this view.
 */
export function RunsPanel({ taskId, refreshKey, focusRunId = null }: { taskId: number; refreshKey?: unknown; focusRunId?: number | null }) {
  const [runs, setRuns] = useState<Run[] | null>(null)
  const [usage, setUsage] = useState<Usage | null>(null)
  const [copied, setCopied] = useState<number | null>(null)
  const scrolledTo = useRef<string | null>(null)
  useEffect(() => {
    api.taskRuns(taskId).then(r => { setRuns(r.runs); setUsage(r.usage) }).catch(() => setRuns([]))
  }, [taskId, refreshKey])

  // Scroll to the linked run once its row is on screen (once per ticket and run).
  useEffect(() => {
    if (focusRunId == null || !runs?.some(r => r.id === focusRunId)) return
    const key = `${taskId}:${focusRunId}`
    if (scrolledTo.current === key) return
    scrolledTo.current = key
    document.getElementById(`run-${focusRunId}`)?.scrollIntoView?.({ block: 'center' })
  }, [runs, focusRunId, taskId])

  useEffect(() => {
    if (copied == null) return
    const t = setTimeout(() => setCopied(null), 1500)
    return () => clearTimeout(t)
  }, [copied])

  const copy = (id: number) => {
    navigator.clipboard?.writeText(`${window.location.origin}${runPath(id)}`).then(() => setCopied(id)).catch(() => {})
  }

  if (runs == null) return null
  return (
    <div>
      <div className="flex items-baseline gap-3 mb-2">
        <span className="amp-label">Runs</span>
        {usage && usage.runs > 0 && (
          <span className="amp-num text-[11px] text-ink-3">
            {tokens(usage.input_tokens)} in · {tokens(usage.output_tokens)} out{usage.cost_usd > 0 ? ` · ${usd(usage.cost_usd)}` : ''}
            {usage.unreported > 0 ? ` · ${usage.unreported} without usage` : ''}
          </span>
        )}
      </div>
      {runs.length === 0 ? (
        <p className="m-0 text-[12.5px] text-ink-3">No runs recorded. Agents record their model, token usage and branch/commit when they complete or stop.</p>
      ) : (
        <div className="border border-line">
          {runs.map(r => {
            const focused = r.id === focusRunId
            const commit = commitURL(r)
            const session = sessionPath(r)
            return (
            <div key={r.id} id={`run-${r.id}`} data-focused={focused || undefined} aria-current={focused ? 'true' : undefined}
              className={`grid grid-cols-[auto_1fr_auto] gap-x-3 gap-y-1 px-3 py-2.5 border-t border-line first:border-t-0 text-[12.5px] ${focused ? 'bg-surface-2' : ''}`}
              style={focused ? { boxShadow: 'inset 3px 0 0 var(--primary)' } : undefined}>
              <i className="w-2 h-2 mt-1.5" style={{ background: OUTCOME[r.outcome] }} aria-hidden="true" />
              <span className="min-w-0">
                <b className="font-semibold text-ink">{r.outcome}</b>
                <span className="text-ink-2"> · {r.agent_id || 'agent'}{r.model ? ` · ${r.model}` : ''}</span>
              </span>
              <span className="flex items-center gap-1.5">
                <span className="amp-num text-[11px] text-ink-3">{new Date(r.created_at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })}</span>
                <button type="button" onClick={() => copy(r.id)} className="amp-btn amp-btn-ghost amp-btn-sm !px-1"
                  title={copied === r.id ? 'Copied' : 'Copy a link to this run'} aria-label={`Copy link to run ${r.id}`}>
                  {copied === r.id ? <Check size={11} className="text-success-ink" aria-hidden="true" /> : <Link2 size={11} className="text-ink-3" aria-hidden="true" />}
                </button>
              </span>
              <span />
              <span className="col-span-2 amp-num text-[11px] text-ink-2 flex flex-wrap gap-x-3 gap-y-0.5">
                {(r.input_tokens != null || r.output_tokens != null) && <span>{tokens(r.input_tokens)} in · {tokens(r.output_tokens)} out{r.cache_read_tokens ? ` · ${tokens(r.cache_read_tokens)} cached` : ''}</span>}
                {r.cost_usd != null && <span>{usd(r.cost_usd)}</span>}
                {(r.branch || r.commit) && <span>{r.branch || '?'}{r.commit ? <> @ {commit
                  ? <a href={commit} target="_blank" rel="noreferrer" className="underline underline-offset-2 hover:text-ink">{r.commit.slice(0, 9)}</a>
                  : r.commit.slice(0, 9)}</> : ''}{r.pushed === false ? ' · not pushed' : r.pushed ? ' · pushed' : ''}</span>}
                {r.merged && <span className="text-success-ink">merged</span>}
                {r.pr_url && <a href={r.pr_url} target="_blank" rel="noreferrer" className="underline underline-offset-2 hover:text-ink">PR</a>}
                {/* /sessions/ is its own page (strict CSP, no sidebar): a full page load. */}
                {session && <a href={session} className="underline underline-offset-2 hover:text-ink">Open session</a>}
              </span>
              {/* The run's note is the agent's own summary, written as markdown. */}
              {r.note && <><span /><div className="col-span-2 min-w-0" data-testid="run-note"><Markdown size="compact">{r.note}</Markdown></div></>}
            </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
