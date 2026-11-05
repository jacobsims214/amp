/**
 * "Decided upstream": what people decided on the tickets this one depends on,
 * so whoever works it sees the answer without opening each dependency. Reads
 * GET /tasks/:id/decisions (v2/docs/specs/human-decision-docs.md). Shown on any
 * ticket that has one (the drawer remounts it per ticket); "Done by" for reasons that aren't decisions.
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../../api/client'
import type { UpstreamDecision } from '../../types'

const isDecision = (d: UpstreamDecision) => d.human_reason === 'judgment' || d.human_reason === 'approval'

export function DecidedUpstream({ taskId, hasDependencies, ticketHref }: {
  taskId: number
  hasDependencies: boolean
  ticketHref: (id: number) => string
}) {
  const [items, setItems] = useState<UpstreamDecision[]>([])
  useEffect(() => {
    if (!hasDependencies) return
    let live = true
    api.getTaskDecisions(taskId).then(r => { if (live) setItems(r.upstream) }).catch(() => {})
    return () => { live = false }
  }, [taskId, hasDependencies])
  if (!hasDependencies || items.length === 0) return null
  return (
    <div className="px-6 py-4 border-b border-line grid gap-2" data-testid="decided-upstream">
      <span className="amp-label">Decided upstream</span>
      <ul className="m-0 p-0 list-none grid gap-2">
        {items.map(d => (
          <li key={d.task_id} className="text-[13px] leading-relaxed text-ink-2" data-testid="decided-upstream-item">
            <span>{isDecision(d) ? 'Decided' : 'Done'} by {d.decided_by_name || 'someone'} on{' '}
              <Link to={ticketHref(d.task_id)} className="underline underline-offset-2 hover:text-ink">#{d.task_id}</Link>
              {d.option ? <>: <b className="text-ink">{d.option}</b> — </> : ': '}</span>
            <span className="text-ink whitespace-pre-line">{d.decision}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}
