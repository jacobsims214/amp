/**
 * "Decided upstream": what people decided on the tickets this one depends on,
 * so whoever works it sees the answer without opening each dependency. Reads
 * GET /tasks/:id/decisions (v2/docs/specs/human-decision-docs.md). Shown on any
 * ticket that has one (the drawer remounts it per ticket); "Done by" for reasons that aren't decisions.
 * It reloads on any decision.updated for one of its dependencies, so an open
 * drawer shows a decision recorded meanwhile.
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../../api/client'
import { useSSE } from '../../hooks/useSSE'
import { decisionAfterOption } from '../../lib/decisions'
import type { UpstreamDecision } from '../../types'

const isDecision = (d: UpstreamDecision) => d.human_reason === 'judgment' || d.human_reason === 'approval'

export function DecidedUpstream({ taskId, projectId, dependencyIds, ticketHref }: {
  taskId: number
  projectId: number
  dependencyIds: number[]
  ticketHref: (id: number) => string
}) {
  const [items, setItems] = useState<UpstreamDecision[]>([])
  const hasDependencies = dependencyIds.length > 0
  const [reloads, setReloads] = useState(0)
  useEffect(() => {
    if (!hasDependencies) return
    let live = true
    api.getTaskDecisions(taskId).then(r => { if (live) setItems(r.upstream) }).catch(() => {})
    return () => { live = false }
  }, [taskId, hasDependencies, reloads])
  useSSE(projectId, e => {
    const id = (e.payload as { task_id?: number } | undefined)?.task_id
    if (e.type === 'decision.updated' && id != null && dependencyIds.includes(id)) setReloads(n => n + 1)
  })
  if (!hasDependencies || items.length === 0) return null
  return (
    <div className="px-6 py-4 border-b border-line grid gap-2" data-testid="decided-upstream">
      <span className="amp-label">Decided upstream</span>
      <ul className="m-0 p-0 list-none grid gap-2">
        {items.map(d => {
          const rest = d.option ? decisionAfterOption(d.option, d.decision) : d.decision
          return (
            <li key={d.task_id} className="text-[13px] leading-relaxed text-ink-2" data-testid="decided-upstream-item">
              <span>{isDecision(d) ? 'Decided' : 'Done'} by {d.decided_by_name || 'someone'} on{' '}
                <Link to={ticketHref(d.task_id)} className="underline underline-offset-2 hover:text-ink">#{d.task_id}</Link>
                {d.option ? <>: <b className="text-ink">{d.option}</b>{rest ? ' — ' : ''}</> : ': '}</span>
              {rest && <span className="text-ink whitespace-pre-line">{rest}</span>}
            </li>
          )
        })}
      </ul>
    </div>
  )
}
