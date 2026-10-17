import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Run, Usage } from '../types'
import { tokens, usd } from '../lib/usage'

const OUTCOME: Record<Run['outcome'], string> = {
  completed: 'var(--st-done)', blocked: 'var(--st-flag)', progress: 'var(--st-prog)', abandoned: 'var(--ink-3)',
}

/**
 * A ticket's runs: which agent and model worked it, what each run spent
 * (when its harness could tell), and where the work went — the trail from
 * a ticket to its code, and back.
 */
export function RunsPanel({ taskId, refreshKey }: { taskId: number; refreshKey?: unknown }) {
  const [runs, setRuns] = useState<Run[] | null>(null)
  const [usage, setUsage] = useState<Usage | null>(null)
  useEffect(() => {
    api.taskRuns(taskId).then(r => { setRuns(r.runs); setUsage(r.usage) }).catch(() => setRuns([]))
  }, [taskId, refreshKey])

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
          {runs.map(r => (
            <div key={r.id} className="grid grid-cols-[auto_1fr_auto] gap-x-3 gap-y-1 px-3 py-2.5 border-t border-line first:border-t-0 text-[12.5px]">
              <i className="w-2 h-2 mt-1.5" style={{ background: OUTCOME[r.outcome] }} aria-hidden="true" />
              <span className="min-w-0">
                <b className="font-semibold text-ink">{r.outcome}</b>
                <span className="text-ink-2"> · {r.agent_id || 'agent'}{r.model ? ` · ${r.model}` : ''}</span>
              </span>
              <span className="amp-num text-[11px] text-ink-3">{new Date(r.created_at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })}</span>
              <span />
              <span className="col-span-2 amp-num text-[11px] text-ink-2 flex flex-wrap gap-x-3 gap-y-0.5">
                {(r.input_tokens != null || r.output_tokens != null) && <span>{tokens(r.input_tokens)} in · {tokens(r.output_tokens)} out{r.cache_read_tokens ? ` · ${tokens(r.cache_read_tokens)} cached` : ''}</span>}
                {r.cost_usd != null && <span>{usd(r.cost_usd)}</span>}
                {(r.branch || r.commit) && <span>{r.branch || '?'}{r.commit ? ` @ ${r.commit.slice(0, 9)}` : ''}{r.pushed === false ? ' · not pushed' : r.pushed ? ' · pushed' : ''}</span>}
                {r.pr_url && <a href={r.pr_url} target="_blank" rel="noreferrer" className="underline underline-offset-2 hover:text-ink">PR</a>}
              </span>
              {r.note && <><span /><span className="col-span-2 text-[12px] text-ink-2">{r.note}</span></>}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
