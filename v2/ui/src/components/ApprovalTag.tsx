/**
 * "Needs approval" on a ready ticket held back only because nobody has
 * approved its current plan (#129): the board's Ready column and the drawer's
 * header otherwise say READY for a ticket that can't be dispatched yet. Shown
 * only when the project requires approval (needsApproval).
 */
import { ShieldAlert } from 'lucide-react'
import type { Task } from '../types'
import { needsApproval } from '../lib/approval'

export function ApprovalTag({ task, requireApproval, compact = false }: { task: Task; requireApproval: boolean; compact?: boolean }) {
  if (!needsApproval(task, requireApproval)) return null
  return (
    <span data-testid="needs-approval"
      title="This project requires plan approval, and nobody has approved this ticket's plan yet. Its owner approves it in My work; until then it isn't dispatched."
      className={`inline-flex items-center gap-1 whitespace-nowrap border px-1.5 py-[3px] font-label font-semibold uppercase leading-none tracking-[0.1em] ${compact ? 'text-[10px]' : 'text-[10.5px]'}`}
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--warning)', color: 'var(--warning-ink)' }}>
      <ShieldAlert size={compact ? 10 : 11} aria-hidden="true" />Needs approval
    </span>
  )
}
