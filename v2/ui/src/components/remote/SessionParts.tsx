/**
 * The pieces of the remote session page: transcript rows, the status bar,
 * the queue strip and the message box. Everything from the harness is
 * untrusted: text is shown as text or through SafeMarkdown, never as HTML.
 */
import { useState } from 'react'
import { Check, ChevronRight, CircleAlert, Loader2, OctagonX, ShieldAlert, Square, TriangleAlert, Wrench } from 'lucide-react'
import { SafeMarkdown } from './SafeMarkdown'
import { promptProblem } from '../../remote/envelope'
import { formatCost, type Outgoing, type TranscriptItem } from '../../remote/session'
import type { QueueItem, SessionStatus, Usage, WorkerInfo } from '../../remote/types'

const formatBytes = (n: number) => n < 1024 ? `${n} B` : n < 1_048_576 ? `${Math.round(n / 1024)} KB` : `${(n / 1_048_576).toFixed(1)} MB`

/** One line of tool arguments for the collapsed row. */
function argsPreview(json: string): string {
  const flat = json.replace(/\s+/g, ' ').trim()
  return flat.length > 90 ? `${flat.slice(0, 90)}…` : flat
}

const RESULT_LINES = 12
const RESULT_CHARS = 1500

function shorten(text: string): { short: string; cut: boolean } {
  const lines = text.split('\n')
  let short = lines.slice(0, RESULT_LINES).join('\n')
  if (short.length > RESULT_CHARS) short = short.slice(0, RESULT_CHARS)
  return { short, cut: short.length < text.length }
}

function UsageLine({ usage, model }: { usage: Usage | null; model: string }) {
  if (!usage) return null
  return (
    <div className="amp-num text-[10.5px] text-ink-3 mt-1.5">
      {model} · {usage.input_tokens.toLocaleString()} in · {usage.output_tokens.toLocaleString()} out
      {usage.cache_read_tokens ? ` · ${usage.cache_read_tokens.toLocaleString()} cached` : ''} · {formatCost(usage.cost_usd_micros)}
    </div>
  )
}

function ToolRow({ item }: { item: Extract<TranscriptItem, { kind: 'tool' }> }) {
  const [all, setAll] = useState(false)
  const r = item.result
  const res = r ? shorten(r.text) : null
  return (
    <details className="group border border-line bg-surface-2" data-testid="tool-row">
      <summary className="flex items-center gap-2 px-2.5 py-1.5 cursor-pointer list-none text-[12.5px] min-w-0">
        <ChevronRight size={13} className="shrink-0 text-ink-3 transition-transform group-open:rotate-90" aria-hidden="true" />
        {!r ? <Loader2 size={13} className="shrink-0 animate-spin text-st-prog" aria-label="running" />
          : r.is_error ? <CircleAlert size={13} className="shrink-0 text-danger-ink" aria-label="failed" />
          : <Check size={13} className="shrink-0 text-success-ink" aria-label="done" />}
        <Wrench size={12} className="shrink-0 text-ink-3" aria-hidden="true" />
        <span className="font-mono font-semibold text-ink shrink-0">{item.name}</span>
        <span className="font-mono text-ink-3 truncate min-w-0">{argsPreview(item.arguments_json)}</span>
      </summary>
      <div className="px-2.5 pb-2.5 grid gap-2">
        {item.arguments_json && <div>
          <div className="amp-label mb-1">Arguments{item.args_truncated ? ' (cut on the laptop)' : ''}</div>
          <pre className="m-0 p-2 bg-surface border border-line font-mono text-[11.5px] leading-relaxed whitespace-pre-wrap break-all max-h-60 overflow-auto">{item.arguments_json}</pre>
        </div>}
        {r && res && <div>
          <div className="amp-label mb-1">{r.is_error ? 'Error output' : 'Result'}</div>
          <pre className={`m-0 p-2 bg-surface border font-mono text-[11.5px] leading-relaxed whitespace-pre-wrap break-all max-h-96 overflow-auto ${r.is_error ? 'border-danger text-danger-ink' : 'border-line text-ink'}`}>{all ? r.text : res.short}</pre>
          <div className="flex flex-wrap gap-x-3 mt-1 text-[11.5px] text-ink-3">
            {res.cut && <button type="button" className="underline" onClick={() => setAll(a => !a)}>{all ? 'Show less' : 'Show all received'}</button>}
            {r.truncated && <span>Full output ({formatBytes(r.full_bytes)}) is on the laptop.</span>}
          </div>
        </div>}
      </div>
    </details>
  )
}

const OUTCOME: Record<string, string> = { completed: 'Run finished', aborted: 'Run stopped', failed: 'Run failed', suspended: 'Run suspended' }

export function TranscriptRow({ item }: { item: TranscriptItem }) {
  switch (item.kind) {
    case 'user':
      return (
        <div className="flex flex-col items-end" data-testid="user-message">
          <div className="max-w-[min(85%,680px)] bg-ink text-on-ink px-3 py-2 text-[14px] leading-6 whitespace-pre-wrap break-words">{item.text}</div>
          <span className="amp-num text-[10.5px] text-ink-3 mt-1">{item.origin === 'remote' ? `from ${item.device_name ?? 'a paired device'}` : 'typed on the laptop'}</span>
        </div>
      )
    case 'assistant':
      return (
        <div className="min-w-0" data-testid="assistant-message">
          {item.thinking && (
            <details className="mb-1.5 text-[12.5px] text-ink-3">
              <summary className="cursor-pointer select-none">Thinking</summary>
              <div className="mt-1 pl-3 border-l-2 border-line whitespace-pre-wrap break-words">{item.thinking}</div>
            </details>
          )}
          {item.text && <SafeMarkdown>{item.text}</SafeMarkdown>}
          {!item.done && !item.text && !item.thinking && <Loader2 size={14} className="animate-spin text-ink-3" aria-label="writing" />}
          {item.error && <p className="m-0 mt-1 text-[12.5px] text-danger-ink">{item.error}</p>}
          {item.done && <UsageLine usage={item.usage} model={item.model} />}
        </div>
      )
    case 'tool':
      return <ToolRow item={item} />
    case 'run_end':
      return (
        <div className="flex items-center gap-2 text-[11.5px] text-ink-3" role="note">
          <span className="h-px flex-1 bg-line" />
          <span className={item.outcome === 'failed' ? 'text-danger-ink' : ''}>{OUTCOME[item.outcome] ?? `Run ${item.outcome}`}{item.error ? `: ${item.error}` : ''}</span>
          <span className="h-px flex-1 bg-line" />
        </div>
      )
    case 'notice': {
      const cls = item.level === 'error' ? 'border-danger text-danger-ink' : item.level === 'warn' ? 'border-warning text-warning-ink' : 'border-line-str text-ink-2'
      return <div className={`border-l-[3px] pl-2.5 py-0.5 text-[12.5px] whitespace-pre-wrap break-words ${cls}`} role="note">{item.text}</div>
    }
    case 'retry':
      return <div className="text-[12px] text-warning-ink" role="note">Retrying ({item.attempt}/{item.max_retries}) in {Math.round(item.delay_ms / 1000)} s: {item.error}</div>
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

export function QueueStrip({ queue, myDeviceId, online, onClear }: {
  queue: QueueItem[]; myDeviceId: string | null; online: boolean; onClear: () => void
}) {
  const [armed, setArmed] = useState(false)
  if (!queue.length) return null
  return (
    <div className="border-t border-line bg-surface-2 px-3 py-2 grid gap-1.5" aria-label="Queued messages">
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

export function WorkerChips({ workers }: { workers: WorkerInfo[] }) {
  if (!workers.length) return null
  const tone: Record<string, string> = { running: 'text-st-prog border-st-prog', done: 'text-ink-2 border-line-str', failed: 'text-danger-ink border-danger', stopped: 'text-ink-3 border-line', blocked: 'text-st-flag border-st-flag' }
  return (
    <div className="flex flex-wrap gap-1.5" aria-label="Workers">
      {workers.map(w => (
        <span key={w.task_id} className={`inline-flex items-center gap-1 border px-1.5 py-px text-[11px] max-w-[220px] ${tone[w.state] ?? 'border-line text-ink-2'}`} title={`#${w.task_id} ${w.name} · ${w.role} · ${w.state}`}>
          <span className="amp-num">#{w.task_id}</span><span className="truncate">{w.name}</span><span className="amp-num">· {w.state}</span>
        </span>
      ))}
    </div>
  )
}

export function StatusBar({ status, host }: { status: SessionStatus | null; host: string }) {
  const s = status
  const parts: [string, React.ReactNode][] = [
    ['Model', s?.model ?? '—'],
    ['Context', s?.context_pct != null ? `${s.context_pct}%` : '—'],
    ['Cost', formatCost(s?.cost_usd_micros)],
    ['Project', s?.project ? `${s.project.name}${s.ticket_id != null ? ` · #${s.ticket_id}` : ''}` : '—'],
    ['Laptop', host],
  ]
  return (
    <dl className="m-0 flex flex-wrap gap-x-5 gap-y-1 text-[12px]" aria-label="Session status">
      {parts.map(([k, v]) => (
        <div key={k} className="flex items-baseline gap-1.5 min-w-0">
          <dt className="amp-label">{k}</dt>
          <dd className="m-0 amp-num text-ink truncate max-w-[240px]">{v}</dd>
        </div>
      ))}
    </dl>
  )
}

export function PresenceBadge({ online, connection }: { online: boolean | null; connection: 'connecting' | 'live' | 'reconnecting' }) {
  const [label, cls] = connection === 'reconnecting' ? ['Reconnecting…', 'border-warning text-warning-ink']
    : online === null ? ['Connecting…', 'border-line-str text-ink-3']
    : online ? ['Laptop online', 'border-success text-success-ink'] : ['Laptop offline', 'border-danger text-danger-ink']
  return (
    <span className={`inline-flex items-center gap-1.5 border px-2 py-0.5 text-[11.5px] font-semibold ${cls}`} role="status" aria-live="polite">
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
    <div className="border-t border-line bg-surface px-3 pt-2.5 pb-[max(10px,env(safe-area-inset-bottom))] grid gap-2">
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
