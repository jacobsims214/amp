import { useEffect, useState } from 'react'
import { Bot, Check, GitBranch, GitMerge, Loader2, Paperclip, ShieldAlert, ShieldCheck, X } from 'lucide-react'
import { api } from '../../api/client'
import { EvidencePlayer } from './EvidencePlayer'
import { mergeStatusText } from '../../lib/humanActions'
import { agoText, isCloseout, latestRunWith, parseSelfCheck, STATUS_LABEL, STATUS_TONE, summarizeChecks } from '../../lib/receipt'
import { isHuman } from '../../lib/human'
import { safeUrl } from '../../lib/safeUrl'
import type { Comment, ReceiptCheck, ReceiptView, Run, Task, TaskReceipt } from '../../types'

interface Props {
  task: Task
  runs: Run[] | null
  comments: Comment[] | null
  /**
   * The ticket's server receipt (#126): undefined while it loads (or for a
   * server without receipts), receipt: null when it has none. Without one
   * the receipt is built from the runs and the "Work complete." comment.
   */
  receipt?: TaskReceipt | null
  /** a person who can work the ticket: Accept and Reject are theirs */
  canWork?: boolean
  /** after an Accept or Reject, so the drawer rereads the ticket and its receipt */
  onChanged?: () => void
}

const VERDICT_TONE: Record<ReceiptCheck['verdict'], string> = { pass: 'var(--success-ink)', fail: 'var(--danger-ink)', cant_tell: 'var(--warning-ink)' }
const VERDICT_LABEL: Record<ReceiptCheck['verdict'], string> = { pass: 'Pass', fail: 'Fail', cant_tell: "Can't tell" }
const ROLE_LABEL = { worker: 'Worker', verifier: 'Verifier', human: 'Person' } as const

/**
 * The completion receipt: who did a completed agent ticket, where its code
 * is, how it was checked and what status that earns. With a server receipt
 * (a project with receipts on) it lists the checks by role, the evidence,
 * earlier attempts and, for a person, Accept / Reject; a held receipt
 * (Require, below the minimum) leads with "Couldn't verify, needs you" and
 * shows on the in-progress ticket too. Otherwise it is the Phase 1 view,
 * built from the runs and the worker's "Work complete." comment and
 * labelled "from comment". Runs and comments load once in the drawer.
 * Mount it with key={`receipt-${task.id}`}: its state is per ticket.
 */
export function CompletionReceipt({ task, runs, comments, receipt, canWork = false, onChanged }: Props) {
  const [artifacts, setArtifacts] = useState(0)
  const latest = receipt && receipt.receipt ? (receipt as ReceiptView & { attempts?: number }) : null
  // A receipt from an earlier attempt isn't this completion's: the ticket
  // completed again later (e.g. after its project went Off), so it shows the
  // comment-based view instead. An accepted one is kept: Accept completes a
  // held ticket after its receipt was finalized.
  const server = latest && !staleReceipt(latest, task) ? latest : null
  const held = !!server?.held
  const show = !isHuman(task) && (task.state === 'completed' || held)
  useEffect(() => {
    if (!show) return
    let live = true
    api.listArtifacts(task.project_id, task.id).then(l => { if (live) setArtifacts(l.length) }).catch(() => {})
    return () => { live = false }
  }, [show, task.project_id, task.id])
  if (!show) return null

  const done = runs ? latestRunWith(runs, r => r.outcome === 'completed') : undefined
  const runCode = runs ? latestRunWith(runs) : undefined
  const code = server?.receipt.branch || server?.receipt.pr_url
    ? { branch: server.receipt.branch, commit: server.receipt.commit_sha, base_branch: server.receipt.base_branch, pr_url: server.receipt.pr_url }
    : runCode
  // An agent-supplied URL: only http(s) is ever a link (never javascript:).
  const prHref = code?.pr_url ? safeUrl(code.pr_url) : ''
  const merge = mergeStatusText(task)
  const when = done?.created_at ?? task.completed_at
  const who = [done?.agent_id || server?.receipt.agent_id || task.agent_id, done?.model].filter(Boolean).join(' · ')
  const attempts = server?.attempts ?? 0

  return (
    <section aria-label="Completion receipt" data-testid="completion-receipt" className="px-6 py-3.5 border-b border-line grid gap-1.5 text-[12.5px] amp-num"
      style={held
        ? { background: 'color-mix(in srgb, var(--warning) 8%, var(--surface))', boxShadow: 'inset 3px 0 0 var(--warning)' }
        : { background: 'color-mix(in srgb, var(--success) 5%, var(--surface))', boxShadow: 'inset 3px 0 0 var(--success)' }}>
      <span className="amp-label">Receipt</span>
      {held && (
        <div className="grid gap-1" data-testid="receipt-held">
          <span className="flex items-center gap-1.5 font-semibold" style={{ color: 'var(--warning-ink)' }}>
            <ShieldAlert size={13} aria-hidden="true" />Couldn't verify, needs you
          </span>
          {server?.hold_reason && <span className="text-ink-2">{server.hold_reason[0].toUpperCase() + server.hold_reason.slice(1)}.</span>}
          <span className="text-ink-3">Accept completes the ticket; Reject sends it back to ready with your note.</span>
        </div>
      )}
      <div className="flex items-center gap-1.5 text-ink" data-testid="receipt-done">
        <Bot size={12} aria-hidden="true" className="text-ink-3" />
        <span>
          {held ? 'Finished' : 'Done'}{who ? <> by <b className="font-semibold">{who}</b></> : null}{when ? ` · ${agoText(when)}` : ''}
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
          {prHref && <a href={prHref} target="_blank" rel="noreferrer" className="underline underline-offset-2 hover:text-ink">PR</a>}
          {merge && (
            <span className="inline-flex items-center gap-1" data-testid="merge-status"
              style={{ color: task.merge_state === 'merged' ? 'var(--success-ink)' : 'var(--teal)' }}>
              <GitMerge size={11} aria-hidden="true" />{merge}
            </span>
          )}
        </div>
      )}
      {server ? <ServerCheck view={server} /> : <CommentCheck comments={comments} />}
      {artifacts > 0 && (
        <div className="flex items-center gap-1.5 text-ink-2" data-testid="receipt-artifacts">
          <Paperclip size={12} aria-hidden="true" className="text-ink-3" />
          <span>{artifacts} artifact{artifacts === 1 ? '' : 's'}</span>
        </div>
      )}
      {server && <ChecksByRole checks={server.checks} />}
      {server && server.evidence.length > 0 && <EvidencePlayer evidence={server.evidence} />}
      {server && canWork && <Verdict taskId={task.id} held={held} accepted={server.status === 'human_accepted'} onChanged={onChanged} />}
      {attempts > 0 && (
        <div className="text-ink-3" data-testid="receipt-attempts">{attempts} earlier attempt{attempts === 1 ? '' : 's'}</div>
      )}
    </section>
  )
}

/** Slack between a completion and its receipt's finalize (they're written in that order). */
const STALE_SLACK_MS = 60_000

function staleReceipt(view: ReceiptView, task: Task): boolean {
  if (view.held || view.status === 'human_accepted' || task.state !== 'completed') return false
  const fin = view.receipt.finalized_at ? Date.parse(view.receipt.finalized_at) : NaN
  const done = task.completed_at ? Date.parse(task.completed_at) : NaN
  return Number.isFinite(fin) && Number.isFinite(done) && done - fin > STALE_SLACK_MS
}

/** Line 3 from a server receipt: the status and the worker's count, or the Reject. */
function ServerCheck({ view }: { view: ReceiptView }) {
  const sum = summarizeChecks(view.checks)
  const label = STATUS_LABEL[view.status]
  return (
    <div className="flex items-start gap-1.5 text-ink-2" data-testid="receipt-check">
      <ShieldCheck size={12} aria-hidden="true" className="text-ink-3 mt-[3px]" />
      <div className="min-w-0">
        {sum.total > 0 ? <span>Self-check {sum.pass}/{sum.total}</span> : <span>No checks recorded</span>}
        <span className="ml-2 font-semibold" style={{ color: STATUS_TONE[view.status] }} data-testid="receipt-status">
          {view.status === 'self_checked' && sum.total > 0 ? `${label} ${sum.pass}/${sum.total}` : label}
        </span>
        {sum.cantTell > 0 && (
          <span className="ml-2 text-warning-ink" data-testid="receipt-cant-tell">{sum.cantTell} couldn't tell</span>
        )}
        {sum.rejection && (
          <div className="mt-1 text-danger-ink" data-testid="receipt-rejected">
            Rejected by {sum.rejection.checked_by}{sum.rejection.note ? `: ${sum.rejection.note}` : ''}
          </div>
        )}
      </div>
    </div>
  )
}

/** Line 3 without a server receipt: the "Work complete." comment's self-check (Phase 1). */
function CommentCheck({ comments }: { comments: Comment[] | null }) {
  const check = comments ? parseSelfCheck(comments) : null
  return (
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
  )
}

/** The checks, grouped by who recorded them. */
function ChecksByRole({ checks }: { checks: ReceiptCheck[] }) {
  if (checks.length === 0) return null
  const { byRole } = summarizeChecks(checks)
  return (
    <div className="grid gap-1 mt-1" data-testid="receipt-checks">
      {(['worker', 'verifier', 'human'] as const).filter(r => byRole[r].length > 0).map(role => (
        <div key={role}>
          <span className="amp-label text-ink-3">{ROLE_LABEL[role]}</span>
          <ul className="m-0 pl-4">
            {byRole[role].map(c => (
              <li key={c.id} data-testid="receipt-check-row">
                <span className="font-semibold" style={{ color: VERDICT_TONE[c.verdict] }}>{VERDICT_LABEL[c.verdict]}</span>
                {c.criterion_ref ? <span className="text-ink-2"> · criterion {c.criterion_ref}</span> : null}
                {c.kind !== 'verdict' ? <span className="text-ink-3"> · {c.kind}</span> : null}
                {c.command ? <code className="ml-1 text-ink-2">{c.command}</code> : null}
                {c.role === 'human' ? <span className="text-ink-3"> · {c.checked_by}</span> : null}
                {c.note ? <span className="text-ink-2"> · {c.note}</span> : null}
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  )
}

/** Accept / Reject, for a person who can work the ticket. Reject needs a note. */
function Verdict({ taskId, held, accepted, onChanged }: { taskId: number; held: boolean; accepted: boolean; onChanged?: () => void }) {
  const [rejecting, setRejecting] = useState(false)
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState<'accept' | 'reject' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const act = async (kind: 'accept' | 'reject') => {
    setBusy(kind)
    setError(null)
    try {
      if (kind === 'accept') await api.acceptReceipt(taskId)
      else await api.rejectReceipt(taskId, note.trim())
      setRejecting(false)
      setNote('')
      onChanged?.()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'That failed')
    } finally {
      setBusy(null)
    }
  }
  if (rejecting) {
    return (
      <form className="grid gap-2 mt-1" data-testid="receipt-reject-form" onSubmit={e => { e.preventDefault(); if (note.trim()) act('reject') }}>
        <label htmlFor={`reject-note-${taskId}`} className="amp-label">What's wrong</label>
        <textarea id={`reject-note-${taskId}`} className="amp-input min-h-[56px] resize-y" value={note} onChange={e => setNote(e.target.value)}
          placeholder={held ? 'For whoever picks the ticket up next' : 'What the receipt gets wrong'} />
        {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
        <div className="flex gap-2">
          <button type="submit" className="amp-btn amp-btn-sm" disabled={!note.trim() || !!busy}>
            {busy === 'reject' ? <Loader2 size={12} className="animate-spin" aria-hidden="true" /> : <X size={12} aria-hidden="true" />}Reject</button>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={!!busy} onClick={() => setRejecting(false)}>Cancel</button>
        </div>
      </form>
    )
  }
  return (
    <div className="flex gap-2 flex-wrap items-center mt-1" data-testid="receipt-verdict">
      {!accepted && (
        <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" disabled={!!busy} onClick={() => act('accept')}>
          {busy === 'accept' ? <Loader2 size={12} className="animate-spin" aria-hidden="true" /> : <Check size={12} aria-hidden="true" />}Accept</button>
      )}
      <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={!!busy} onClick={() => setRejecting(true)}>
        <X size={12} aria-hidden="true" />Reject…</button>
      {error && <span role="alert" className="text-[12px] text-danger-ink w-full">{error}</span>}
    </div>
  )
}
