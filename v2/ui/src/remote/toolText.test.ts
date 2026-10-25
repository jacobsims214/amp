import { describe, expect, it } from 'vitest'
import {
  ampResultSummary, errorLine, formatElapsed, outputSummary, parseArgs, resultDetail, resultSummary, runningElapsed,
  shortTool, ticketIds, toolArgs, toolTitle,
} from './toolText'
import { blockText, initialSessionState, sessionReducer, type SessionState, type TranscriptItem } from './session'
import { thinkingLabel, workerLine } from './transcriptView'
import parity from './mock/parityStream.json'
import type { ChatEvent, Envelope } from './types'

/**
 * parityStream.json is the manager feed recorded from the real harness chat (amp-tui Shell on the
 * faux provider, #2386). The TUI drew these lines for that very stream (harness-play/rc-m1/
 * ui-parity/tui/tui-08-full.txt); the web must say the same things.
 */
function replay(): SessionState {
  let s = initialSessionState
  ;(parity as { at_ms: number; event: ChatEvent }[]).forEach((e, i) => {
    const env = { v: 1, alg: 'ES256', session_id: 's', harness_id: 'h', device_id: null, seq: i + 1, ts: e.at_ms,
      nonce: 'n', op: 'chat_event', body: e.event, sig: 'x' } as Envelope
    s = sessionReducer(s, { type: 'event', env, myDeviceId: null })
  })
  return s
}

type Tool = Extract<TranscriptItem, { kind: 'tool' }>
const toolLine = (t: Tool) => {
  const args = parseArgs(t.arguments_json)
  return [toolTitle(t.name, args), toolArgs(t.name, args), t.result ? resultSummary(t.name, t.result.text, t.result.is_error) : '']
    .filter(Boolean).join(' · ')
}

describe('tool lines in the TUI\'s words', () => {
  it('names AMP calls the way the TUI does', () => {
    expect(shortTool('amp_complete_task')).toBe('complete_task')
    expect(shortTool('amp_amp_list_tasks')).toBe('list_tasks')
    expect(toolTitle('amp_get_task', { task_id: 514 })).toBe('get_task #514')
    expect(toolTitle('amp_handover_tasks', { task_ids: [1, 2, 3, 4, 5] })).toBe('handover_tasks #1 #2 #3 +2')
    expect(toolTitle('amp_create_tasks', { tasks: [{}, {}] })).toBe('create_tasks 2 tickets')
    expect(toolTitle('bash', {})).toBe('bash')
    expect(toolArgs('bash', { command: 'make test\nmore' })).toBe('make test')
    expect(toolArgs('amp_kb_get', { path: 'architecture/x.md' })).toBe('architecture/x.md')
    expect(ticketIds({ task_id: '#218' })).toEqual([218])
  })

  it('summarises results in one line', () => {
    expect(outputSummary('')).toBe('no output')
    expect(outputSummary('ok')).toBe('ok')
    expect(outputSummary('a\nb\nc\n')).toBe('3 lines')
    expect(ampResultSummary('amp_list_tasks', '{"count":0}', false)).toBe('no tickets')
    expect(ampResultSummary('amp_kb_write', '{"path":"a.md"}', false)).toBe('wrote a.md')
    expect(ampResultSummary('amp_complete_task', '{"run":{}}', false)).toBe('done · run recorded')
    expect(ampResultSummary('amp_whatever', '{"task":{"id":3,"name":"X"},"ok":true}', false)).toBe('task: #3 X · ok: true')
    expect(errorLine('tool error: amp_x: tools/call amp_x: Mcp error: -32603: doc not found\nmore')).toBe('doc not found')
    expect(resultDetail('amp_get_plan', '{"epics":[]}', false)).toBe('{\n  "epics": []\n}')
  })

  it('times calls as the TUI does', () => {
    expect(formatElapsed(12)).toBe('12 ms')
    expect(formatElapsed(1702)).toBe('1.7s')
    expect(formatElapsed(65_400)).toBe('1m5s')
    expect(runningElapsed(400)).toBe('')
    expect(runningElapsed(3_200)).toBe('3s')
  })
})

describe('the recorded harness feed reads like the TUI', () => {
  const s = replay()
  const tools = s.items.filter((i): i is Tool => i.kind === 'tool')

  it('says what each call did, with its duration', () => {
    const lines = tools.map(toolLine)
    // The TUI's lines (tui-08-full.txt), minus the width cuts it makes for an 100-column terminal.
    expect(lines).toEqual([
      'get_plan · 0 epics, 0 stories, 0 tickets',
      'kb_search · Go feed API NATS JetStream docker compose · 2 results: architecture/feed-api.md, how-to/docker.md',
      'list_tasks · 7 tickets: 1 ready, 2 in progress, 1 question, 2 waiting',
      'create_epic · Small Go feed API · id: 76 · name: Small Go feed API',
      'create_story · MVP · id: 78 · name: MVP',
      'create_tasks 8 tickets · created 8 tickets: #514 #515 #516 #517 #518 #519 +2',
      'get_task #514 · Research JetStream APIs and version pins · in progress · 3 comments',
      'check_plan · NO-GO: 1 issue · 1 ready now · 3 awaiting approval',
      'assign_task #526 · "me" needs a signed-in AMP user',
      'bash · python -m pytest -q mathx · 30 lines',
    ])
    const took = tools.map(t => formatElapsed(t.endedAt! - t.startedAt!))
    expect(took[0]).toBe('12 ms') // TUI: get_plan · … · 12 ms
    expect(took[1]).toBe('1.7s') // TUI: kb_search · … · 1.7s
    expect(took[8]).toBe('6 ms') // TUI: assign_task #526 · … · 6 ms
    expect(took[9]).toBe('2.3s') // TUI: bash · … · 2.3s
    expect(tools[8].result?.is_error).toBe(true)
  })

  it('folds thinking as "Thought for Ns"', () => {
    const thinking = s.items.flatMap(i => i.kind === 'assistant' ? i.blocks.filter(b => b.kind === 'thinking') : [])
    // TUI: "▸ Thought for 4s", then "▸ Thought for 1s".
    expect(thinking.map(b => thinkingLabel(b, Date.now()))).toEqual(['Thought for 4s', 'Thought for 1s'])
  })

  it('ends runs as the shell says it: nothing, "Stopped." once, the reply\'s errors', () => {
    const ends = s.items.filter(i => i.kind === 'run_end')
    expect(ends.map(e => e.kind === 'run_end' && [e.outcome, e.covered])).toEqual([
      ['completed', true], ['completed', true], ['aborted', true], ['failed', true],
    ])
    expect(s.items.filter(i => i.kind === 'notice').map(n => n.kind === 'notice' && n.text)).toEqual(['Stopped.'])
    const errors = s.items.filter(i => i.kind === 'assistant' && i.error && i.stop_reason === 'error')
    expect(errors).toHaveLength(2) // TUI: "✗ Error: 429 …", then "✗ Error: No more faux responses queued"
    expect(s.items.filter(i => i.kind === 'retry')).toHaveLength(1)
  })

  it('keeps the remote turn\'s device', () => {
    const users = s.items.filter(i => i.kind === 'user')
    expect(users.map(u => u.kind === 'user' && [u.text, u.origin, u.device_name])).toEqual([
      ['plan out a small Go feed API with NATS JetStream in docker compose', 'local', null],
      ['run the tests', 'remote', 'Safari on iPhone'],
      ['summarise the plan', 'remote', 'Safari on iPhone'],
      ['try again', 'local', null],
    ])
    const reply = s.items.find(i => i.kind === 'assistant' && blockText(i, 'text').startsWith('Created the plan'))
    expect(reply).toBeDefined()
  })

  it('draws a worker the way the roster chip does', () => {
    const w = { task_id: 218, name: 'Add the clamp helper', role: 'amp:amp-worker-backend', state: 'running' as const, model: 'wf-1', started_at_ms: 1000 }
    expect(workerLine(w, 4_200, true)).toBe('#218 Add the clamp helper · worker-backend (wf-1) · running 3s')
    expect(workerLine({ ...w, state: 'done' }, 4_200, false)).toBe('Add the clamp helper · worker-backend (wf-1) · done')
  })
})
