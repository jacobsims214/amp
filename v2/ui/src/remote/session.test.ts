import { describe, expect, it } from 'vitest'
import { blockText, formatCost, initialSessionState, isBusy, sessionReducer, sessionTokens, type SessionAction, type SessionState } from './session'
import { seedChatEvents } from './mock/mockRelay'
import { CHAT_EVENT_TYPES, type ChatEvent, type Envelope } from './types'

let seq = 0
const env = (op: string, body: unknown, device_id: string | null = null): Envelope =>
  ({ v: 1, alg: 'ES256', session_id: 's', harness_id: 'h', device_id, seq: ++seq, ts: 0, nonce: 'n', op, body, sig: 'x' })
const run = (actions: SessionAction[], from: SessionState = initialSessionState) => actions.reduce(sessionReducer, from)
const chat = (ev: ChatEvent): SessionAction => ({ type: 'event', env: env('chat_event', ev), myDeviceId: 'me' })

describe('sessionReducer', () => {
  it('the seed transcript covers every ChatEvent type', () => {
    expect(new Set(seedChatEvents().map(e => e.type))).toEqual(new Set(CHAT_EVENT_TYPES))
  })

  it('builds the transcript from every ChatEvent type', () => {
    const s = run(seedChatEvents().map(chat))
    expect(s.items.map(i => i.kind)).toEqual(['user', 'assistant', 'tool', 'retry', 'assistant', 'run_end', 'notice'])
    const a = s.items[1]
    expect(a.kind === 'assistant' && a).toMatchObject({ done: true })
    expect(a.kind === 'assistant' && blockText(a, 'thinking')).toContain('amp-core')
    expect(a.kind === 'assistant' && blockText(a, 'text')).toBe('Running the **amp-core** tests first.')
    const t = s.items[2]
    expect(t.kind === 'tool' && t.result).toMatchObject({ is_error: true, truncated: true, full_bytes: 182_331 })
    expect(s.workers).toHaveLength(2)
    expect(s.queue).toEqual([])
    expect(s.running).toBeNull()
    expect(s.lastAppliedSeq).toBe(seq)
  })

  it('tracks the run and the queue', () => {
    let s = run([chat({ type: 'run_start', run_id: 'r1', prompt_id: 'p1' })])
    expect(isBusy(s)).toBe(true)
    s = run([chat({ type: 'queue', items: [{ prompt_id: 'p2', origin: 'remote', device_id: 'me', preview: 'next' }] })], s)
    expect(s.queue).toHaveLength(1)
    s = run([chat({ type: 'run_end', run_id: 'r1', outcome: 'aborted', error: null })], s)
    expect(isBusy(s)).toBe(false)
  })

  it('matches acks and refusals to this device\'s commands only', () => {
    let s = run([{ type: 'outgoing', out: { seq: 100, op: 'prompt', client_msg_id: 'm-1', text: 'hi', state: 'forwarded', prompt_id: null, problem: null } }])
    // Another device's ack doesn't touch ours.
    s = run([{ type: 'event', env: env('ack', { ref: { device_id: 'other', seq: 100 }, op: 'prompt', result: 'started', prompt_id: 'p9', queue_position: null }, 'other'), myDeviceId: 'me' }], s)
    expect(s.outgoing[0].state).toBe('forwarded')
    s = run([{ type: 'event', env: env('ack', { ref: { device_id: 'me', seq: 100 }, op: 'prompt', result: 'queued', prompt_id: 'p3', queue_position: 1 }, 'me'), myDeviceId: 'me' }], s)
    expect(s.outgoing[0]).toMatchObject({ state: 'queued', prompt_id: 'p3' })
    // When the queued prompt starts, the transcript shows it and the placeholder goes.
    s = run([chat({ type: 'user_message', prompt_id: 'p3', text: 'hi', origin: 'remote', device_id: 'me', device_name: 'Chrome on macOS' })], s)
    expect(s.outgoing).toEqual([])

    s = run([{ type: 'outgoing', out: { seq: 200, op: 'prompt', client_msg_id: 'm-2', text: 'more', state: 'forwarded', prompt_id: null, problem: null } },
      { type: 'event', env: env('refused', { ref: { device_id: 'me', seq: 200 }, op: 'prompt', code: 'queue_full', message: 'full', last_seq: null }, 'me'), myDeviceId: 'me' }], s)
    expect(s.outgoing[0]).toMatchObject({ state: 'refused', problem: expect.stringMatching(/Five messages/) })
  })

  it('times out a command with no answer, but not one already answered', () => {
    const s = run([
      { type: 'outgoing', out: { seq: 1, op: 'prompt', client_msg_id: 'a', text: 'a', state: 'forwarded', prompt_id: null, problem: null } },
      { type: 'outgoing', out: { seq: 2, op: 'prompt', client_msg_id: 'b', text: 'b', state: 'queued', prompt_id: 'p', problem: null } },
      { type: 'outgoing_timeout', seq: 1 }, { type: 'outgoing_timeout', seq: 2 },
    ])
    expect(s.outgoing.map(o => o.state)).toEqual(['no_answer', 'queued'])
  })

  it('follows relay presence and endings, and keeps snapshots off the seq counter', () => {
    let s = run([{ type: 'relay', frame: { type: 'attached', online: true, oldest_seq: 1, latest_seq: 5 } }])
    expect(s).toMatchObject({ online: true, connection: 'live' })
    s = run([{ type: 'relay', frame: { type: 'offline' } }], s)
    expect(s.online).toBe(false)
    s = run([{ type: 'relay', frame: { type: 'gap', requested_seq: 1, oldest_seq: 9 } }], s)
    expect(s.items.at(-1)?.kind).toBe('gap')
    const before = s.lastAppliedSeq
    s = run([{ type: 'snapshot', env: env('status', { model: 'm', busy: true, context_pct: 1, cost_usd_micros: 5, project: null, ticket_id: null, branch: null, queue_len: 0, workers_running: 0, devices: [] }) }], s)
    expect(s.status?.model).toBe('m')
    expect(s.lastAppliedSeq).toBe(before)
    s = run([{ type: 'event', env: env('bye', { reason: 'idle' }), myDeviceId: 'me' }], s)
    expect(s.ended).toEqual({ reason: 'idle' })
  })

  it('stops applying events after a version mismatch', () => {
    const s = run([{ type: 'version_mismatch' }, chat({ type: 'notice', level: 'info', code: null, text: 'x' })])
    expect(s.items).toEqual([])
  })

  it('formats integer micro-dollars', () => {
    expect(formatCost(1_834_200)).toBe('$1.83')
    expect(formatCost(41_250)).toBe('$0.04')
    expect(formatCost(12_500_000)).toBe('$13')
    expect(formatCost(null)).toBe('—')
  })
})

describe('what the TUI shows besides (#2388)', () => {
  it('follows compaction, keeps the TUI\'s error sentence and refused line, and reads older bodies', () => {
    let s = run([chat({ type: 'compaction', active: true, manual: false })])
    expect(s.compaction).toMatchObject({ manual: false })
    s = run([chat({ type: 'compaction', active: false, manual: false })], s)
    expect(s.compaction).toBeNull()
    s = run([
      chat({ type: 'message_end', message_id: 'm1', stop_reason: 'error', error: 'http error 429: {"error":{}}', model: 'x', provider: 'openrouter',
        usage: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, cost_usd_micros: 0 },
        error_summary: 'OpenRouter rate-limited the request (429).' }),
      chat({ type: 'tool_end', tool_call_id: 'c1', name: 'amp_approve_plan', is_error: true, text: 'nope', truncated: false, full_bytes: 4,
        refused_line: 'AMP refused: this needs a person in the AMP web UI (below)' }),
      // An older harness: no new members.
      chat({ type: 'tool_end', tool_call_id: 'c2', name: 'bash', is_error: false, text: 'ok', truncated: false, full_bytes: 2 }),
    ])
    const a = s.items.find(i => i.kind === 'assistant')
    expect(a?.kind === 'assistant' && a.error_summary).toBe('OpenRouter rate-limited the request (429).')
    const tools = s.items.filter(i => i.kind === 'tool')
    expect(tools.map(t => t.kind === 'tool' && t.result?.refused_line)).toEqual(['AMP refused: this needs a person in the AMP web UI (below)', null])
  })

  it('a run that ends clears a compaction that never said it finished', () => {
    const s = run([chat({ type: 'run_start', run_id: 'r', prompt_id: 'p' }), chat({ type: 'compaction', active: true, manual: true }),
      chat({ type: 'run_end', run_id: 'r', outcome: 'aborted', error: null })])
    expect(s.compaction).toBeNull()
  })
})

describe('sessionTokens (#2388)', () => {
  const totals = { input: 10, output: 5, cache_read: 0, cache_write: 0, cost_usd_micros: 0, messages: 1 }
  it('prefers the harness\'s own totals, and falls back to the replies seen', () => {
    const status = { usage: { input_tokens: 120340, output_tokens: 8311, cache_read_tokens: 901200, cache_write_tokens: 0 } } as never
    expect(sessionTokens(status, totals)).toMatchObject({ total: 1_029_851, fromHarness: true })
    expect(sessionTokens(null, totals)).toMatchObject({ total: 15, fromHarness: false })
    expect(sessionTokens(null, { ...totals, input: 0, output: 0 })).toBeNull()
  })
})
