/**
 * How the session page lays out the transcript, apart from drawing it (so it is tested without
 * React and the components file only exports components): timers, the TUI's thinking label and
 * reply error line, worker lines, and grouping consecutive tool calls.
 */
import { useEffect, useState } from 'react'
import type { Block, TranscriptItem } from './session'
import { clean, runningElapsed, shortModel, shortRole } from './toolText'
import type { WorkerInfo } from './types'

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
  if (a.stop_reason === 'error') return { text: `✗ Error: ${a.error ? clean(a.error.split('\n')[0]) : 'the model returned an error'}`, tone: 'danger' }
  if (a.stop_reason === 'length') return { text: '! Response was truncated before completion.', tone: 'warn' }
  if (a.error) return { text: `✗ ${clean(a.error.split('\n')[0])}`, tone: 'danger' }
  return null
}

/** Whether an assistant message draws anything (one that only made tool calls doesn't, so
 *  the calls around it stay one group, as in the TUI). */
export function assistantVisible(a: Assistant): boolean {
  return a.blocks.some(b => b.text.trim()) || errorLine(a) !== null
}

/** A worker's line (the TUI's roster chip): `#218 Name · worker-backend (model) · running 3s`. */
export function workerLine(w: WorkerInfo, now: number, withId: boolean): string {
  const parts: string[] = []
  const head = `${withId ? `#${w.task_id} ` : ''}${clean(w.name)}`.trim()
  if (head) parts.push(head)
  const role = shortRole(w.role)
  if (role) parts.push(`${role}${w.model ? ` (${shortModel(w.model)})` : ''}`)
  const elapsed = w.state === 'running' && w.started_at_ms ? runningElapsed(now - w.started_at_ms) : ''
  parts.push(elapsed ? `${w.state} ${elapsed}` : w.state)
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

