import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Loader2 } from 'lucide-react'
import { api } from '../../api/client'
import { Panel } from '../ui/PageHeader'
import { expiresIn } from '../../lib/artifacts'
import { ArtifactKindIcon } from './ArtifactBits'
import type { Artifact } from '../../types'

/** Home page warning: artifacts I made, or on my tickets, that expire in the
 *  next two weeks because nobody kept them. Keep each, or let it go. Renders
 *  nothing when there's nothing to decide. */
export function ExpiringArtifacts() {
  const [list, setList] = useState<Artifact[]>([])
  const [busy, setBusy] = useState<number | null>(null)
  const load = useCallback(() => { api.expiringArtifacts(14).then(setList).catch(() => setList([])) }, [])
  useEffect(load, [load])
  if (list.length === 0) return null

  const act = async (a: Artifact, keep: boolean) => {
    setBusy(a.id)
    try {
      if (keep) await api.keepArtifact(a.id, true)
      else await api.letArtifactExpire(a.id)
      setList(l => l.filter(x => x.id !== a.id))
    } finally {
      setBusy(null)
    }
  }

  return (
    <Panel label="Artifacts" title={`${list.length} expiring soon`} right={<span>keep it, or let it go</span>}>
      {list.map(a => (
        <div key={a.id} className="grid grid-cols-[18px_1fr_auto] gap-x-3 gap-y-1 items-start px-4 py-3 border-t border-line first:border-t-0">
          <span className="pt-0.5"><ArtifactKindIcon kind={a.kind} /></span>
          <div className="min-w-0 grid gap-0.5">
            <Link to={`/artifacts/${a.id}`} className="text-[13px] font-semibold text-ink hover:underline truncate">{a.title}</Link>
            <span className="text-[12px] text-ink-2">
              Expires <b className="font-semibold text-ink">{expiresIn(a.expires_at)}</b>
              {a.task_ref ? <> · ticket <span className="amp-num">{a.task_ref}</span>{a.task_name ? <> {a.task_name}</> : null}</> : null}
              {a.status === 'approved' ? <> · approved v{a.approved_version}</> : null}
            </span>
          </div>
          <div className="flex gap-1.5">
            <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" disabled={busy === a.id} onClick={() => act(a, true)}>
              {busy === a.id && <Loader2 size={11} className="animate-spin" />}Keep
            </button>
            <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" disabled={busy === a.id} onClick={() => act(a, false)}>Let it expire</button>
          </div>
        </div>
      ))}
    </Panel>
  )
}
