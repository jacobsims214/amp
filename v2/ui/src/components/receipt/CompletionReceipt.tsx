import { useEffect, useState } from 'react'
import { Bot, GitBranch, GitMerge, ShieldCheck, Paperclip } from 'lucide-react'
import { api } from '../../api/client'
import { mergeStatusText } from '../../lib/humanActions'
import { agoText, isCloseout, latestRunWith, parseSelfCheck } from '../../lib/receipt'
import { isHuman } from '../../lib/human'
import type { Comment, Run, Task } from '../../types'

/**
 * Receipt v0: who did a completed agent ticket, where its code is, how it was
 * checked and what status that earns, built from the runs and the worker's
 * "Work complete." comment. Runs and comments load once in the drawer.
 * Mount it with key={task.id}: the artifact count is per ticket.
 */
export function CompletionReceipt({ task, runs, comments }: { task: Task; runs: Run[] | null; comments: Comment[] | null }) {
  const [artifacts, setArtifacts] = useState(0)
  const show = task.state === 'completed' && !isHuman(task)
  useEffect(() => {
    if (!show) return
    let live = true
    api.listArtifacts(task.project_id, task.id).then(l => { if (live) setArtifacts(l.length) }).catch(() => {})
    return () => { live = false }
  }, [show, task.project_id, task.id])
  if (!show) return null

  const done = runs ? latestRunWith(runs, r => r.outcome === 'completed') : undefined
  const code = runs ? latestRunWith(runs) : undefined
  const merge = mergeStatusText(task)
  const check = comments ? parseSelfCheck(comments) : null
  const when = done?.created_at ?? task.completed_at
  const who = [done?.agent_id || task.agent_id, done?.model].filter(Boolean).join(' · ')

  return (
    <section aria-label="Completion receipt" data-testid="completion-receipt" className="px-6 py-3.5 border-b border-line grid gap-1.5 text-[12.5px] amp-num"
      style={{ background: 'color-mix(in srgb, var(--success) 5%, var(--surface))', boxShadow: 'inset 3px 0 0 var(--success)' }}>
      <span className="amp-label">Receipt</span>
      <div className="flex items-center gap-1.5 text-ink" data-testid="receipt-done">
        <Bot size={12} aria-hidden="true" className="text-ink-3" />
        <span>
          Done{who ? <> by <b className="font-semibold">{who}</b></> : null}{when ? ` · ${agoText(when)}` : ''}
        </span>
        {isCloseout(done) && (
          <span className="border border-line px-1.5 py-[2px] font-label text-[10px] font-semibold uppercase tracking-[0.1em] text-warning-ink" title="Completed by someone other than the dispatched worker">Closeout</span>
        )}
      </div>
      {(code || merge) && (
        <div className="flex items-center gap-1.5 flex-wrap text-ink-2" data-testid="receipt-code">
          <GitBranch size={12} aria-hidden="true" className="text-ink-3" />
          {code?.branch && (
            <span>{code.branch}{code.commit ? ` @ ${code.commit.slice(0, 7)}` : ''}{code.base_branch ? ` → ${code.base_branch}` : ''}</span>
          )}
          {code?.pr_url && <a href={code.pr_url} target="_blank" rel="noreferrer" className="underline underline-offset-2 hover:text-ink">PR</a>}
          {merge && (
            <span className="inline-flex items-center gap-1" data-testid="merge-status"
              style={{ color: task.merge_state === 'merged' ? 'var(--success-ink)' : 'var(--teal)' }}>
              <GitMerge size={11} aria-hidden="true" />{merge}
            </span>
          )}
        </div>
      )}
      <div className="flex items-start gap-1.5 text-ink-2" data-testid="receipt-check">
        <ShieldCheck size={12} aria-hidden="true" className="text-ink-3 mt-[3px]" />
        {comments == null ? <span className="text-ink-3">Loading…</span> : check ? (
          <div className="min-w-0">
            <span>Self-checked (from comment) {check.verified}/{check.total}</span>
            <span className="ml-2 text-ink-3" data-testid="receipt-status">Unverified</span>
            {check.unverified.length > 0 && (
              <ul className="m-0 mt-1 pl-4 text-warning-ink" data-testid="receipt-unverified">
                {check.unverified.map((c, i) => <li key={i}>{c.criterion}{c.why ? `: ${c.why}` : ''}</li>)}
              </ul>
            )}
          </div>
        ) : (
          <span><span>No self-check posted</span><span className="ml-2 text-ink-3" data-testid="receipt-status">Unverified</span></span>
        )}
      </div>
      {artifacts > 0 && (
        <div className="flex items-center gap-1.5 text-ink-2" data-testid="receipt-artifacts">
          <Paperclip size={12} aria-hidden="true" className="text-ink-3" />
          <span>{artifacts} artifact{artifacts === 1 ? '' : 's'}</span>
        </div>
      )}
    </section>
  )
}
