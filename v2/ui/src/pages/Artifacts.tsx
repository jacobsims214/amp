import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
import { ProjectNav } from '../components/ProjectNav'
import { ArtifactKindIcon, ArtifactStatusTag } from '../components/artifacts/ArtifactBits'
import { useSearchParamState, usePatchSearchParams, type ParamCodec } from '../hooks/useSearchParamState'
import { KIND_LABEL, STATUS_LABEL, expiresIn, expiryBucket, type ExpiryFilter } from '../lib/artifacts'
import { projectLink } from '../lib/projectLink'
import type { Artifact, ArtifactKind, ArtifactStatus } from '../types'
import { ManagerAgent } from '../components/ManagerAgent'

const FILTERS: (ArtifactStatus | 'all')[] = ['all', 'in_review', 'changes_requested', 'approved']
const KINDS = Object.keys(KIND_LABEL) as ArtifactKind[]
const EXPIRY_LABEL: Record<ExpiryFilter, string> = { all: 'Any expiry', soon: 'Expiring soon', kept: 'Kept' }

/** `?status=`, `?kind=` and `?expiry=`: one of a fixed set, anything else reads as the default. */
function choiceParam<T extends string>(def: T, allowed: readonly T[]): ParamCodec<T> {
  return { default: def, parse: raw => (allowed.includes(raw as T) ? (raw as T) : def), serialize: v => v }
}
const statusParam = choiceParam<ArtifactStatus | 'all'>('all', FILTERS)
const kindParam = choiceParam<ArtifactKind | 'all'>('all', ['all', ...KINDS])
const expiryParam = choiceParam<ExpiryFilter>('all', ['all', 'soon', 'kept'])
type SortKey = 'status' | 'updated' | 'ticket'
const SORT_LABEL: Record<SortKey, string> = { status: 'Waiting first', updated: 'Recently updated', ticket: 'Group by ticket' }
const sortParam = choiceParam<SortKey>('status', ['status', 'updated', 'ticket'])
const qParam: ParamCodec<string> = { default: '', parse: raw => raw, serialize: v => v }

/** A project's artifacts: mockups, diagrams, images and documents its agents
 *  published for review, waiting-for-you first. */
export function Artifacts() {
  const pid = Number(useParams().projectId)
  const [list, setList] = useState<Artifact[] | null>(null)
  const [filter, setFilter] = useSearchParamState('status', statusParam)
  const [kind, setKind] = useSearchParamState('kind', kindParam)
  const [expiry, setExpiry] = useSearchParamState('expiry', expiryParam)
  const [sort, setSort] = useSearchParamState('sort', sortParam)
  const [q, setQ] = useSearchParamState('q', qParam)
  const patch = usePatchSearchParams()
  const filtered = filter !== 'all' || kind !== 'all' || expiry !== 'all' || sort !== 'status' || q.trim() !== ''
  const load = useCallback(() => { api.listArtifacts(pid).then(setList).catch(() => setList([])) }, [pid])
  useEffect(load, [load])
  useSSE(pid, e => { if (e.type === 'artifact.updated') load() })

  const shown = useMemo(() => {
    const rank: Record<ArtifactStatus, number> = { in_review: 0, changes_requested: 1, approved: 2 }
    const needle = q.trim().toLowerCase()
    return (list ?? []).filter(a => filter === 'all' || a.status === filter)
      .filter(a => kind === 'all' || a.kind === kind)
      .filter(a => expiry === 'all' || expiryBucket(a) === expiry)
      .filter(a => !needle || [a.title, a.task_ref, a.task_name].some(t => t?.toLowerCase().includes(needle)))
      .sort((a, b) => {
        const recent = b.updated_at.localeCompare(a.updated_at)
        if (sort === 'updated') return recent
        if (sort === 'ticket') {
          if (!a.task_ref !== !b.task_ref) return a.task_ref ? -1 : 1
          return (a.task_ref ?? '').localeCompare(b.task_ref ?? '', undefined, { numeric: true }) || recent
        }
        return rank[a.status] - rank[b.status] || recent
      })
  }, [list, filter, kind, expiry, sort, q])

  // Sorting by ticket lays the list out as one section per ticket (artifacts with no ticket last).
  const groups = useMemo(() => {
    if (sort !== 'ticket') return [{ key: 'all', label: null as string | null, taskId: null as number | null, items: shown }]
    const out: { key: string; label: string | null; taskId: number | null; items: Artifact[] }[] = []
    for (const a of shown) {
      const key = a.task_ref ?? 'none'
      let g = out.find(x => x.key === key)
      if (!g) {
        g = { key, label: a.task_ref ? (a.task_name ? `${a.task_ref} ${a.task_name}` : a.task_ref) : 'No ticket', taskId: a.task_ref ? a.task_id : null, items: [] }
        out.push(g)
      }
      g.items.push(a)
    }
    return out
  }, [shown, sort])

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
        {list != null && list.length > 0 && (
          <div className="flex flex-wrap items-center gap-2 mb-4">
            <input type="search" value={q} onChange={e => setQ(e.target.value)} placeholder="Find by title or ticket" aria-label="Find artifacts"
              className="amp-input text-[13px] w-[260px]" />
            <select value={kind} onChange={e => setKind(e.target.value as ArtifactKind | 'all')} aria-label="Kind" className="amp-input text-[13px]">
              <option value="all">All kinds</option>
              {KINDS.map(k => <option key={k} value={k}>{KIND_LABEL[k]}</option>)}
            </select>
            <select value={expiry} onChange={e => setExpiry(e.target.value as ExpiryFilter)} aria-label="Expiry" className="amp-input text-[13px]">
              {(Object.keys(EXPIRY_LABEL) as ExpiryFilter[]).map(x => <option key={x} value={x}>{EXPIRY_LABEL[x]}</option>)}
            </select>
            <select value={sort} onChange={e => setSort(e.target.value as SortKey)} aria-label="Sort" className="amp-input text-[13px]">
              {(Object.keys(SORT_LABEL) as SortKey[]).map(x => <option key={x} value={x}>{SORT_LABEL[x]}</option>)}
            </select>
            <span className="text-[12px] text-ink-3" aria-live="polite">{shown.length} of {list.length}</span>
            {filtered && (
              <button type="button" onClick={() => patch({ status: null, kind: null, expiry: null, sort: null, q: null })}
                className="text-[12px] text-ink-2 underline bg-transparent border-0 cursor-pointer">Clear filters</button>
            )}
          </div>
        )}
        {list == null ? (
          <div className="flex justify-center py-16"><Loader2 size={20} className="animate-spin text-ink-2" /></div>
        ) : shown.length === 0 ? (
          <div className="amp-panel p-8 max-w-[640px] grid gap-2">
            <h2 className="amp-heading m-0 text-[17px]">{list.length ? 'No artifacts match these filters' : 'No artifacts yet'}</h2>
            <p className="m-0 text-[13px] text-ink-2 leading-relaxed">
              Artifacts are what agents make for you to look at before code: UI mockups, prototypes, diagrams, images and documents.
              Ask your <ManagerAgent /> to put a design step in the plan, or have any agent call <code>amp_publish_artifact</code>.
              Each one is versioned here, and you approve it or ask for changes.
            </p>
          </div>
        ) : (
          <div className="grid gap-5">
            {groups.map(g => (
              <section key={g.key} aria-label={g.label ?? undefined} className="grid gap-2">
                {g.label != null && (
                  <h2 className="amp-heading m-0 text-[13px] flex items-center gap-2">
                    {g.taskId != null
                      ? <Link to={projectLink(pid, 'board', g.taskId)} className="amp-num text-ink underline">{g.label}</Link>
                      : g.label}
                    <span className="text-[12px] font-normal text-ink-3">{g.items.length}</span>
                  </h2>
                )}
                <div className="grid gap-3 [grid-template-columns:repeat(auto-fill,minmax(280px,1fr))]">
                  {g.items.map(a => <ArtifactCard key={a.id} a={a} onKept={load} />)}
                </div>
              </section>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

function ArtifactCard({ a, onKept }: { a: Artifact; onKept: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const soon = expiryBucket(a) === 'soon'
  const keep = async () => {
    setBusy(true); setError(null)
    try { await api.keepArtifact(a.id, true); onKept() }
    catch (e) { setError(e instanceof Error ? e.message : "Couldn't keep it — try again") }
    finally { setBusy(false) }
  }
  const visual = a.kind === 'image' || a.kind === 'svg'
  return (
    <div className="amp-panel text-ink hover:border-ink-3 flex flex-col overflow-hidden">
      <Link to={`/artifacts/${a.id}`} className="no-underline text-ink flex flex-col" aria-label={a.title}>
        <div className="relative h-[150px] bg-surface-2 border-b border-line flex items-center justify-center overflow-hidden">
          {visual
            ? <img src={api.artifactContentUrl(a.id, a.current_version)} alt="" className="max-h-full max-w-full object-contain" loading="lazy" />
            : a.kind === 'html'
              ? <iframe src={api.artifactContentUrl(a.id, a.current_version)} title="" tabIndex={-1} sandbox="allow-scripts" loading="lazy"
                  className="absolute left-0 top-0 pointer-events-none origin-top-left border-0 bg-white" style={{ width: '400%', height: '400%', transform: 'scale(0.25)' }} />
              : <ArtifactKindIcon kind={a.kind} size={28} />}
        </div>
        <div className="px-3.5 pt-3.5 flex items-start gap-2">
          <span className="font-semibold text-[13.5px] leading-snug flex-1">{a.title}</span>
          <ArtifactStatusTag status={a.status} />
        </div>
      </Link>
      <div className="p-3.5 pt-1.5 grid gap-1.5">
        <span className="text-[12px] text-ink-2">
          {KIND_LABEL[a.kind]} · <Link to={`/artifacts/${a.id}?v=${a.current_version}`} title="Open version history" className="amp-num text-ink-2 underline">v{a.current_version}</Link>
          {a.task_ref && a.task_id != null
            ? <> · <Link to={projectLink(a.project_id, 'board', a.task_id)} title={a.task_name} className="amp-num text-ink-2 underline">{a.task_ref}</Link></>
            : null}
          {a.created_by_email ? <> · {a.created_by_email}</> : null}
        </span>
        <span className="text-[11.5px] text-ink-3 flex items-center gap-2 flex-wrap">
          {a.keep ? 'Kept' : `Expires ${expiresIn(a.expires_at)}`}
          {soon && (
            <button type="button" onClick={keep} disabled={busy} aria-label={`Keep ${a.title}`}
              className="text-[11.5px] text-ink underline bg-transparent border-0 p-0 cursor-pointer disabled:opacity-50">Keep</button>
          )}
        </span>
        {error && <span role="alert" className="text-[11.5px] text-danger-ink leading-snug">{error}</span>}
      </div>
    </div>
  )
}
