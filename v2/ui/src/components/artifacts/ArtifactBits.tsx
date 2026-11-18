import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { FileCode2, FileImage, FileText, Shapes } from 'lucide-react'
import { api } from '../../api/client'
import { useSSE } from '../../hooks/useSSE'
import { useStreamReconnect } from '../../hooks/useStreamReconnect'
import { STATUS_LABEL } from '../../lib/artifacts'
import type { Artifact, ArtifactKind, ArtifactStatus } from '../../types'

const STATUS_STYLE: Record<ArtifactStatus, React.CSSProperties> = {
  in_review: { background: 'var(--surface)', borderColor: 'var(--line)', color: 'var(--ink)' },
  approved: { background: 'var(--success)', borderColor: 'var(--success)', color: '#fff' },
  changes_requested: { background: 'var(--st-flag)', borderColor: 'var(--st-flag)', color: '#fff' },
}

export function ArtifactStatusTag({ status }: { status: ArtifactStatus }) {
  return (
    <span className="inline-flex items-center whitespace-nowrap border px-1.5 py-[4px] font-label text-[10.5px] font-semibold uppercase leading-none tracking-[0.1em]"
      style={{ borderRadius: 'var(--radius)', ...STATUS_STYLE[status] }}>
      {STATUS_LABEL[status]}
    </span>
  )
}

export function ArtifactKindIcon({ kind, size = 14 }: { kind: ArtifactKind; size?: number }) {
  const Icon = kind === 'html' ? FileCode2 : kind === 'image' ? FileImage : kind === 'svg' ? Shapes : FileText
  return <Icon size={size} className="text-ink-3 flex-shrink-0" aria-hidden="true" />
}

/** A ticket's artifacts, for the ticket drawer. Renders nothing when there are none. */
export function TicketArtifacts({ projectId, taskId }: { projectId: number; taskId: number }) {
  const [list, setList] = useState<Artifact[]>([])
  // The ticket whose read last settled, answered or failed. Until this ticket's
  // first read settles, a reconnect is dropped: that read is already fresh (#2539).
  const [settledFor, setSettledFor] = useState<number | null>(null)
  // Only the latest read may answer, so a slower earlier one (another ticket's,
  // or a re-read a newer one superseded) can't put an old list back.
  const latest = useRef(0)
  const load = useCallback(() => {
    const read = ++latest.current
    const settle = (artifacts: Artifact[]) => {
      if (read !== latest.current) return
      setList(artifacts)
      setSettledFor(taskId)
    }
    api.listArtifacts(projectId, taskId).then(settle, () => settle([]))
  }, [projectId, taskId])
  useEffect(load, [load])
  // Only this ticket's artifacts. Every tab gets every artifact event in the
  // project, and the payload is the artifact, whose task_id may be null (#2548).
  useSSE(projectId, e => {
    if (e.type === 'artifact.updated' && (e.payload as Artifact | undefined)?.task_id === taskId) load()
  })
  // An artifact published while the stream was down never arrives as an event.
  useStreamReconnect(load, settledFor === taskId)
  if (list.length === 0) return null
  return (
    <div className="px-6 py-3 border-b border-line grid gap-1.5">
      <span className="amp-label">Artifacts</span>
      {list.map(a => (
        <Link key={a.id} to={`/artifacts/${a.id}`} className="flex items-center gap-2 text-[13px] text-ink hover:text-link no-underline">
          <ArtifactKindIcon kind={a.kind} />
          <span className="truncate">{a.title}</span>
          <span className="amp-num text-ink-3 text-[11.5px]">v{a.current_version}</span>
          <span className="ml-auto"><ArtifactStatusTag status={a.status} /></span>
        </Link>
      ))}
    </div>
  )
}
