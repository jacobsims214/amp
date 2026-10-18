import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
import { ProjectNav } from '../components/ProjectNav'
import { ArtifactKindIcon, ArtifactStatusTag } from '../components/artifacts/ArtifactBits'
import { KIND_LABEL, STATUS_LABEL, expiresIn } from '../lib/artifacts'
import type { Artifact, ArtifactStatus } from '../types'

const FILTERS: (ArtifactStatus | 'all')[] = ['all', 'in_review', 'changes_requested', 'approved']

/** A project's artifacts: mockups, diagrams, images and documents its agents
 *  published for review, waiting-for-you first. */
export function Artifacts() {
  const pid = Number(useParams().projectId)
  const [list, setList] = useState<Artifact[] | null>(null)
  const [filter, setFilter] = useState<ArtifactStatus | 'all'>('all')
  const load = useCallback(() => { api.listArtifacts(pid).then(setList).catch(() => setList([])) }, [pid])
  useEffect(load, [load])
  useSSE(pid, e => { if (e.type === 'artifact.updated') load() })

  const shown = useMemo(() => {
    const rank: Record<ArtifactStatus, number> = { in_review: 0, changes_requested: 1, approved: 2 }
    return (list ?? []).filter(a => filter === 'all' || a.status === filter)
      .sort((a, b) => rank[a.status] - rank[b.status] || b.updated_at.localeCompare(a.updated_at))
  }, [list, filter])

  return (
    <div className="flex flex-col min-h-full">
      <ProjectNav projectId={pid} currentView="artifacts" rightSlot={
        <div className="flex border border-line" role="group" aria-label="Status">
          {FILTERS.map(f => (
            <button key={f} type="button" onClick={() => setFilter(f)} aria-pressed={filter === f}
              className={`font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] px-2.5 py-2 border-l border-line first:border-l-0 cursor-pointer ${filter === f ? 'bg-ink text-on-ink' : 'bg-surface text-ink-2 hover:text-ink'}`}>
              {f === 'all' ? 'All' : STATUS_LABEL[f]}
            </button>
          ))}
        </div>
      } />
      <div className="p-7 pt-5">
        {list == null ? (
          <div className="flex justify-center py-16"><Loader2 size={20} className="animate-spin text-ink-2" /></div>
        ) : shown.length === 0 ? (
          <div className="amp-panel p-8 max-w-[640px] grid gap-2">
            <h2 className="amp-heading m-0 text-[17px]">{list.length ? 'Nothing here with that status' : 'No artifacts yet'}</h2>
            <p className="m-0 text-[13px] text-ink-2 leading-relaxed">
              Artifacts are what agents make for you to look at before code: UI mockups, prototypes, diagrams, images and documents.
              Ask your manager agent to put a design step in the plan, or have any agent call <code>amp_publish_artifact</code>.
              Each one is versioned here, and you approve it or ask for changes.
            </p>
          </div>
        ) : (
          <div className="grid gap-3 [grid-template-columns:repeat(auto-fill,minmax(280px,1fr))]">
            {shown.map(a => <ArtifactCard key={a.id} a={a} />)}
          </div>
        )}
      </div>
    </div>
  )
}

function ArtifactCard({ a }: { a: Artifact }) {
  const visual = a.kind === 'image' || a.kind === 'svg'
  return (
    <Link to={`/artifacts/${a.id}`} className="amp-panel no-underline text-ink hover:border-ink-3 flex flex-col overflow-hidden">
      <div className="relative h-[150px] bg-surface-2 border-b border-line flex items-center justify-center overflow-hidden">
        {visual
          ? <img src={api.artifactContentUrl(a.id, a.current_version)} alt="" className="max-h-full max-w-full object-contain" loading="lazy" />
          : a.kind === 'html'
            ? <iframe src={api.artifactContentUrl(a.id, a.current_version)} title="" tabIndex={-1} sandbox="allow-scripts" loading="lazy"
                className="absolute left-0 top-0 pointer-events-none origin-top-left border-0 bg-white" style={{ width: '400%', height: '400%', transform: 'scale(0.25)' }} />
            : <ArtifactKindIcon kind={a.kind} size={28} />}
      </div>
      <div className="p-3.5 grid gap-1.5">
        <div className="flex items-start gap-2">
          <span className="font-semibold text-[13.5px] leading-snug flex-1">{a.title}</span>
          <ArtifactStatusTag status={a.status} />
        </div>
        <span className="text-[12px] text-ink-2">
          {KIND_LABEL[a.kind]} · v{a.current_version}{a.task_ref ? <> · <span className="amp-num">{a.task_ref}</span></> : null}
          {a.created_by_email ? <> · {a.created_by_email}</> : null}
        </span>
        <span className="text-[11.5px] text-ink-3">{a.keep ? 'Kept' : `Expires ${expiresIn(a.expires_at)}`}</span>
      </div>
    </Link>
  )
}
