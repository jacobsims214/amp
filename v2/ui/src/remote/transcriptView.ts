/**
 * How the session page lays out the transcript, apart from drawing it (so it is tested without
 * React and the components file only exports components): timers, the TUI's thinking label and
 * reply error line, worker lines, and grouping consecutive tool calls.
 */
import { useEffect, useState } from 'react'
import type { Block, TranscriptItem } from './session'
import { clean, ctxLabel, rosterDuration, shortModel, shortRole, tokLabel, workerContextPercent } from './toolText'
import type { AmpConnection, WorkerInfo } from './types'
import type { DisplayState } from '../lib/ticketState'

type Tool = Extract<TranscriptItem, { kind: 'tool' }>
type Assistant = Extract<TranscriptItem, { kind: 'assistant' }>

/** Re-renders every `ms` while `on`, for live timers. */
export function useNow(on: boolean, ms = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!on) return
    const t = setInterval(() => setNow(Date.now()), ms)
    return () => clearInterval(t)
  }, [on, ms])
  return now
}

/** "Thought for 9s" once it ended, "Thinking · 3s" while it streams. */
export function thinkingLabel(b: Block, now: number): string {
  const secs = Math.floor(Math.max(0, (b.endedAt ?? now) - b.startedAt) / 1000)
  return b.endedAt != null ? `Thought for ${secs}s` : `Thinking · ${secs}s`
}

/** The reply's error line, as the TUI words it: a stopped reply has none (the run says "Stopped."). */
export function errorLine(a: Assistant): { text: string; tone: 'danger' | 'warn' } | null {
  if (!a.done || a.stop_reason === 'aborted') return null
  // #2388: the harness sends the TUI's one sentence; an older harness only the raw text (its first line then).
  const said = a.error_summary ? clean(a.error_summary) : a.error ? clean(a.error.split('\n')[0]) : null
  if (a.stop_reason === 'error') return { text: `✗ Error: ${said ?? 'the model returned an error'}`, tone: 'danger' }
  if (a.stop_reason === 'length') return { text: '! Response was truncated before completion.', tone: 'warn' }
  if (said) return { text: `✗ ${said}`, tone: 'danger' }
  return null
}

/** The raw provider text behind a reply's error line, when it says more than the line (#2388). */
export function errorDetail(a: Assistant): string | null {
  if (!a.error || !errorLine(a)) return null
  const raw = a.error.trim()
  return raw && (a.error_summary ? clean(raw) !== clean(a.error_summary) : raw.includes('\n')) ? raw : null
}

/** Whether an assistant message draws anything (one that only made tool calls doesn't, so
 *  the calls around it stay one group, as in the TUI). */
export function assistantVisible(a: Assistant): boolean {
  return a.blocks.some(b => b.text.trim()) || errorLine(a) !== null
}

/** `Epic › Story`, or whichever is known (roster.rs `epic_story`). */
export function epicStory(epic: string | null | undefined, story: string | null | undefined): string {
  const e = clean(epic ?? ''), st = clean(story ?? '')
  return e && st ? `${e} › ${st}` : e || st
}

/** A worker's state as the TUI's chip words it: `running 3m12s`, `completed after 3m10s`. */
export function workerState(w: WorkerInfo, now: number): string {
  if (w.state === 'running') return w.started_at_ms != null ? `running ${rosterDuration(now - w.started_at_ms)}` : 'running'
  const word = w.state === 'done' ? 'completed' : w.state
  return w.started_at_ms != null && w.ended_at_ms != null ? `${word} after ${rosterDuration(w.ended_at_ms - w.started_at_ms)}` : word
}

/** The chip's usage pieces: `7.8k tok`, `ctx 8%`, `$0.04` (only what is known). */
export function workerUsage(w: WorkerInfo): { tokens: string | null; ctx: string | null; ctxPercent: number | null; cost: string | null } {
  const u = w.usage
  const used = u ? (u.input_tokens || 0) + (u.output_tokens || 0) + (u.cache_read_tokens || 0) + (u.cache_write_tokens || 0) : 0
  const p = workerContextPercent(w.context_tokens, w.context_window)
  return {
    tokens: used > 0 ? tokLabel(used) : null,
    ctx: p != null ? ctxLabel(p) : null,
    ctxPercent: p,
    cost: u && u.cost_usd_micros > 0 ? `$${(u.cost_usd_micros / 1e6).toFixed(2)}` : null,
  }
}

/** A worker's line (the TUI's roster chip, roster.rs `chip`):
 *  `#2142 Name · Epic › Story · worker-backend (model) · running 3m12s · 48.0k tok · ctx 31% · $0.04`. */
export function workerLine(w: WorkerInfo, now: number, withId: boolean): string {
  const parts: string[] = []
  const head = `${withId ? `#${w.task_id} ` : ''}${clean(w.name)}`.trim()
  if (head) parts.push(head)
  const es = epicStory(w.epic, w.story)
  if (es) parts.push(es)
  const role = shortRole(w.role)
  if (role) parts.push(`${role}${w.model ? ` (${shortModel(w.model)})` : ''}`)
  parts.push(workerState(w, now))
  const u = workerUsage(w)
  for (const piece of [u.tokens, u.ctx, u.cost]) if (piece) parts.push(piece)
  return parts.join(' · ')
}

/** Groups consecutive tool calls (skipping assistant messages that draw nothing between them). */
export type Row = { kind: 'item'; item: TranscriptItem } | { kind: 'tools'; key: string; calls: Tool[] }
export function groupRows(items: TranscriptItem[]): Row[] {
  const rows: Row[] = []
  let open: Extract<Row, { kind: 'tools' }> | null = null
  for (const it of items) {
    if (it.kind === 'tool') {
      if (!open) { open = { kind: 'tools', key: it.key, calls: [] }; rows.push(open) }
      open.calls.push(it)
      continue
    }
    if (it.kind === 'assistant' && !assistantVisible(it)) continue
    if (it.kind === 'retry' || (it.kind === 'run_end' && it.covered)) continue
    open = null
    rows.push({ kind: 'item', item: it })
  }
  return rows
}


/** The harness's board state word (`in_progress`, protocol 6.3) as the web UI's display state. */
export function displayStateOf(word: string | null | undefined): DisplayState | null {
  const m: Record<string, DisplayState> = { ready: 'ready', in_progress: 'prog', question: 'flag', scheduled: 'sched', waiting: 'wait', done: 'done' }
  return (word && m[word]) || null
}

/** The harness's AMP connection as the TUI header words it (chrome.rs `health_word`), and its tone. */
export function ampConnectionWord(c: AmpConnection | null | undefined): { word: string; tone: 'ok' | 'warn' | 'down' } | null {
  if (!c || typeof c.state !== 'string') return null
  switch (c.state) {
    case 'connected': return { word: 'connected', tone: 'ok' }
    case 'connecting': return { word: 'connecting', tone: 'warn' }
    case 'slow': return { word: c.latency_ms != null ? `slow ${c.latency_ms} ms` : 'slow', tone: 'warn' }
    case 'degraded': return { word: 'degraded', tone: 'warn' }
    case 'login_needed': return { word: 're-login needed', tone: 'down' }
    case 'disconnected': return { word: 'disconnected · retrying…', tone: 'down' }
    default: return { word: clean(c.state), tone: 'warn' }
  }
}

/** The TUI's spinner text while the context is compacted (rpi-tui `StatusIndicator::compaction`). */
export function compactionLabel(manual: boolean): string {
  return manual ? 'Compacting context…' : 'Auto-compacting…'
}

/** The plan-approval segment of the TUI's status line (chrome.rs), or null. */
export function planApprovalText(p: { yours: number; others: number } | null | undefined): string | null {
  if (!p) return null
  const parts: string[] = []
  if (p.yours > 0) parts.push(`plan: ${p.yours} waiting for your approval`)
  if (p.others > 0) parts.push(p.yours > 0 ? `${p.others} others' waiting for their owners` : `plan: ${p.others} others' waiting for their owners' approval`)
  return parts.length ? parts.join(' · ') : null
}
