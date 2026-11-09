/**
 * The Get started checklist (#128), shown on an empty My work where the
 * queue will later be. Each step is read from the server
 * (GET /api/my-work/getting-started), so a step stays checked once it's
 * done, on every device. The fourth step, seeing a receipt, arrived with
 * verification receipts (#149). The one thing kept in this browser is that
 * you hid the list.
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { CheckCircle2, ChevronRight, Circle } from 'lucide-react'
import { api } from '../api/client'
import { CopyButton } from './admin'
import { ManagerAgent } from './ManagerAgent'
import { CONNECT_AGENT_DOC, RECEIPTS_DOC, mcpURL } from '../lib/guide'
import { projectLink } from '../lib/projectLink'
import { gettingStartedDone } from '../lib/gettingStarted'
import type { GettingStarted, GettingStartedStep } from '../types'

const HIDE_KEY = (who: string) => `amp.getStarted.hidden.${who}`
const hidden = (who: string) => { try { return localStorage.getItem(HIDE_KEY(who)) === '1' } catch { return false } }
const hide = (who: string) => { try { localStorage.setItem(HIDE_KEY(who), '1') } catch { /* this visit only */ } }

const CLIENT: Record<string, string> = { harness: 'the AMP Harness', mcp: 'an MCP client', token: 'an API token' }

/** The step count in words, so the heading reads "four steps" rather than "4 steps". */
const COUNT_WORD = ['no', 'one', 'two', 'three', 'four', 'five', 'six']
const inWords = (n: number) => COUNT_WORD[n] ?? String(n)

function ago(iso?: string): string {
  if (!iso) return ''
  const ms = Date.now() - new Date(iso).getTime()
  const n = (v: number, unit: string) => `${v} ${unit}${v === 1 ? '' : 's'} ago`
  if (ms < 3_600_000) return n(Math.max(1, Math.round(ms / 60_000)), 'minute')
  if (ms < 172_800_000) return n(Math.round(ms / 3_600_000), 'hour')
  return n(Math.round(ms / 86_400_000), 'day')
}

function Step({ n, title, step, children }: { n: number; title: string; step: GettingStartedStep; children: React.ReactNode }) {
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 px-5 py-3.5 border-t border-line-sub first:border-t-0"
      aria-label={`Step ${n}: ${title}, ${step.done ? 'done' : 'to do'}`}>
      <span className="row-span-2 pt-0.5" aria-hidden="true">
        {step.done
          ? <CheckCircle2 size={18} style={{ color: 'var(--success-ink)' }} />
          : <Circle size={18} className="text-ink-3" />}
      </span>
      <span className={`text-[14px] font-semibold ${step.done ? 'text-ink-2' : 'text-ink'}`}>{n}. {title}</span>
      <div className="text-[13px] text-ink-2 leading-normal grid gap-2 min-w-0">{children}</div>
    </li>
  )
}

const linkCls = 'inline-flex items-center gap-0.5 underline underline-offset-2 hover:text-ink'

/**
 * who keys the hidden flag: the user id, or local-dev when sign-in is off.
 * Renders nothing until the steps load, when they all are done, when the
 * list was hidden here, or when the server can't say (an older API).
 */
export function GetStarted({ who }: { who: string }) {
  const [g, setG] = useState<GettingStarted | null>(null)
  const [isHidden, setHidden] = useState(() => hidden(who))
  useEffect(() => {
    let live = true
    api.gettingStarted().then(r => { if (live) setG(r) }).catch(() => {})
    return () => { live = false }
  }, [])
  if (!g || isHidden || gettingStartedDone(g)) return null

  const steps = [g.agent, g.project, g.plan, g.receipt]
  const done = steps.filter(s => s.done).length
  const pid = g.plan.project_id || g.project.project_id
  // The receipt step's link opens the first receipt on the Board, where a
  // completed ticket's receipt heads its drawer.
  const receiptPid = g.receipt.project_id || pid
  return (
    <section className="amp-panel amp-panel-top" aria-labelledby="get-started-title">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-5 py-3.5 border-b border-line">
        <span className="amp-label">Get started</span>
        <h2 id="get-started-title" className="m-0 text-[17px] font-semibold text-ink">Set up AMP in {inWords(steps.length)} steps</h2>
        <span className="ml-auto amp-num text-[11px] text-ink-3">{done} of {steps.length} done</span>
        <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => { hide(who); setHidden(true) }}>Hide</button>
      </div>
      <ol className="list-none m-0 p-0">
        <Step n={1} title="Connect an agent" step={g.agent}>
          {g.agent.done ? (
            <p className="m-0">Connected{g.agent.client === 'token' ? '' : ` ${ago(g.agent.at)}`}{g.agent.client && CLIENT[g.agent.client] ? ` through ${CLIENT[g.agent.client]}` : ''}.</p>
          ) : <>
            <p className="m-0">Your agent harness talks to AMP over MCP at this address. Point it here and sign in; this step checks itself the first time it connects.</p>
            <div className="flex items-center gap-2">
              <code className="flex-1 min-w-0 px-3 py-2 text-[12px] amp-num text-ink break-all bg-surface-2 border border-line" aria-label="MCP address">{mcpURL()}</code>
              <CopyButton text={mcpURL()} label="Copy address" />
            </div>
            <Link to={CONNECT_AGENT_DOC} className={linkCls}>How to connect your agent<ChevronRight size={13} aria-hidden="true" /></Link>
          </>}
        </Step>
        <Step n={2} title="Create or describe a project" step={g.project}>
          {g.project.done && g.project.project_id ? (
            <p className="m-0">You have a project. <Link to={projectLink(g.project.project_id, 'board')} className={linkCls}>Open its Board<ChevronRight size={13} aria-hidden="true" /></Link></p>
          ) : <>
            <p className="m-0">Create one yourself, or describe what you want to your <ManagerAgent /> and it creates the project for you.</p>
            <Link to="/projects?new=1" className="amp-btn amp-btn-primary amp-btn-sm justify-self-start">Create a project</Link>
          </>}
        </Step>
        <Step n={3} title="Review and approve the agent's plan" step={g.plan}>
          {g.plan.done ? (
            <p className="m-0">You approved a plan {ago(g.plan.at)}.</p>
          ) : <>
            <p className="m-0">Ask your <ManagerAgent /> to plan the project. Its plan waits for you here and in the project's My work: read it, then Approve. Agents start only on approved tickets.</p>
            {pid ? <Link to={projectLink(pid, 'my-work')} className={linkCls}>Open the project's My work<ChevronRight size={13} aria-hidden="true" /></Link> : null}
          </>}
        </Step>
        <Step n={4} title="See your first receipt" step={g.receipt}>
          {g.receipt.done ? <>
            <p className="m-0">You have a receipt{g.receipt.at ? `, ${ago(g.receipt.at)}` : ''}: who did the work, where the code is, and how it was checked.</p>
            {g.receipt.task_id && receiptPid ? (
              <Link to={projectLink(receiptPid, 'board', g.receipt.task_id)} className={linkCls}>Open the receipt<ChevronRight size={13} aria-hidden="true" /></Link>
            ) : null}
          </> : <>
            <p className="m-0">A receipt shows who did a ticket, where the code is, and how it was checked. An agent records one when it finishes a ticket, once receipts are on in the project.</p>
            <Link to={RECEIPTS_DOC} className={linkCls}>How receipts work<ChevronRight size={13} aria-hidden="true" /></Link>
            {receiptPid ? <Link to={projectLink(receiptPid, 'settings')} className={linkCls}>Turn receipts on in Project settings<ChevronRight size={13} aria-hidden="true" /></Link> : null}
          </>}
        </Step>
      </ol>
    </section>
  )
}
