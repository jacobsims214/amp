/**
 * The pieces of the remote session page. They show what the harness TUI shows, in the same
 * order and words (amp-tui transcript.rs, view.rs, roster.rs, chrome.rs), drawn with AMP's
 * web design system rather than as a terminal:
 *
 * - user turns as bubbles, a remote turn tagged with its device (the TUI's `[remote: …]`);
 * - assistant text as sanitised Markdown, thinking folded as "Thought for 9s";
 * - tool calls as one line each (`✓ get_task #514 · Card form · in progress · 11 ms`), grouped,
 *   long groups folding their earliest successful calls into `✓ 4 calls · …`, a failed call's
 *   error on its line, and a dispatched worker's live status under its call;
 * - the shell's notices ("Stopped.", errors) as it phrases them; nothing for a completed run;
 * - the dock: worker chips, the working line with its timer (and the retry countdown), the
 *   queue, the message box, Stop, and the status line.
 *
 * Everything from the harness is untrusted: text is shown as text or through SafeMarkdown,
 * never as HTML.
 */
import { useState } from 'react'
import { Check, ChevronRight, Loader2, OctagonX, ShieldAlert, Square, TriangleAlert, X } from 'lucide-react'
import { SafeMarkdown } from './SafeMarkdown'
import { promptProblem } from '../../remote/envelope'
import { formatCost, sessionTokens, type Block, type Outgoing, type Totals, type TranscriptItem } from '../../remote/session'
import {
  WORKER_TOOLS, clean, formatElapsed, formatSpan, latestSentence, parseArgs, resultDetail, resultSummary,
  runningElapsed, shortModel, shortRole, ticketIds, tokens as tokenCount, toolArgs, toolTitle,
} from '../../remote/toolText'
import {
  ampConnectionWord, compactionLabel, displayStateOf, epicStory, errorDetail, errorLine, planApprovalText, thinkingLabel,
  workerLine, workerState, workerUsage,
} from '../../remote/transcriptView'
import type { QueueItem, SessionStatus, WorkerInfo } from '../../remote/types'
import { StateDot, StateTag } from '../ui/StateTag'
import { STATE_LABEL } from '../../lib/ticketState'

type Tool = Extract<TranscriptItem, { kind: 'tool' }>
type Assistant = Extract<TranscriptItem, { kind: 'assistant' }>

const formatBytes = (n: number) => n < 1024 ? `${n} B` : n < 1_048_576 ? `${Math.round(n / 1024)} KB` : `${(n / 1_048_576).toFixed(1)} MB`

// ── User and assistant turns ────────────────────────────────────────────────

function UserMessage({ item, myDeviceId }: { item: Extract<TranscriptItem, { kind: 'user' }>; myDeviceId: string | null }) {
  const remote = item.origin === 'remote'
  const mine = remote && item.device_id != null && item.device_id === myDeviceId
  return (
    <div className="flex flex-col items-end" data-testid="user-message">
      {remote && (
        <span className="mb-1 inline-flex items-center gap-1 border border-line-str bg-surface px-1.5 py-px text-[10.5px] font-semibold text-ink-2" data-testid="remote-tag">
          <span className="amp-label !text-[9.5px]">Remote</span>{item.device_name ?? 'a paired device'}{mine ? ' (this device)' : ''}
        </span>
      )}
      <div className="max-w-[min(85%,680px)] bg-ink text-on-ink px-3 py-2 text-[14px] leading-6 whitespace-pre-wrap break-words">{item.text}</div>
      {!remote && <span className="text-[10.5px] text-ink-3 mt-1">typed on the laptop</span>}
    </div>
  )
}

function Thinking({ block, now }: { block: Block; now: number }) {
  const live = block.endedAt == null
  return (
    <details className="group text-[13px]" data-testid="thinking">
      <summary className="inline-flex items-center gap-1.5 cursor-pointer select-none list-none text-ink-3 hover:text-ink-2 max-w-full">
        <ChevronRight size={13} className="shrink-0 transition-transform group-open:rotate-90" aria-hidden="true" />
        <span className="italic shrink-0">{thinkingLabel(block, now)}</span>
        {live && <span className="italic truncate min-w-0 group-open:hidden">{latestSentence(block.text)}</span>}
      </summary>
      <div className="mt-1.5 ml-[6px] pl-3 border-l-2 border-line italic text-ink-2 leading-6 whitespace-pre-wrap break-words">{block.text.trim()}</div>
    </details>
  )
}

function AssistantMessage({ item, now }: { item: Assistant; now: number }) {
  const err = errorLine(item)
  const detail = errorDetail(item)
  const visible = item.blocks.filter(b => b.text.trim())
  return (
    <div className="min-w-0 grid gap-2" data-testid="assistant-message">
      {visible.map((b, i) => b.kind === 'thinking'
        ? <Thinking key={i} block={b} now={now} />
        : <SafeMarkdown key={i}>{b.text}</SafeMarkdown>)}
      {err && <p className={`m-0 text-[13px] ${err.tone === 'danger' ? 'text-danger-ink' : 'text-warning-ink'}`} data-testid="reply-error">{err.text}</p>}
      {detail && (
        // #2388: the TUI shows only the sentence; the provider's own text is one click away here.
        <details className="group text-[12px]" data-testid="reply-error-detail">
          <summary className="inline-flex items-center gap-1 cursor-pointer select-none list-none text-ink-3 hover:text-ink-2">
            <ChevronRight size={12} className="shrink-0 transition-transform group-open:rotate-90" aria-hidden="true" />The provider's message
          </summary>
          <pre className="m-0 mt-1 p-2 bg-surface border border-line font-mono text-[11.5px] leading-relaxed whitespace-pre-wrap break-all max-h-60 overflow-auto text-ink-2">{detail}</pre>
        </details>
      )}
    </div>
  )
}

// ── Tool calls ──────────────────────────────────────────────────────────────

const RESULT_PREVIEW_LINES = 12

function ToolDetail({ item }: { item: Tool }) {
  const [all, setAll] = useState(false)
  const r = item.result
  let argsText = item.arguments_json
  try { if (argsText) argsText = JSON.stringify(JSON.parse(argsText), null, 2) } catch { /* shown as sent */ }
  // #2388: a call AMP refused for a person's step reads as the TUI's fixed line (the approval notice under it has the rest).
  const detail = r ? (r.refused_line ?? resultDetail(item.name, r.text, r.is_error)) : ''
  const lines = detail.replace(/\r\n?/g, '\n').split('\n')
  const hidden = Math.max(0, lines.length - RESULT_PREVIEW_LINES)
  return (
    <div className="pl-[26px] pr-2.5 pb-2.5 pt-0.5 grid gap-2">
      {argsText && argsText !== '{}' && <div>
        <div className="amp-label mb-1">Arguments{item.args_truncated ? ' (cut on the laptop)' : ''}</div>
        <pre className="m-0 p-2 bg-surface border border-line font-mono text-[11.5px] leading-relaxed whitespace-pre-wrap break-all max-h-60 overflow-auto text-ink-2">{argsText}</pre>
      </div>}
      {r && <div>
        <div className="amp-label mb-1">{r.is_error ? 'Error' : 'Result'}</div>
        {detail.trim()
          ? <pre className={`m-0 p-2 bg-surface border font-mono text-[11.5px] leading-relaxed whitespace-pre-wrap break-all max-h-96 overflow-auto ${r.is_error ? 'border-danger text-danger-ink' : 'border-line text-ink'}`}>{all ? detail : lines.slice(0, RESULT_PREVIEW_LINES).join('\n')}</pre>
          : <p className="m-0 text-[12px] text-ink-3">No output.</p>}
        <div className="flex flex-wrap gap-x-3 mt-1 text-[11.5px] text-ink-3">
          {hidden > 0 && <button type="button" className="underline" onClick={() => setAll(a => !a)}>{all ? 'Show less' : `… ${hidden} more line${hidden === 1 ? '' : 's'}`}</button>}
          {r.truncated && <span>Full output ({formatBytes(r.full_bytes)}) is on the laptop.</span>}
        </div>
      </div>}
    </div>
  )
}

function ToolGlyph({ state }: { state: 'running' | 'ok' | 'failed' }) {
  if (state === 'running') return <Loader2 size={13} className="shrink-0 animate-spin text-st-prog" aria-label="running" />
  if (state === 'failed') return <X size={13} strokeWidth={3} className="shrink-0 text-danger-ink" aria-label="failed" />
  return <Check size={13} strokeWidth={3} className="shrink-0 text-success-ink" aria-label="done" />
}

function ToolRow({ item, workers, now }: { item: Tool; workers: WorkerInfo[]; now: number }) {
  const args = parseArgs(item.arguments_json)
  const r = item.result
  const state = !r ? 'running' : r.is_error ? 'failed' : 'ok'
  const summary = r ? (r.refused_line ?? resultSummary(item.name, r.text, r.is_error)) : ''
  const elapsed = item.startedAt == null ? ''
    : r ? (item.endedAt != null ? formatElapsed(item.endedAt - item.startedAt) : '')
    : runningElapsed(now - item.startedAt)
  const worker = WORKER_TOOLS.includes(item.name) ? workers.find(w => w.task_id === ticketIds(args)[0]) : undefined
  const tone = state === 'failed' ? 'bg-[color-mix(in_srgb,var(--danger)_7%,var(--surface))]' : state === 'running' ? 'bg-surface-2' : ''
  return (
    <details className={`group ${tone}`} data-testid="tool-row" data-state={state}>
      <summary className="block cursor-pointer list-none hover:bg-surface-3">
        <div className="flex items-center gap-2 px-2.5 py-[5px] text-[12.5px] min-w-0">
          <ToolGlyph state={state} />
          <span className="font-mono font-semibold text-ink shrink-0 max-w-[45%] truncate">{toolTitle(item.name, args)}</span>
          {/* The arguments give way first (to a third of the line), as in the TUI. */}
          <span className="font-mono text-ink-3 truncate min-w-0 shrink-[3] max-w-[33%] sm:max-w-none">{toolArgs(item.name, args)}</span>
          {summary && <><span className="text-line-str shrink-0" aria-hidden="true">·</span>
            <span className={`truncate min-w-0 shrink ${state === 'failed' ? 'text-danger-ink' : 'text-ink-2'}`} data-testid="tool-summary">{summary}</span></>}
          {elapsed && <span className="amp-num text-[11px] text-ink-3 shrink-0 ml-auto pl-1">{elapsed}</span>}
          <ChevronRight size={12} className={`shrink-0 text-ink-3 transition-transform group-open:rotate-90 ${elapsed ? '' : 'ml-auto'}`} aria-hidden="true" />
        </div>
        {/* A dispatched worker's live status, under its call (#2347); part of the line, so it shows while folded. */}
        {worker && (
          <div className="flex items-center gap-1.5 pl-[26px] pr-2.5 pb-1.5 -mt-0.5 text-[11.5px] text-ink-2 min-w-0" data-testid="worker-live">
            <span className="w-px self-stretch bg-line-str" aria-hidden="true" />
            <span className="truncate">{workerLine(worker, now, false)}</span>
          </div>
        )}
      </summary>
      <ToolDetail item={item} />
    </details>
  )
}

/** The TUI's fold: a group longer than this folds its earliest successful calls… */
const GROUP_FOLD_AFTER = 5
/** …keeping the last few whole. */
const GROUP_KEEP_LAST = 3

function FoldedCalls({ calls, workers, now }: { calls: Tool[]; workers: WorkerInfo[]; now: number }) {
  const kinds = new Map<string, number>()
  let took: number | null = 0
  for (const c of calls) {
    const kind = toolTitle(c.name, parseArgs(c.arguments_json)).split(/\s+/)[0]
    kinds.set(kind, (kinds.get(kind) ?? 0) + 1)
    took = took != null && c.startedAt != null && c.endedAt != null ? took + (c.endedAt - c.startedAt) : null
  }
  const names = [...kinds].map(([k, n]) => n > 1 ? `${k} ×${n}` : k).join(', ')
  return (
    <details className="group" data-testid="tool-fold">
      <summary className="flex items-center gap-2 px-2.5 py-[5px] cursor-pointer list-none text-[12.5px] min-w-0 hover:bg-surface-3">
        <ToolGlyph state="ok" />
        <span className="font-mono font-semibold text-ink shrink-0">{calls.length} calls</span>
        <span className="text-line-str shrink-0" aria-hidden="true">·</span>
        <span className="truncate min-w-0 text-ink-2">{names}</span>
        {took != null && <span className="amp-num text-[11px] text-ink-3 shrink-0 ml-auto pl-1">{formatElapsed(took)}</span>}
        <ChevronRight size={12} className={`shrink-0 text-ink-3 transition-transform group-open:rotate-90 ${took != null ? '' : 'ml-auto'}`} aria-hidden="true" />
      </summary>
      <div className="border-t border-line-sub">
        {calls.map(c => <ToolRow key={c.key} item={c} workers={workers} now={now} />)}
      </div>
    </details>
  )
}

/** Consecutive tool calls as one block, folded the way the TUI folds them. */
export function ToolGroup({ calls, workers, now }: { calls: Tool[]; workers: WorkerInfo[]; now: number }) {
  const foldBefore = calls.length > GROUP_FOLD_AFTER ? calls.length - GROUP_KEEP_LAST : 0
  const folded = calls.slice(0, foldBefore).filter(c => c.result && !c.result.is_error)
  const rows: React.ReactNode[] = []
  let drawn = false
  calls.forEach((c, i) => {
    if (i < foldBefore && c.result && !c.result.is_error && folded.length > 1) {
      if (!drawn) { rows.push(<FoldedCalls key={`fold-${c.key}`} calls={folded} workers={workers} now={now} />); drawn = true }
      return
    }
    rows.push(<ToolRow key={c.key} item={c} workers={workers} now={now} />)
  })
  return <div className="border border-line bg-surface divide-y divide-line-sub" data-testid="tool-group">{rows}</div>
}

// ── Other transcript rows ───────────────────────────────────────────────────

export function TranscriptRow({ item, now, myDeviceId = null }: { item: TranscriptItem; now: number; myDeviceId?: string | null }) {
  switch (item.kind) {
    case 'user':
      return <UserMessage item={item} myDeviceId={myDeviceId} />
    case 'assistant':
      return <AssistantMessage item={item} now={now} />
    case 'tool':
      return <ToolGroup calls={[item]} workers={[]} now={now} />
    case 'run_end':
      // The TUI shows nothing for a completed run, and the shell's own line for the others.
      if (item.covered) return null
      return (
        <p className={`m-0 text-[13px] ${item.outcome === 'failed' ? 'text-danger-ink' : 'text-ink-3'}`} role="note" data-testid="run-end">
          {item.outcome === 'aborted' ? 'Stopped.' : item.outcome === 'failed' ? `✗ ${item.error ? clean(item.error) : 'The run failed.'}` : item.outcome === 'suspended' ? 'Suspended.' : `Run ${item.outcome}.`}
        </p>
      )
    case 'notice': {
      const cls = item.level === 'error' ? 'text-danger-ink' : item.level === 'warn' ? 'text-warning-ink' : 'text-ink-3'
      return <p className={`m-0 text-[13px] leading-6 whitespace-pre-wrap break-words ${cls}`} role="note" data-testid="notice" data-level={item.level}>{item.text}</p>
    }
    case 'retry':
      // The TUI shows a retry only in its status slot, while it counts down (see WorkingLine).
      return null
    case 'gap':
      return <div className="text-[12px] text-ink-3 text-center" role="note">Earlier messages aren't available.</div>
    case 'security':
      return (
        <div className="flex items-start gap-2 border border-danger bg-surface px-2.5 py-2 text-[12.5px] text-danger-ink" role="alert">
          <ShieldAlert size={15} className="shrink-0 mt-0.5" aria-hidden="true" />
          <span>Untrusted data from the relay was dropped ({item.verdict.replace(/_/g, ' ')}).</span>
        </div>
      )
  }
}

const OUT_LABEL: Record<Outgoing['state'], string> = {
  sending: 'Sending…', forwarded: 'Waiting for the laptop…', started: 'Starting…', queued: 'Queued', done: 'Done',
  refused: 'Not sent', no_answer: 'No answer from the laptop', failed: 'Not sent',
}

/** Our own prompts that the transcript doesn't show yet. Queued ones live in the queue strip. */
export function OutgoingRow({ out }: { out: Outgoing }) {
  const bad = out.state === 'refused' || out.state === 'failed' || out.state === 'no_answer'
  return (
    <div className="flex flex-col items-end" data-testid="outgoing">
      {out.text && <div className={`max-w-[min(85%,680px)] px-3 py-2 text-[14px] leading-6 whitespace-pre-wrap break-words border ${bad ? 'border-danger bg-surface text-ink' : 'border-line-str bg-surface-3 text-ink-2'}`}>{out.text}</div>}
      <span className={`text-[11px] mt-1 ${bad ? 'text-danger-ink' : 'text-ink-3'}`}>
        {out.op !== 'prompt' && `${out.op === 'stop' ? 'Stop' : 'Disconnect'}: `}{bad && out.problem ? out.problem : OUT_LABEL[out.state]}
      </span>
    </div>
  )
}

// ── The dock: workers, working line, queue, message box, status line ──────

export function QueueStrip({ queue, myDeviceId, online, onClear }: {
  queue: QueueItem[]; myDeviceId: string | null; online: boolean; onClear: () => void
}) {
  const [armed, setArmed] = useState(false)
  if (!queue.length) return null
  return (
    <div className="border-t border-line bg-surface-2 px-3 sm:px-5 py-2 grid gap-1.5" aria-label="Queued messages">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="amp-label">Queued ({queue.length})</span>
        <span className="text-[11.5px] text-ink-3">Runs when the agent finishes.</span>
        {/* M1 has no per-message cancel (cancel_queued is reserved for M2): Stop clears the whole queue. */}
        {!armed
          ? <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost ml-auto" disabled={!online} onClick={() => setArmed(true)}>Cancel queued</button>
          : <span className="ml-auto inline-flex items-center gap-1.5 flex-wrap justify-end">
              <span className="text-[11.5px] text-ink-2">This also stops the current run.</span>
              <button type="button" className="amp-btn amp-btn-sm amp-btn-accent" onClick={() => { setArmed(false); onClear() }}>Stop and clear</button>
              <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => setArmed(false)}>Keep</button>
            </span>}
      </div>
      {queue.map((q, i) => (
        <div key={q.prompt_id} className="flex items-baseline gap-2 text-[12.5px] min-w-0" data-testid="queued">
          <span className="amp-num text-ink-3 shrink-0">{i + 1}.</span>
          <span className="truncate text-ink-2">{q.preview}</span>
          <span className="amp-num text-[10.5px] text-ink-3 shrink-0 ml-auto">{q.device_id && q.device_id === myDeviceId ? 'you' : q.origin === 'local' ? 'laptop' : 'other device'}</span>
        </div>
      ))}
    </div>
  )
}

const WORKER_TONE: Record<string, string> = {
  running: 'border-st-prog', done: 'border-line-str', failed: 'border-danger', stopped: 'border-line', blocked: 'border-st-flag',
}
const WORKER_STATE_INK: Record<string, string> = {
  running: 'text-st-prog', done: 'text-ink-2', failed: 'text-danger-ink', stopped: 'text-ink-3', blocked: 'text-st-flag',
}

/** A chip's context colour, as roster.rs `ctx_color`: calm, amber from 80%, red from 95%. */
const chipCtxTone = (p: number) => p >= 95 ? 'text-danger-ink' : p >= 80 ? 'text-warning-ink' : 'text-ink-3'

/** The chat's workers, read-only (watching and stopping one from here is M2): the TUI's roster chips,
 *  `#2142 Name · Epic › Story · worker-backend (model) · running 3m12s · 48.0k tok · ctx 31% · $0.04`. */
export function WorkerChips({ workers, now }: { workers: WorkerInfo[]; now: number }) {
  if (!workers.length) return null
  const running = workers.filter(w => w.state === 'running').length
  return (
    <div className="grid gap-1 px-3 sm:px-5 py-2 border-t border-line bg-surface" aria-label="Workers">
      {workers.map(w => {
        const role = shortRole(w.role)
        const es = epicStory(w.epic, w.story)
        const u = workerUsage(w)
        const ticket = displayStateOf(w.ticket_state)
        return (
          <div key={w.task_id} className={`flex items-center gap-1.5 min-w-0 border-l-2 pl-2 text-[12px] ${WORKER_TONE[w.state] ?? 'border-line'}`} data-testid="worker-chip"
            title={workerLine(w, now, true)}>
            {ticket && <span title={`Ticket: ${STATE_LABEL[ticket]}`} className="inline-flex shrink-0"><StateDot state={ticket} size={7} /></span>}
            <span className="amp-num font-semibold text-accent shrink-0">#{w.task_id}</span>
            <span className="truncate min-w-0 text-ink">{clean(w.name)}</span>
            {es && <span className="text-ink-3 shrink-[2] truncate min-w-0 hidden md:inline" data-testid="worker-epic">· {es}</span>}
            {role && <span className="text-ink-3 shrink-0 hidden sm:inline">· {role}{w.model ? <span className="text-ink-3"> ({shortModel(w.model)})</span> : null}</span>}
            <span className={`amp-num shrink-0 ${WORKER_STATE_INK[w.state] ?? 'text-ink-2'}`} data-testid="worker-state">· {workerState(w, now)}</span>
            {u.tokens && <span className="amp-num shrink-0 text-ink-3" data-testid="worker-tokens">· {u.tokens}</span>}
            {u.ctx && <span className={`amp-num shrink-0 ${chipCtxTone(u.ctxPercent ?? 0)}`} data-testid="worker-ctx">· {u.ctx}</span>}
            {u.cost && <span className="amp-num shrink-0 text-ink-3 hidden sm:inline" data-testid="worker-cost">· {u.cost}</span>}
          </div>
        )
      })}
      <p className="m-0 text-[11px] text-ink-3">{running ? `${running} running` : 'none running'} · watch or stop them on the laptop</p>
    </div>
  )
}

/** The TUI's spinner line: `Working… 12s`, or the retry countdown while one is scheduled. */
export function WorkingLine({ busy, runStartedAt, retry, compaction = null, now }: {
  busy: boolean; runStartedAt: number | null; retry: Extract<TranscriptItem, { kind: 'retry' }> | null
  /** #2388: the harness is compacting the context (the TUI's status slot says so meanwhile). */
  compaction?: { manual: boolean; since: number } | null; now: number
}) {
  if (!busy && !compaction) return null
  const left = retry ? Math.max(0, Math.ceil((retry.at + retry.delay_ms - now) / 1000)) : 0
  const text = compaction
    ? `${compactionLabel(compaction.manual)} ${runningElapsed(now - compaction.since) || ''}`
    : retry
    ? `Retrying (${retry.attempt}/${retry.max_retries}) in ${left}s…`
    : `Working…${runStartedAt ? ` ${runningElapsed(now - runStartedAt) || ''}` : ''}`
  return (
    <div className="flex items-center gap-2 px-3 sm:px-5 py-1.5 border-t border-line bg-surface text-[12.5px] min-w-0" role="status" data-testid="working" data-compaction={compaction ? 'true' : undefined}>
      <Loader2 size={13} className={`shrink-0 animate-spin ${retry ? 'text-warning-ink' : 'text-st-prog'}`} aria-hidden="true" />
      <span className={retry ? 'text-warning-ink' : 'text-ink-2'}>{text.trim()}</span>
      {retry && !compaction && <span className="truncate min-w-0 text-ink-3">{clean(retry.error)}</span>}
      <span className="ml-auto shrink-0 text-[11px] text-ink-3">Stop to {retry ? 'cancel' : 'interrupt'}</span>
    </div>
  )
}

/** Context use in the TUI footer's colours (footer.rs): calm, amber above 70%, red above 90%. */
const ctxTone = (p: number) => p > 90 ? 'text-danger-ink' : p > 70 ? 'text-warning-ink' : 'text-ink'

/**
 * The status line: the TUI footer's usage half (`$0.004 · ctx 4% · model • thinking`) and its AMP
 * status line (questions, workers, plan approval, board counts), with the session's tokens and how
 * long it has been up. Tokens and cost are the harness's own totals (#2388); a harness that predates
 * them falls back to the replies this page has seen.
 */
export function StatusLine({ status, totals, startedAt, workers, now }: {
  status: SessionStatus | null; totals: Totals; startedAt: number | null; workers: WorkerInfo[]; now: number
}) {
  const s = status
  const running = s?.workers_running ?? workers.filter(w => w.state === 'running').length
  const t = sessionTokens(s, totals)
  const thinking = s?.thinking ? (s.thinking === 'off' ? 'thinking off' : clean(s.thinking)) : null
  const parts: [string, React.ReactNode, string?][] = [
    ['Model', s?.model ? <>{shortModel(s.model)}{thinking && <span className="text-ink-3"> • {thinking}</span>}</> : '—', s?.model ?? undefined],
    ['Context', s?.context_pct != null ? <span className={ctxTone(s.context_pct)}>{s.context_pct}%</span> : '—'],
    ['Tokens', t ? tokenCount(t.total) : '—',
      t ? `${t.input.toLocaleString()} in · ${t.output.toLocaleString()} out · ${t.cache_read.toLocaleString()} cached${t.cache_write ? ` · ${t.cache_write.toLocaleString()} cache write` : ''}${t.fromHarness ? ', the session\'s totals on the laptop' : `, over the ${totals.messages} replies shown here`}` : undefined],
    ['Cost', formatCost(s?.cost_usd_micros)],
    ['Up', startedAt ? formatSpan(now - startedAt) : '—'],
    ['Workers', running ? `${running} running` : '0'],
  ]
  const b = s?.board
  const plan = planApprovalText(s?.plan_approval)
  const counts: [keyof NonNullable<SessionStatus['board']>, 'prog' | 'ready' | 'sched'][] = [['in_progress', 'prog'], ['ready', 'ready'], ['scheduled', 'sched']]
  return (
    <div className="border-t border-line bg-surface-2 px-3 sm:px-5 py-1.5 grid gap-1">
      <dl className="m-0 flex flex-wrap gap-x-4 gap-y-0.5 text-[11.5px]" aria-label="Session status">
        {parts.map(([k, v, title]) => (
          <div key={k} className="flex items-baseline gap-1.5 min-w-0" title={title}>
            <dt className="amp-label !text-[10px]">{k}</dt>
            <dd className="m-0 amp-num text-ink truncate max-w-[260px]">{v}</dd>
          </div>
        ))}
      </dl>
      {(b || plan) && (
        // The TUI's AMP status line: the questions chip first, then plan approval, then the board's counts.
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11.5px] min-w-0" aria-label="Board" data-testid="board-line">
          {b && b.question > 0 && <StateTag state="flag" label={`${b.question} question${b.question === 1 ? '' : 's'}`} />}
          {plan && <span className="text-warning-ink" data-testid="plan-approval">{plan}</span>}
          {b && (counts.some(([k]) => b[k] > 0)
            ? counts.filter(([k]) => b[k] > 0).map(([k, st]) => (
                <span key={k} className="inline-flex items-center gap-1 amp-num text-ink-2"><StateDot state={st} size={7} />{b[k]} {STATE_LABEL[st]}</span>
              ))
            : !b.question && <span className="text-ink-3">no open tickets</span>)}
        </div>
      )}
    </div>
  )
}

/** The page's header facts (the TUI's header and footer): project, working directory (its
 *  last component only), branch, role and OS. */
export function HeaderFacts({ status, os }: { status: SessionStatus | null; os: string | null }) {
  const s = status
  const facts: string[] = []
  if (s?.project) facts.push(`${s.project.name} #${s.project.id}`)
  // The ticket line below names the ticket when the harness sends its name; an older one sends only the id.
  if (s?.ticket_id != null && !s.ticket_name && !s.ticket_state) facts.push(`ticket #${s.ticket_id}`)
  // The TUI footer's `cwd (branch)`.
  const cwd = s?.cwd ? clean(s.cwd) : '', branch = s?.branch ? clean(s.branch) : ''
  const where = cwd && branch ? `${cwd} (${branch})` : cwd || branch
  if (where) facts.push(where)
  if (s?.role) facts.push(clean(s.role))
  if (os) facts.push(os)
  if (!facts.length) return null
  return <span className="amp-num text-[11.5px] text-ink-2 truncate min-w-0" data-testid="header-facts">{facts.join(' · ')}</span>
}

/** The TUI header's second line while a ticket is current: `■ In progress #2388 Name    ▌ Epic › Story`. */
export function TicketLine({ status }: { status: SessionStatus | null }) {
  const s = status
  if (s?.ticket_id == null || (!s.ticket_name && !s.ticket_state)) return null
  const st = displayStateOf(s.ticket_state)
  const es = epicStory(s.epic, s.story)
  return (
    <div className="bg-surface border-b border-line px-3 sm:px-5 py-1.5 flex items-center gap-2 min-w-0 text-[12.5px]" data-testid="ticket-line">
      {st && <StateTag state={st} />}
      <span className="amp-num font-semibold text-ink shrink-0">#{s.ticket_id}</span>
      {s.ticket_name && <span className="truncate min-w-0 text-ink">{clean(s.ticket_name)}</span>}
      {es && <span className="ml-auto shrink-[2] truncate min-w-0 pl-2 border-l-2 border-accent text-ink-3 hidden sm:inline" data-testid="ticket-epic">{es}</span>}
    </div>
  )
}

/** The harness's own AMP connection (the TUI header's `● connected`), when its status carries it. */
export function AmpConnectionBadge({ status }: { status: SessionStatus | null }) {
  const c = ampConnectionWord(status?.amp_connection)
  if (!c) return null
  const cls = c.tone === 'ok' ? 'text-success-ink' : c.tone === 'warn' ? 'text-warning-ink' : 'text-danger-ink'
  return (
    <span className="inline-flex items-center gap-1.5 text-[11.5px] whitespace-nowrap text-ink-2" data-testid="amp-connection"
      title="The laptop's own connection to AMP (its AMP tools), as the harness header shows it">
      <span className="amp-label !text-[9.5px]">AMP</span><span className={`w-1.5 h-1.5 rounded-full bg-current ${cls}`} aria-hidden="true" /><span className={c.tone === 'ok' ? '' : cls}>{c.word}</span>
    </span>
  )
}

export function PresenceBadge({ online, connection }: { online: boolean | null; connection: 'connecting' | 'live' | 'reconnecting' }) {
  const [label, cls] = connection === 'reconnecting' ? ['Reconnecting…', 'border-warning text-warning-ink']
    : online === null ? ['Connecting…', 'border-line-str text-ink-3']
    : online ? ['Laptop online', 'border-success text-success-ink'] : ['Laptop offline', 'border-danger text-danger-ink']
  return (
    <span className={`inline-flex items-center gap-1.5 border px-2 py-0.5 text-[11.5px] font-semibold whitespace-nowrap ${cls}`} role="status" aria-live="polite">
      <span className="w-1.5 h-1.5 rounded-full bg-current" aria-hidden="true" />{label}
    </span>
  )
}

export function Composer({ disabled, disabledReason, busy, online, onSend, onStop }: {
  disabled: boolean; disabledReason: string | null; busy: boolean; online: boolean; onSend: (text: string) => void; onStop: () => void
}) {
  const [text, setText] = useState('')
  const [problem, setProblem] = useState<string | null>(null)
  const submit = () => {
    const p = promptProblem(text)
    if (p) { setProblem(p); return }
    setProblem(null)
    onSend(text)
    setText('')
  }
  return (
    <div className="border-t border-line bg-surface px-3 sm:px-5 pt-2.5 pb-2.5 grid gap-2">
      <form className="flex gap-2 items-end" onSubmit={e => { e.preventDefault(); submit() }}>
        <label htmlFor="remote-message" className="sr-only">Message</label>
        <textarea id="remote-message" rows={2} value={text} disabled={disabled}
          placeholder={disabled ? disabledReason ?? '' : busy ? 'The agent is working. Your message will be queued.' : 'Message the agent'}
          onChange={e => setText(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); submit() } }}
          className="amp-input flex-1 min-w-0 resize-none text-[14px] leading-6 py-2 max-h-48" />
        <button type="submit" className="amp-btn amp-btn-primary h-[42px]" disabled={disabled}>{busy ? 'Queue' : 'Send'}</button>
      </form>
      {problem && <p className="m-0 text-[12px] text-danger-ink" role="alert">{problem}</p>}
      {disabled && disabledReason && <p className="m-0 text-[12px] text-ink-2">{disabledReason}</p>}
      {/* Stop must be cheap: enabled whenever the laptop is online (R-M7). */}
      <button type="button" onClick={onStop} disabled={!online}
        className="amp-btn amp-btn-accent w-full justify-center h-12 text-[13px] gap-2" aria-label="Stop the agent">
        {busy ? <OctagonX size={18} aria-hidden="true" /> : <Square size={16} aria-hidden="true" />}Stop
      </button>
    </div>
  )
}

export function Banner({ tone, children }: { tone: 'danger' | 'warn' | 'info'; children: React.ReactNode }) {
  const cls = tone === 'danger' ? 'border-danger text-danger-ink' : tone === 'warn' ? 'border-warning text-warning-ink' : 'border-line-str text-ink-2'
  return (
    <div className={`flex items-start gap-2 border-l-[3px] bg-surface px-3 py-2 text-[13px] ${cls}`} role="status">
      <TriangleAlert size={15} className="shrink-0 mt-0.5" aria-hidden="true" />
      <div className="min-w-0">{children}</div>
    </div>
  )
}

