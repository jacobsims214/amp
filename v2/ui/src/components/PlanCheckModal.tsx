import { useEffect, useState } from 'react'
import { AlertTriangle, CheckCircle2, Loader2, Wrench } from 'lucide-react'
import { api } from '../api/client'
import { CrudModal } from './CrudModal'
import type { PlanReport, Task } from '../types'

const ISSUE_TITLE: Record<string, string> = {
  isolated: 'Not connected to anything',
  wide_start: 'Too much can start at once',
  empty_stories: 'Empty stories',
}

// The plan check: AMP runs the dependency graph, so before dispatching it's
// worth seeing what's unconnected or able to start all at once. Runs on open
// (which also repairs tickets stuck blocked on finished dependencies).
export function PlanCheckModal({ projectId, tasks, onOpenTask, onClose }: {
  projectId: number
  tasks: Task[]
  onOpenTask: (taskId: number) => void
  onClose: () => void
}) {
  const [report, setReport] = useState<PlanReport | null>(null)
  const [error, setError] = useState<string | null>(null)
  const byId = new Map(tasks.map(t => [t.id, t]))

  useEffect(() => {
    api.planCheck(projectId).then(setReport).catch(e => setError(e instanceof Error ? e.message : 'Check failed'))
  }, [projectId])

  const chip = (id: number) => (
    <button key={id} onClick={() => onOpenTask(id)} title={byId.get(id)?.name}
      className="px-2 py-0.5 rounded-md text-xs text-[#DDE6F0] hover:bg-[#172540] cursor-pointer" style={{ border: '1px solid #1E2C45' }}>
      <span className="font-mono text-[#7E91A8]">#{id}</span> <span className="truncate">{byId.get(id)?.name ?? ''}</span>
    </button>
  )

  return (
    <CrudModal title="Plan check" onClose={onClose}>
      {error ? (
        <p className="text-xs text-[#F87171]">{error}</p>
      ) : !report ? (
        <div className="flex justify-center py-6"><Loader2 size={18} className="text-[#6366F1] animate-spin" /></div>
      ) : (
        <div className="space-y-4">
          <div className="flex items-start gap-2">
            {report.ok
              ? <CheckCircle2 size={15} className="text-[#10B981] mt-0.5 flex-shrink-0" />
              : <AlertTriangle size={15} className="text-[#FBBF24] mt-0.5 flex-shrink-0" />}
            <p className="text-sm text-[#DDE6F0] leading-relaxed">{report.summary}</p>
          </div>
          {report.issues.map((iss, i) => (
            <div key={i} className="rounded-xl p-3 space-y-2" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>
              <div className="text-xs font-semibold text-[#FBBF24]">{ISSUE_TITLE[iss.kind] ?? iss.kind}</div>
              <p className="text-xs text-[#7E91A8] leading-relaxed">{iss.message}</p>
              {iss.task_ids && iss.task_ids.length > 0 && (
                <div className="flex flex-wrap gap-1.5 max-h-32 overflow-y-auto">{iss.task_ids.map(chip)}</div>
              )}
              {iss.fix && <p className="text-xs text-[#DDE6F0] leading-relaxed">{iss.fix}</p>}
            </div>
          ))}
          {report.repaired_task_ids && report.repaired_task_ids.length > 0 && (
            <div className="rounded-xl p-3 space-y-2" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>
              <div className="flex items-center gap-1.5 text-xs font-semibold text-[#10B981]"><Wrench size={12} />Repaired</div>
              <p className="text-xs text-[#7E91A8]">These were stuck blocked although everything they wait on is done; they're ready now.</p>
              <div className="flex flex-wrap gap-1.5">{report.repaired_task_ids.map(chip)}</div>
            </div>
          )}
          <p className="text-[11px] text-[#3D5068]">
            Open a ticket to fix its dependencies or move it; changes apply immediately.
          </p>
        </div>
      )}
    </CrudModal>
  )
}
