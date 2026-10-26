/**
 * My Work → My sessions: the caller's live remote-controlled harness sessions.
 * Rendered only when the capability flag says remote control is available.
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { MonitorSmartphone } from 'lucide-react'
import { Panel } from '../ui/PageHeader'
import { useRemote } from '../../remote/remote-context'
import type { SessionSummary } from '../../remote/types'

function since(ms: number): string {
  const d = Date.now() - ms
  if (d < 3_600_000) return `${Math.max(1, Math.round(d / 60_000))}m`
  return `${Math.round(d / 3_600_000)}h`
}

export function MySessionsPanel() {
  const { transport } = useRemote()
  const [sessions, setSessions] = useState<SessionSummary[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    const load = () => transport.listSessions()
      .then(s => { if (live) { setSessions(s); setError(null) } })
      .catch(e => { if (live) setError(e instanceof Error ? e.message : 'Failed to load sessions') })
    load()
    const t = setInterval(load, 30_000)
    return () => { live = false; clearInterval(t) }
  }, [transport])

  return (
    <Panel label="My sessions" title={sessions?.length ? 'Remote control' : 'No live sessions'} right={<>{sessions && <span>{sessions.length} live</span>}<Link to="/sessions" className="text-link hover:underline font-sans text-[12px]">All sessions</Link></>}>
      {error && <p className="m-0 px-4 py-3 text-[13px] text-danger-ink">{error}</p>}
      {!error && sessions?.length === 0 && (
        <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Run <code className="font-mono">/remote</code> in the harness on your laptop to drive that session from here or from your phone.</p>
      )}
      {sessions?.map(s => (
        // A full page load (not client-side routing), so the session page gets its strict CSP header.
        <a key={s.session_id} href={`/sessions/${encodeURIComponent(s.session_id)}`}
          className="grid grid-cols-[auto_1fr_auto] gap-x-2.5 gap-y-0.5 items-center px-4 py-2.5 border-t border-line first:border-t-0 hover:bg-surface-2">
          <MonitorSmartphone size={18} className="row-span-2 text-ink" aria-hidden="true" />
          <span className="text-[13px] font-semibold text-ink truncate">{s.host}</span>
          <span className={`amp-num text-[11px] ${s.online ? 'text-success-ink' : 'text-danger-ink'}`}>{s.online ? 'online' : 'offline'}</span>
          <span className="col-start-2 col-span-2 text-[11.5px] text-ink-2 truncate">
            {s.project ? s.project.name : 'no project'} · started {since(s.started_at_ms)} ago · key {s.harness_fp_display}
          </span>
        </a>
      ))}
    </Panel>
  )
}
