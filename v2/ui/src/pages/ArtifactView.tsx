import { useCallback, useEffect, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { Check, ExternalLink, Loader2, Monitor, Smartphone, Tablet, Undo2 } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
import { PageHeader } from '../components/ui/PageHeader'
import { Markdown } from '../components/ui/Markdown'
import { ArtifactKindIcon, ArtifactStatusTag } from '../components/artifacts/ArtifactBits'
import { KIND_LABEL, expiresIn } from '../lib/artifacts'
import type { Artifact, ArtifactVersion, AuditEntry, Comment, Task } from '../types'

const VIEWPORTS = [
  { id: 'desktop', label: 'Desktop', width: '100%', Icon: Monitor },
  { id: 'tablet', label: 'Tablet', width: '820px', Icon: Tablet },
  { id: 'phone', label: 'Phone', width: '390px', Icon: Smartphone },
] as const
type Viewport = typeof VIEWPORTS[number]['id']

function when(iso: string) {
  return new Date(iso).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
}

function size(n: number) {
  return n < 1024 ? `${n} B` : n < 1 << 20 ? `${(n / 1024).toFixed(1)} KB` : `${(n / (1 << 20)).toFixed(1)} MB`
}

/** One artifact: the content (sandboxed), its versions, the review, and the
 *  ticket's conversation, where review feedback goes. */
export function ArtifactView() {
  const id = Number(useParams().artifactId)
  const [params, setParams] = useSearchParams()
  const [artifact, setArtifact] = useState<Artifact | null>(null)
  const [versions, setVersions] = useState<ArtifactVersion[]>([])
  const [error, setError] = useState<string | null>(null)
  const [viewport, setViewport] = useState<Viewport>('desktop')

  const load = useCallback(() => {
    api.getArtifact(id).then(r => { setArtifact(r.artifact); setVersions(r.versions); setError(null) })
      .catch(e => setError(e instanceof Error ? e.message : 'Could not load the artifact'))
  }, [id])
  useEffect(load, [load])
  useSSE(artifact?.project_id ?? null, e => {
    if (e.type === 'artifact.updated' && (e.payload as Artifact | undefined)?.id === id) load()
  })

  const latest = artifact?.current_version ?? 0
  const version = Number(params.get('v')) || latest
  const selected = versions.find(v => v.version === version)
  const pick = (v: number) => setParams(v === latest ? {} : { v: String(v) }, { replace: true })

  if (error) {
    return <div className="p-7"><p className="text-danger-ink text-[13px]">{error}</p></div>
  }
  if (!artifact) {
    return <div className="flex justify-center py-16"><Loader2 size={20} className="animate-spin text-ink-2" /></div>
  }
  const src = api.artifactContentUrl(artifact.id, version)

  return (
    <div className="flex flex-col h-full min-h-0">
      <PageHeader
        crumb={<><Link to={`/project/${artifact.project_id}/artifacts`} className="text-inherit no-underline hover:text-ink">Artifacts</Link>
          {artifact.task_ref ? <> · <span className="amp-num tracking-normal">{artifact.task_ref}</span></> : null}</>}
        title={<span className="inline-flex items-center gap-2.5"><ArtifactKindIcon kind={artifact.kind} size={18} />{artifact.title}</span>}
        actions={<>
          <ArtifactStatusTag status={artifact.status} />
          <select className="amp-input !w-auto !py-1.5 text-[12.5px]" value={version} onChange={e => pick(Number(e.target.value))} aria-label="Version">
            {versions.map(v => (
              <option key={v.version} value={v.version}>
                v{v.version}{v.version === latest ? ' (latest)' : ''}{v.version === artifact.approved_version ? ' · approved' : ''}
              </option>
            ))}
          </select>
          <a className="amp-btn amp-btn-sm" href={src} target="_blank" rel="noopener noreferrer"><ExternalLink size={12} />Open</a>
        </>}
      />
      <div className="flex-1 min-h-0 grid lg:grid-cols-[minmax(0,1fr)_340px]">
        <section className="min-h-[60vh] lg:min-h-0 flex flex-col border-b lg:border-b-0 lg:border-r border-line bg-surface-2">
          {artifact.kind === 'html' && (
            <div className="flex items-center gap-1 px-4 py-2 border-b border-line bg-surface" role="group" aria-label="Viewport">
              {VIEWPORTS.map(v => (
                <button key={v.id} type="button" onClick={() => setViewport(v.id)} aria-pressed={viewport === v.id}
                  className={`amp-btn amp-btn-sm ${viewport === v.id ? 'amp-btn-primary' : 'amp-btn-ghost'}`}>
                  <v.Icon size={12} />{v.label}
                </button>
              ))}
              <span className="ml-auto text-[11.5px] text-ink-3">Runs sandboxed: it can't see your AMP session.</span>
            </div>
          )}
          <div className="flex-1 min-h-0 overflow-auto flex justify-center p-4">
            <ArtifactContent artifact={artifact} src={src} width={VIEWPORTS.find(v => v.id === viewport)!.width} />
          </div>
        </section>
        <aside className="min-h-0 overflow-y-auto">
          <ReviewPanel artifact={artifact} version={version} selected={selected} onReviewed={a => { setArtifact(a); pick(a.current_version); load() }} />
          <RetentionPanel artifact={artifact} onChange={setArtifact} />
          {selected && (
            <div className="px-5 py-4 border-t border-line grid gap-1 text-[12.5px] text-ink-2">
              <span className="amp-label">Version {selected.version}</span>
              {selected.note && <p className="m-0 text-ink">{selected.note}</p>}
              <span>{selected.created_by_email || 'Someone'} · {when(selected.created_at)} · {size(selected.size_bytes)} · {KIND_LABEL[artifact.kind]}</span>
            </div>
          )}
          <ActivityPanel artifactId={artifact.id} updatedAt={artifact.updated_at} />
          {artifact.task_id != null && <TicketThread projectId={artifact.project_id} taskId={artifact.task_id} />}
        </aside>
      </div>
    </div>
  )
}

const ACTIVITY_LABEL: Record<string, string> = {
  'artifact.publish': 'published', 'artifact.view': 'viewed', 'artifact.approve': 'approved',
  'artifact.request_changes': 'requested changes', 'artifact.keep': 'kept it', 'artifact.unkeep': 'stopped keeping it',
}

/** The audit trail: who published, reviewed, kept and viewed it, and when.
 *  Only its creator, its ticket's owner and project leads see this; for
 *  everyone else the panel isn't shown. */
function ActivityPanel({ artifactId, updatedAt }: { artifactId: number; updatedAt: string }) {
  const [entries, setEntries] = useState<AuditEntry[] | null>(null)
  useEffect(() => {
    api.artifactActivity(artifactId).then(setEntries).catch(() => setEntries(null))
  }, [artifactId, updatedAt])
  if (!entries) return null
  return (
    <div className="px-5 py-4 border-t border-line grid gap-2">
      <span className="amp-label">Activity</span>
      <ol className="m-0 p-0 list-none grid gap-1.5 text-[12px] text-ink-2">
        {entries.slice(0, 15).map(e => {
          const d = e.details as { version?: number; via?: string } | undefined
          return (
            <li key={e.id}>
              <span className="text-ink">{e.actor_email || 'AMP'}</span> {ACTIVITY_LABEL[e.action] ?? e.action.replace('artifact.', '')}
              {d?.version ? <> v{d.version}</> : null}
              {e.action === 'artifact.view' && d?.via === 'mcp' ? <> (agent)</> : null}
              <span className="text-ink-3"> · {when(e.at)}</span>
            </li>
          )
        })}
      </ol>
    </div>
  )
}

/** Keep it forever, or let it expire on schedule. */
function RetentionPanel({ artifact, onChange }: { artifact: Artifact; onChange: (a: Artifact) => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const toggle = async () => {
    setBusy(true); setError(null)
    try { onChange(await api.keepArtifact(artifact.id, !artifact.keep)) }
    catch (e) { setError(e instanceof Error ? e.message : 'Could not change it') }
    finally { setBusy(false) }
  }
  return (
    <div className="px-5 py-4 border-t border-line grid gap-2">
      <span className="amp-label">Retention</span>
      <p className="m-0 text-[12.5px] text-ink-2 leading-relaxed">
        {artifact.keep
          ? <>Kept{artifact.kept_at ? <> since {when(artifact.kept_at)}</> : null}: it won't expire.</>
          : <>Expires <b className="font-semibold text-ink">{expiresIn(artifact.expires_at)}</b>
              {artifact.expires_at ? <> ({new Date(artifact.expires_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })})</> : null}
              {' '}unless someone keeps it. Each new version pushes this back.</>}
      </p>
      <button type="button" className="amp-btn amp-btn-sm justify-self-start" disabled={busy} onClick={toggle}>
        {busy && <Loader2 size={11} className="animate-spin" />}{artifact.keep ? 'Stop keeping' : 'Keep it'}
      </button>
      {error && <p className="m-0 text-[12.5px] text-danger-ink">{error}</p>}
    </div>
  )
}

function ArtifactContent({ artifact, src, width }: { artifact: Artifact; src: string; width: string }) {
  const [text, setText] = useState<string | null>(null)
  const textual = artifact.kind === 'markdown' || artifact.kind === 'text'
  useEffect(() => {
    if (!textual) return
    let live = true
    fetch(src).then(r => r.text()).then(t => { if (live) setText(t) }).catch(() => { if (live) setText('') })
    return () => { live = false }
  }, [src, textual])

  switch (artifact.kind) {
    case 'html':
      return (
        <iframe key={src} src={src} title={artifact.title} sandbox="allow-scripts allow-forms allow-popups allow-modals"
          className="bg-white border border-line shadow-sm h-full min-h-[560px]" style={{ width, maxWidth: '100%' }} />
      )
    case 'svg':
    case 'image':
      return <img src={src} alt={artifact.title} className="max-w-full max-h-full object-contain self-center bg-white border border-line" />
    case 'pdf':
      return <iframe key={src} src={src} title={artifact.title} className="w-full h-full min-h-[640px] border border-line bg-white" />
    case 'markdown':
      return <article className="w-full max-w-[760px] bg-surface border border-line px-8 py-6 self-start">{text == null ? <Loader2 size={16} className="animate-spin" /> : <Markdown>{text}</Markdown>}</article>
    default:
      return <pre className="w-full max-w-[960px] self-start bg-surface border border-line p-5 text-[12.5px] whitespace-pre-wrap">{text}</pre>
  }
}

function ReviewPanel({ artifact, version, selected, onReviewed }: {
  artifact: Artifact; version: number; selected?: ArtifactVersion; onReviewed: (a: Artifact) => void
}) {
  const [asking, setAsking] = useState(false)
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const latest = version === artifact.current_version

  const review = async (approve: boolean) => {
    setBusy(true); setError(null)
    try {
      const a = await api.reviewArtifact(artifact.id, { approve, note: note.trim() || undefined, version })
      setAsking(false); setNote('')
      onReviewed(a)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not save the review')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="px-5 py-4 grid gap-3">
      <span className="amp-label">Review</span>
      {artifact.status === 'approved' && artifact.approved_version != null && (
        <p className="m-0 text-[13px] text-ink">
          v{artifact.approved_version} approved{artifact.reviewed_by_email ? <> by {artifact.reviewed_by_email}</> : null}
          {artifact.reviewed_at ? <> · {when(artifact.reviewed_at)}</> : null}.
        </p>
      )}
      {artifact.status === 'changes_requested' && (
        <p className="m-0 text-[13px] text-ink">
          Changes requested on v{artifact.current_version}{artifact.reviewed_by_email ? <> by {artifact.reviewed_by_email}</> : null}. The agent picks up the feedback from the ticket.
        </p>
      )}
      {!latest ? (
        <p className="m-0 text-[12.5px] text-ink-2">You're looking at v{version}. Reviews apply to the latest version, v{artifact.current_version}.</p>
      ) : (
        <>
          {artifact.status === 'in_review' && (
            <p className="m-0 text-[12.5px] text-ink-2">
              {artifact.task_id != null
                ? 'Approve it, or say what to change. Your verdict goes on the ticket, and if the agent is waiting for review it goes back to ready.'
                : 'Approve it, or say what to change.'}
            </p>
          )}
          {asking ? (
            <div className="grid gap-2">
              <label className="amp-label" htmlFor="artifact-changes">What should change?</label>
              <textarea id="artifact-changes" className="amp-input min-h-[110px] resize-y" value={note} onChange={e => setNote(e.target.value)}
                placeholder="Be specific: what, where, and why." autoFocus />
              <div className="flex gap-2">
                <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" disabled={busy || !note.trim()} onClick={() => review(false)}>
                  {busy && <Loader2 size={11} className="animate-spin" />}Request changes
                </button>
                <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={busy} onClick={() => { setAsking(false); setError(null) }}>Cancel</button>
              </div>
            </div>
          ) : (
            <div className="flex gap-2 flex-wrap">
              {!(artifact.status === 'approved' && artifact.approved_version === version) && (
                <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" disabled={busy} onClick={() => review(true)}>
                  {busy ? <Loader2 size={11} className="animate-spin" /> : <Check size={12} />}Approve v{version}
                </button>
              )}
              <button type="button" className="amp-btn amp-btn-sm" disabled={busy} onClick={() => setAsking(true)}><Undo2 size={12} />Request changes</button>
            </div>
          )}
        </>
      )}
      {error && <p className="m-0 text-[12.5px] text-danger-ink">{error}</p>}
      {selected == null && <p className="m-0 text-[12.5px] text-ink-3">Loading versions…</p>}
    </div>
  )
}

/** The ticket the artifact belongs to, and its comments: publishes, reviews
 *  and discussion all land here, so agents read them too. */
function TicketThread({ projectId, taskId }: { projectId: number; taskId: number }) {
  const [task, setTask] = useState<Task | null>(null)
  const [comments, setComments] = useState<Comment[]>([])
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const load = useCallback(() => {
    api.getTask(taskId).then(setTask).catch(() => setTask(null))
    api.getComments(taskId).then(setComments).catch(() => setComments([]))
  }, [taskId])
  useEffect(load, [load])
  useSSE(projectId, e => {
    if (['comment.added', 'task.updated', 'task.blocked', 'task.unblocked', 'task.completed', 'artifact.updated'].includes(e.type)) load()
  })
  const send = async () => {
    if (!draft.trim()) return
    setBusy(true)
    try { await api.addComment(taskId, draft.trim()); setDraft(''); load() } finally { setBusy(false) }
  }
  return (
    <div className="px-5 py-4 border-t border-line grid gap-3">
      <div className="flex items-center gap-2">
        <span className="amp-label">Ticket</span>
        {task && <Link to={`/project/${projectId}/board?task=${taskId}`} className="text-[12.5px] text-link truncate">{task.name}</Link>}
      </div>
      {task?.state === 'blocked' && task.block_reason && (
        <p className="m-0 text-[12.5px] text-ink border-l-2 pl-2.5" style={{ borderColor: 'var(--st-flag)' }}>Waiting on a person: {task.block_reason}</p>
      )}
      <ol className="m-0 p-0 list-none grid gap-2.5">
        {comments.slice(-12).map(c => (
          <li key={c.id} className="grid gap-0.5">
            <span className="text-[11.5px] text-ink-3">{c.author} · {when(c.created_at)}</span>
            <div className="text-[12.5px] text-ink [&_p]:m-0"><Markdown>{c.body}</Markdown></div>
          </li>
        ))}
      </ol>
      <div className="grid gap-2">
        <textarea className="amp-input min-h-[64px] resize-y text-[12.5px]" value={draft} onChange={e => setDraft(e.target.value)} placeholder="Comment on the ticket…" aria-label="Comment" />
        <button type="button" className="amp-btn amp-btn-sm justify-self-start" disabled={busy || !draft.trim()} onClick={send}>Comment</button>
      </div>
    </div>
  )
}
