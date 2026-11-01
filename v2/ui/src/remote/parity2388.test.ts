import { describe, expect, it } from 'vitest'
import { initialSessionState, sessionReducer, type SessionState } from './session'
import { resultSummary, toolTitle, parseArgs } from './toolText'
import { ampConnectionWord, compactionLabel, epicStory, errorLine, planApprovalText, workerLine } from './transcriptView'
import parity from './mock/parity2388.json'
import type { ChatEvent, Envelope, SessionStatus, WorkerInfo } from './types'

/**
 * #2388: parity2388.json was recorded from the real harness: the manager feed and the status its
 * remote link sent, for a run with an AMP call refused for approval, a provider error (429) and a
 * compaction; and two real Supervisor workers' worker_status. `tui` holds the TUI's own lines for
 * the same run. The web must say the same things.
 */
const rec = parity as unknown as {
  status: SessionStatus; stream: { at_ms: number; event: ChatEvent }[]; workers: { at_ms: number; event: ChatEvent }
  tui: { refused_row: string; error_line: string; compaction: string; chips: string[]; header: string[]; footer: string[] }
}

function replay(): SessionState {
  let seq = 0
  return rec.stream.reduce((s, e) => sessionReducer(s, { type: 'event', myDeviceId: null,
    env: { v: 1, alg: 'ES256', session_id: 's', harness_id: 'h', device_id: null, seq: ++seq, ts: e.at_ms, nonce: 'n', op: 'chat_event', body: e.event, sig: 'x' } as Envelope }),
  initialSessionState)
}

describe('the web says what the TUI says for the same harness run (#2388)', () => {
  const s = replay()

  it('gap 5: an AMP call refused for approval reads as the TUI\'s row', () => {
    const t = s.items.find(i => i.kind === 'tool' && i.name === 'amp_approve_plan')
    expect(t?.kind).toBe('tool')
    if (t?.kind !== 'tool' || !t.result) throw new Error('no refused call')
    const line = t.result.refused_line ?? resultSummary(t.name, t.result.text, t.result.is_error)
    expect(rec.tui.refused_row).toContain(`${toolTitle(t.name, parseArgs(t.arguments_json))} · ${line}`)
  })

  it('gap 4: a failed reply reads as the TUI\'s one sentence', () => {
    const a = s.items.find(i => i.kind === 'assistant' && i.stop_reason === 'error')
    if (a?.kind !== 'assistant') throw new Error('no failed reply')
    expect(errorLine(a)?.text).toBe(rec.tui.error_line)
    expect(a.error).toMatch(/^http error 429: /) // the raw text stays as the detail
  })

  it('gap 3: compaction started and finished, worded as the TUI\'s indicator', () => {
    const types = rec.stream.map(e => e.event).filter(e => e.type === 'compaction')
    expect(types).toEqual([{ type: 'compaction', active: true, manual: false }, { type: 'compaction', active: false, manual: false }])
    expect(rec.tui.compaction.replace('...', '…')).toContain(compactionLabel(false))
    expect(s.compaction).toBeNull()
  })

  it('gap 1: worker chips match the TUI roster line for line', () => {
    const ev = rec.workers.event
    if (ev.type !== 'worker_status') throw new Error('no worker_status')
    const now = rec.workers.at_ms + 2_000 // the TUI's capture said "running 2s"
    const web = ev.workers.map((w: WorkerInfo) => `▸ ${workerLine({ ...w, started_at_ms: rec.workers.at_ms }, now, true)}`)
    expect(web).toEqual(rec.tui.chips)
  })

  it('gaps 2 and 6: the status carries the TUI header and status line facts', () => {
    const st = rec.status
    expect(rec.tui.header[1]).toContain(`#${st.ticket_id} ${st.ticket_name}`)
    expect(rec.tui.header[1]).toContain(epicStory(st.epic, st.story))
    expect(rec.tui.header[0]).toContain(`● ${ampConnectionWord(st.amp_connection)!.word.replace(' · retrying…', '')}`)
    expect(rec.tui.footer[0]).toContain(planApprovalText(st.plan_approval)!)
    expect(rec.tui.footer[0]).toContain(`${st.board!.question} QUESTIONS`)
    expect(rec.tui.footer[1]).toContain(`(${st.branch}) • ${st.role} · #${st.ticket_id}`)
    expect(rec.tui.footer[1]).toContain(`• ${st.thinking}`)
    // Only the working directory's last component leaves the laptop.
    expect(rec.tui.footer[1]).toContain(`/${st.cwd} (`)
    expect(st.cwd).toBe('amp')
    expect(st.usage).toMatchObject({ input_tokens: expect.any(Number), output_tokens: expect.any(Number) })
  })
})
