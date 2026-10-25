/**
 * Tool calls in words, the way the harness TUI shows them (amp-tui `amp_render.rs` and
 * `view.rs`): a title (`complete_task #2078`), the call's most telling argument, a one-line
 * summary of the result (`3 tickets: 1 ready, 1 waiting`, `120 lines`), and durations as
 * `12 ms`, `1.2s`, `1m5s`. Everything here works on untrusted text and returns plain strings;
 * the caller renders them as text, never as HTML.
 */

type Json = null | boolean | number | string | Json[] | { [k: string]: Json }
type Obj = { [k: string]: Json }

const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v)

/** Control characters out, whitespace collapsed (the TUI's `clean`). */
export function clean(s: string): string {
  // eslint-disable-next-line no-control-regex
  return s.replace(/[\u0000-\u001f\u007f-\u009f‪-‮⁦-⁩]/g, ' ').replace(/\s+/g, ' ').trim()
}

/** At most `n` characters, with an ellipsis when cut. */
export function shorten(s: string, n: number): string {
  const chars = Array.from(s)
  return chars.length <= n ? s : `${chars.slice(0, Math.max(1, n - 1)).join('')}…`
}

/** `amp_complete_task` → `complete_task` (an `amp_amp_` harness prefix is dropped too). */
export function shortTool(name: string): string {
  const n = name.startsWith('amp_amp_') ? name.slice('amp_amp_'.length) : name
  return n.startsWith('amp_') ? n.slice(4) : n
}

export const isAmpTool = (name: string) => name.startsWith('amp_')

/** The tools whose line carries a worker's live status under it (#2347). */
export const WORKER_TOOLS = ['amp_run_ticket', 'amp_dispatch_task']

function idOf(v: Json | undefined): number | null {
  if (typeof v === 'number' && Number.isInteger(v)) return v
  if (typeof v === 'string') {
    const t = v.trim().replace(/^#+/, '')
    return /^-?\d+$/.test(t) ? Number(t) : null
  }
  return null
}

/** Parses the call's `arguments_json`; anything but an object is no arguments. */
export function parseArgs(json: string): Obj {
  try {
    const v = JSON.parse(json) as unknown
    return isObj(v) ? v : {}
  } catch {
    return {}
  }
}

/** The tickets a call names: its `task_id`, else its `task_ids`. */
export function ticketIds(args: Obj): number[] {
  const one = idOf(args.task_id)
  if (one != null) return [one]
  return Array.isArray(args.task_ids) ? args.task_ids.map(idOf).filter((n): n is number => n != null) : []
}

function ticketRefs(args: Obj): string {
  const ids = ticketIds(args)
  if (idOf(args.task_id) != null) return `#${ids[0]}`
  const out = ids.slice(0, 3).map(id => `#${id}`)
  if (ids.length > 3) out.push(`+${ids.length - 3}`)
  return out.join(' ')
}

/** The tool line's title: `complete_task #2078`, `create_tasks 8 tickets`, or the tool's name. */
export function toolTitle(name: string, args: Obj): string {
  if (!isAmpTool(name)) return clean(name)
  let title = shortTool(name)
  const refs = ticketRefs(args)
  if (refs) title += ` ${refs}`
  else if (Array.isArray(args.tasks)) title += ` ${args.tasks.length} ticket${args.tasks.length === 1 ? '' : 's'}`
  return clean(title)
}

const AMP_ARG_KEYS = ['query', 'path', 'name', 'title', 'state', 'reason', 'answer', 'body', 'note']
const OTHER_ARG_KEYS = ['path', 'pattern', 'command', 'url', 'query', 'file_path']

/** What follows the title: the call's most telling argument, on one line. */
export function toolArgs(name: string, args: Obj): string {
  const amp = isAmpTool(name)
  for (const k of amp ? AMP_ARG_KEYS : OTHER_ARG_KEYS) {
    const v = args[k]
    if (typeof v === 'string' && v.trim()) return shorten(clean(v.split('\n')[0] ?? ''), amp ? 72 : 120)
  }
  return ''
}

const count = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`
const size = (v: Json | undefined): number => Array.isArray(v) ? v.length : typeof v === 'number' ? Math.max(0, Math.trunc(v)) : 0

/** A ticket's state in the web UI's words (amp-core `Status::of(..).label()`). */
function stateWord(task: Obj): string | null {
  const state = typeof task.state === 'string' ? task.state : null
  if (!state) return null
  const reason = typeof task.block_reason === 'string' ? task.block_reason : ''
  switch (state) {
    case 'completed': return 'done'
    case 'in_progress': return 'in progress'
    case 'scheduled': return 'scheduled'
    case 'blocked': return reason.startsWith('scheduled:') ? 'scheduled' : reason ? 'question' : 'waiting'
    default: return 'ready'
  }
}

/** An AMP result as `key: value · ...` (the TUI's `result_text`). */
function resultText(raw: string): string {
  let v: unknown
  try { v = JSON.parse(raw.trim()) } catch { return raw }
  if (!isObj(v)) return raw
  const records: string[] = []
  const parts: string[] = []
  for (const [k, val] of Object.entries(v)) {
    if (k === 'effective_metadata') continue
    let record = false
    let text: string
    if (val === null || val === '' || (Array.isArray(val) && !val.length) || (isObj(val) && !Object.keys(val).length)) continue
    if (typeof val === 'boolean' || typeof val === 'number') text = String(val)
    else if (typeof val === 'string') text = shorten(val, 60)
    else if (Array.isArray(val)) text = count(val.length, 'item', 'items')
    else {
      const id = typeof val.id === 'number' ? val.id : null
      const nm = typeof val.name === 'string' ? val.name : typeof val.title === 'string' ? val.title : null
      record = id != null
      text = id != null && nm != null ? `#${id} ${shorten(nm, 48)}` : id != null ? `#${id}` : count(Object.keys(val).length, 'field', 'fields')
    }
    ;(record ? records : parts).push(`${k}: ${clean(text)}`)
  }
  const all = [...records, ...parts]
  return all.length ? all.join(' · ') : 'ok'
}

function firstLine(text: string): string {
  return clean(text.split('\n').find(l => l.trim()) ?? '')
}

/** An error without the transport's wrapping (`... Mcp error: -32603: ` before AMP's sentence). */
export function errorText(raw: string): string {
  let text = raw.trim()
  const at = text.indexOf('Mcp error: ')
  if (at >= 0) {
    text = text.slice(at + 'Mcp error: '.length)
    const m = /^(-?\d+): /.exec(text)
    if (m) text = text.slice(m[0].length)
  }
  return text
}

export const errorLine = (raw: string) => firstLine(errorText(raw))

function listTasks(v: Obj): string | null {
  if (typeof v.count !== 'number') return null
  const total = v.count
  if (total === 0) return 'no tickets'
  const questions = size(v.questions)
  const waiting = Array.isArray(v.blocked) ? Math.max(0, v.blocked.length - questions) : size(v.waiting)
  const buckets: [number, string][] = [
    [size(v.ready_to_dispatch), 'ready'], [size(v.in_progress), 'in progress'], [questions, 'question'],
    [waiting, 'waiting'], [size(v.scheduled), 'scheduled'], [size(v.completed), 'done'],
  ]
  const parts = buckets.filter(([n]) => n > 0).map(([n, w]) => w === 'question' ? count(n, 'question', 'questions') : `${n} ${w}`)
  let out = count(total, 'ticket', 'tickets')
  if (parts.length) out += `: ${parts.join(', ')}`
  const people = size(v.waiting_on_people)
  if (people > 0) out += ` · ${people} for people`
  return out
}

function getPlan(v: Obj): string | null {
  if (!Array.isArray(v.epics)) return null
  const epics = v.epics
  const stories = epics.reduce<number>((n, e) => n + (isObj(e) ? size(e.stories) : 0), 0)
  const tail = `${count(stories, 'story', 'stories')}, ${count(typeof v.tickets === 'number' ? v.tickets : 0, 'ticket', 'tickets')}`
  if (epics.length === 1 && isObj(epics[0]) && typeof epics[0].name === 'string') return `${shorten(clean(epics[0].name), 40)}: ${tail}`
  return `${count(epics.length, 'epic', 'epics')}, ${tail}`
}

function getTask(v: Obj): string | null {
  const task = v.task
  if (!isObj(task) || typeof task.name !== 'string') return null
  const parts = [shorten(clean(task.name), 60)]
  const st = stateWord(task)
  if (st) parts.push(st)
  if (typeof v.comments_total === 'number' && v.comments_total > 0) parts.push(count(v.comments_total, 'comment', 'comments'))
  return parts.join(' · ')
}

function kbSearch(v: Obj): string | null {
  if (!Array.isArray(v.results)) return null
  if (!v.results.length) return 'no results'
  const paths: string[] = []
  for (const r of v.results) if (isObj(r) && typeof r.path === 'string' && !paths.includes(r.path)) paths.push(r.path)
  let out = count(v.results.length, 'result', 'results')
  if (paths.length) out += `: ${clean(paths.join(', '))}`
  return out
}

function createTasks(v: Obj): string | null {
  if (!Array.isArray(v.created)) return null
  const ids = v.created.map(t => isObj(t) ? idOf(t.id) : null).filter((n): n is number => n != null).map(id => `#${id}`)
  let out = `created ${count(v.created.length, 'ticket', 'tickets')}`
  if (ids.length) out += `: ${ids.slice(0, 6).join(' ')}${ids.length > 6 ? ` +${ids.length - 6}` : ''}`
  const warnings = size(v.warnings)
  if (Array.isArray(v.warnings) && warnings > 0) out += ` · ${count(warnings, 'warning', 'warnings')}`
  return out
}

function checkPlan(v: Obj): string | null {
  if (typeof v.ok !== 'boolean') return null
  const issues = size(v.issues)
  const parts = [v.ok && issues === 0 ? 'plan OK' : v.ok ? `plan OK, ${count(issues, 'note', 'notes')}` : `NO-GO: ${count(issues, 'issue', 'issues')}`]
  const ready = size(v.ready_now)
  if (ready > 0) parts.push(`${ready} ready now`)
  const awaiting = size(v.awaiting_approval)
  if (awaiting > 0) {
    if (Array.isArray(v.awaiting_yours) && Array.isArray(v.awaiting_approval_tickets)) {
      const yours = v.awaiting_yours.length
      const others = Math.max(0, awaiting - yours)
      parts.push(others === 0 ? `${yours} of yours awaiting approval`
        : yours === 0 ? `${others} others' awaiting their owners' approval`
        : `${yours} of yours awaiting approval, ${others} others'`)
    } else parts.push(`${awaiting} awaiting approval`)
  }
  return parts.join(' · ')
}

function listArtifacts(v: Obj): string | null {
  if (!Array.isArray(v.artifacts)) return null
  if (!v.artifacts.length) return 'no artifacts'
  const by = new Map<string, number>()
  for (const a of v.artifacts) {
    const s = isObj(a) && typeof a.status === 'string' ? a.status.replace(/_/g, ' ') : ''
    by.set(s, (by.get(s) ?? 0) + 1)
  }
  const parts = [...by].filter(([s]) => s).map(([s, n]) => `${n} ${clean(s)}`)
  return count(v.artifacts.length, 'artifact', 'artifacts') + (parts.length ? `: ${parts.join(', ')}` : '')
}

function projectText(p: Json | undefined): string | null {
  if (!isObj(p) || typeof p.name !== 'string') return null
  const id = idOf(p.id)
  let out = id != null ? `#${id} ${shorten(clean(p.name), 48)}` : shorten(clean(p.name), 48)
  if (typeof p.code === 'string' && p.code) out += ` (${clean(p.code)})`
  return out
}

/** One line saying what an AMP tool call did (the TUI's `result_summary`). */
export function ampResultSummary(name: string, raw: string, isError: boolean): string {
  if (isError) return errorLine(raw)
  const fallback = () => firstLine(resultText(raw))
  let parsed: unknown
  try { parsed = JSON.parse(raw.trim()) } catch { return fallback() }
  if (!isObj(parsed)) return fallback()
  const v = parsed
  let s: string | null = null
  switch (shortTool(name)) {
    case 'list_tasks': s = listTasks(v); break
    case 'get_plan': s = getPlan(v); break
    case 'get_task': s = getTask(v); break
    case 'kb_search': s = kbSearch(v); break
    case 'create_tasks': s = createTasks(v); break
    case 'check_plan': s = checkPlan(v); break
    case 'list_artifacts': s = listArtifacts(v); break
    case 'whoami':
      if (typeof v.email === 'string') {
        s = clean(v.email)
        const orgs = size(v.orgs)
        if (Array.isArray(v.orgs) && orgs > 0) s += ` · ${count(orgs, 'org', 'orgs')}`
        if (v.global_admin === true) s += ' · admin'
      }
      break
    case 'get_project': {
      const p = projectText(v.project)
      const st = isObj(v.project) && typeof v.project.state === 'string' && v.project.state ? clean(v.project.state) : null
      s = p && st ? `${p} · ${st}` : p
      break
    }
    case 'list_projects': s = Array.isArray(v.projects) ? count(v.projects.length, 'project', 'projects') : null; break
    case 'create_project': { const p = projectText(v); s = p ? `created ${p}` : null; break }
    case 'dispatch_task': s = typeof v.dispatched_to === 'string' && v.dispatched_to ? `dispatched to ${clean(v.dispatched_to)}` : 'dispatched'; break
    case 'complete_task':
      s = 'done'
      if (isObj(v.run)) s += ' · run recorded'
      else if (typeof v.run_error === 'string') s += ` · run not recorded: ${clean(v.run_error)}`
      break
    case 'block_task': s = 'blocked: the question is on the ticket'; break
    case 'report_run': s = typeof v.outcome === 'string' && v.outcome ? `run recorded: ${clean(v.outcome)}` : 'run recorded'; break
    case 'answer_question':
      s = v.state === 'backlog' ? 'answered · ready to dispatch' : v.state === 'blocked' ? 'answered · still waits on dependencies' : 'answered'
      break
    case 'kb_write': s = typeof v.path === 'string' ? `wrote ${clean(v.path)}` : null; break
  }
  return s ?? fallback()
}

/** A non-AMP tool's result in one line: a short single line as is, else its line count. */
export function outputSummary(text: string): string {
  const lines = text.replace(/\s+$/, '').split('\n')
  if (lines.length === 1 && !lines[0].trim()) return 'no output'
  if (lines.length === 1 && Array.from(lines[0].trim()).length <= 60) return clean(lines[0])
  return count(lines.length, 'line', 'lines')
}

/** The summary on a finished call's line (a failed call: its error's first line). */
export function resultSummary(name: string, text: string, isError: boolean): string {
  if (isAmpTool(name)) return ampResultSummary(name, text, isError)
  return isError ? errorLine(text) : outputSummary(text)
}

/** The expanded result: AMP JSON pretty-printed, an AMP error without its wrapping, else as is. */
export function resultDetail(name: string, text: string, isError: boolean): string {
  if (!isAmpTool(name)) return text
  if (isError) return errorText(text)
  try { return JSON.stringify(JSON.parse(text.trim()), null, 2) } catch { return text }
}

/** A finished call's duration: `12 ms` under a second, then `1.2s`, `1m5s`. */
export function formatElapsed(ms: number): string {
  const d = Math.max(0, ms)
  if (d < 1000) return `${Math.floor(d)} ms`
  if (d < 60_000) return `${(d / 1000).toFixed(1)}s`
  return `${Math.floor(d / 60_000)}m${Math.floor((d % 60_000) / 1000)}s`
}

/** A running timer: nothing for the first second, then whole seconds (`5s`, `1m5s`). */
export function runningElapsed(ms: number): string {
  const s = Math.floor(Math.max(0, ms) / 1000)
  if (s === 0) return ''
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60}s`
}

/** A span of whole seconds for people: `45s`, `3m12s`, `1h05m`. */
export function formatSpan(ms: number): string {
  const s = Math.floor(Math.max(0, ms) / 1000)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s`
  return `${Math.floor(s / 3600)}h${String(Math.floor((s % 3600) / 60)).padStart(2, '0')}m`
}

/** `48k tok`, `1.2M tok`. */
export function tokLabel(n: number): string {
  if (n < 1000) return `${n} tok`
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k tok`
  return `${(n / 1_000_000).toFixed(1)}M tok`
}

/** `amp:amp-worker-backend` → `worker-backend`; `anthropic/claude-opus-5-5` → `claude-opus-5-5`. */
export const shortRole = (role: string) => clean(role).replace(/^amp:/, '').replace(/^amp-/, '')
export const shortModel = (model: string) => clean(model).split('/').pop() ?? model

/** The last sentence of streaming thinking, for its collapsed line. */
export function latestSentence(text: string): string {
  const t = clean(text)
  const parts = t.split(/(?<=[.!?])\s+/).filter(Boolean)
  return parts.at(-1) ?? ''
}

/** `3m ago`, `2h ago`, `just now`. */
export function ago(ms: number, now = Date.now()): string {
  const d = now - ms
  if (d < 60_000) return 'just now'
  if (d < 3_600_000) return `${Math.round(d / 60_000)}m ago`
  if (d < 86_400_000) return `${Math.round(d / 3_600_000)}h ago`
  return `${Math.round(d / 86_400_000)}d ago`
}

/** `in 4h 10m`, `in 25m`, `now`. */
export function inTime(ms: number, now = Date.now()): string {
  const d = ms - now
  if (d <= 60_000) return 'now'
  const mins = Math.round(d / 60_000)
  const h = Math.floor(mins / 60)
  const m = mins % 60
  return h ? `in ${h}h ${String(m).padStart(2, '0')}m` : `in ${m}m`
}
