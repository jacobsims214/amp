import { useCallback, useEffect, useState } from 'react'
import { Check } from 'lucide-react'
import { api } from '../api/client'
import { Panel } from './ui/PageHeader'
import type { Run, Task } from '../types'

/**
 * Work the agents finished that waits on a person: completed tickets whose
 * pushed branch or PR nobody has reported merged. Marking one merged records
 * a run with merged=true, so it leaves the queue for everyone.
 */
export function MergeQueue({ projectId, tasks, onOpen, refreshKey }: {
  projectId: number; tasks: Task[]; onOpen: (id: number) => void; refreshKey?: unknown
}) {
  const [runs, setRuns] = useState<Run[]>([])
  const [error, setError] = useState<string | null>(null)
  const load = useCallback(() => { api.unmerged(projectId).then(setRuns).catch(() => {}) }, [projectId])
  useEffect(() => { load() }, [load, refreshKey])
  const byId = new Map(tasks.map(t => [t.id, t]))
  const markMerged = async (r: Run) => {
    setError(null)
    try {
      await api.recordRun(r.task_id, { outcome: 'completed', merged: true, branch: r.branch, commit: r.commit, pr_url: r.pr_url, note: 'merged (marked in AMP)' })
      load()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not mark it merged')
    }
  }
  if (runs.length === 0) return null
  return (
    <Panel label="Merge" title="Waiting on a person" right={<span>{runs.length} done, not merged</span>}>
      {error && <p className="m-0 px-4 py-2 text-[12px] text-danger-ink">{error}</p>}
      {runs.map(r => (
        <div key={r.id} className="grid grid-cols-[1fr_auto] gap-x-3 gap-y-0.5 items-center px-4 py-2.5 border-t border-line first:border-t-0 text-[12.5px]">
          <button type="button" onClick={() => onOpen(r.task_id)} className="min-w-0 text-left truncate hover:underline cursor-pointer">
            <span className="amp-num text-ink-3">#{r.task_id}</span> {byId.get(r.task_id)?.name ?? ''}
          </button>
          <button type="button" className="amp-btn amp-btn-sm" onClick={() => markMerged(r)} title="Record that this work is merged"><Check size={11} />Merged</button>
          <span className="col-span-2 amp-num text-[11px] text-ink-2 truncate">
            {r.branch || 'branch?'}{r.commit ? ` @ ${r.commit.slice(0, 9)}` : ''}{r.pushed === false ? ' · NOT pushed' : ''}
            {r.pr_url ? <> · <a href={r.pr_url} target="_blank" rel="noreferrer" className="underline">PR</a></> : null}
          </span>
        </div>
      ))}
    </Panel>
  )
}
