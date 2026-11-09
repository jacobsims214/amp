import { useEffect, useState } from 'react'
import { AlertTriangle, CheckCircle2, Loader2, Wrench } from 'lucide-react'
import { api } from '../api/client'
import { CrudModal } from './CrudModal'
import type { PlanReport, Task } from '../types'
import { Markdown } from './ui/Markdown'

const ISSUE_TITLE: Record<string, string> = {
  isolated: 'Not connected to anything',
  wide_start: 'Too much can start at once',
  empty_stories: 'Empty stories',
  no_context: 'No project context in the KB',
  coupled_gate: "One ticket joins different people's work",
  metadata_drift: "Epics' metadata doesn't match",
  awaiting_approval: 'Waiting for plan approval',
  human_without_owner: "A person's ticket has no owner",
  human_without_brief: "A person's ticket has no brief",
  judgment_without_doc: 'A decision has no decision doc',
  unlinked_merge_step: 'Merge step not linked to its PR',
  duplicate_merge_step: 'Two merge steps for one PR',
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
      className="px-2 py-0.5 rounded-md text-xs text-ink hover:bg-surface-3 cursor-pointer" style={{ border: '1px solid var(--line)' }}>
      <span className="font-mono text-ink-2">#{id}</span> <span className="truncate">{byId.get(id)?.name ?? ''}</span>
    </button>
  )

  return (
    <CrudModal title="Plan check" onClose={onClose}>
      {error ? (
        <p className="text-xs text-danger-ink">{error}</p>
      ) : !report ? (
        <div className="flex justify-center py-6"><Loader2 size={18} className="text-primary animate-spin" /></div>
      ) : (
        <div className="space-y-4">
          <div className="flex items-start gap-2">
            {report.ok
              ? <CheckCircle2 size={15} className="text-success mt-0.5 flex-shrink-0" />
              : <AlertTriangle size={15} className="text-warning-ink mt-0.5 flex-shrink-0" />}
            <div className="min-w-0" data-testid="plan-summary"><Markdown size="compact" projectId={projectId}>{report.summary}</Markdown></div>
          </div>
          {report.issues.map((iss, i) => (
            <div key={i} className="rounded-xl p-3 space-y-2" style={{ background: 'var(--bg)', border: '1px solid var(--line)' }}>
              <div className={`text-xs font-semibold ${iss.severity === 'warn' ? 'text-warning-ink' : 'text-danger-ink'}`}>
                {ISSUE_TITLE[iss.kind] ?? iss.kind.replace(/_/g, ' ')}
                <span className="ml-1.5 font-label text-[10px] tracking-[0.12em]">{iss.severity === 'warn' ? 'WARN' : 'NO-GO'}</span>
              </div>
              <Markdown size="compact" projectId={projectId}>{iss.message}</Markdown>
              {iss.task_ids && iss.task_ids.length > 0 && (
                <div className="flex flex-wrap gap-1.5 max-h-32 overflow-y-auto">{iss.task_ids.map(chip)}</div>
              )}
              {iss.fix && <p className="text-xs text-ink leading-relaxed">{iss.fix}</p>}
            </div>
          ))}
          {report.repaired_task_ids && report.repaired_task_ids.length > 0 && (
            <div className="rounded-xl p-3 space-y-2" style={{ background: 'var(--bg)', border: '1px solid var(--line)' }}>
              <div className="flex items-center gap-1.5 text-xs font-semibold text-success"><Wrench size={12} />Repaired</div>
              <p className="text-xs text-ink-2">These were stuck blocked although everything they wait on is done; they're ready now.</p>
              <div className="flex flex-wrap gap-1.5">{report.repaired_task_ids.map(chip)}</div>
            </div>
          )}
          <p className="text-[11px] text-ink-3">
            Open a ticket to fix its dependencies or move it; changes apply immediately.
          </p>
        </div>
      )}
    </CrudModal>
  )
}
