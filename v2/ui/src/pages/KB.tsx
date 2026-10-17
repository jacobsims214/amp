import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  Plus, Search, FileText, Tag, Trash2, Edit3, Save, X,
  Loader2, ChevronRight, ChevronDown, BookOpen, Eye, Code2, FolderOpen, Folder, Clock, MessageSquare,
   Building2, Users, Lock, Info, CheckCircle2, RotateCcw,
} from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import { api } from '../api/client'
import { PageHeader } from '../components/ui/PageHeader'
import { ProjectNav } from '../components/ProjectNav'
import type { KBDoc, KBDocSummary, KBInfo, KBRef, KBScope, KBSearchResult, KBSource, KBTagCount } from '../types'

// What each kind of KB is for; shown on team and org KB pages and in the editor.
const SCOPE_PURPOSE: Record<KBScope, string> = {
  project: "This project's architecture, decisions, and gotchas.",
  team: 'Knowledge for every project this team works on: runbooks, services the team owns, team conventions.',
  org: 'Knowledge for the whole org: brand and design system, process, standards, policy.',
}

type View = 'browse' | 'doc' | 'editor'

// Tags are labels, not states: one quiet style for all of them.
const TAG_STYLE = { bg: 'var(--surface)', text: 'var(--ink-2)', ring: 'var(--line-str)' }
const tagColor = () => TAG_STYLE

function readingTime(content: string) {
  const words = content.trim().split(/\s+/).length
  const mins = Math.max(1, Math.round(words / 200))
  return `${mins} min read`
}

const inputCls = 'amp-input'

// One page for every knowledge base: /project/:projectId/kb, /teams/:teamId/kb, /orgs/:orgId/kb.
export function KB() {
  const { projectId, teamId, orgId } = useParams<{ projectId?: string; teamId?: string; orgId?: string }>()
  const pid = projectId ? Number(projectId) : null
  const kbRef: KBRef = useMemo(
    () => teamId ? { team_id: Number(teamId) } : orgId ? { org_id: Number(orgId) } : { project_id: Number(projectId) },
    [projectId, teamId, orgId],
  )
  const [info, setInfo] = useState<KBInfo | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const canWrite = info?.can_write ?? false
  const shared = info != null && info.scope !== 'project'

  const [view, setView] = useState<View>('browse')
  const [docs, setDocs] = useState<KBDocSummary[]>([])
  const [tags, setTags] = useState<KBTagCount[]>([])
  const [selectedDoc, setSelectedDoc] = useState<KBDoc | null>(null)
  const [searchQuery, setSearchQuery] = useState('')
  const [searchResults, setSearchResults] = useState<KBSearchResult[]>([])
  const [searching, setSearching] = useState(false)
  const [loading, setLoading] = useState(true)
  const [tagFilter, setTagFilter] = useState<string | null>(null)
  const [expandedDirs, setExpandedDirs] = useState<Set<string>>(new Set(['architecture', 'docs', '']))

  // Editor state
  const [editPath, setEditPath] = useState('')
  const [editTitle, setEditTitle] = useState('')
  const [editContent, setEditContent] = useState('')
  const [editTags, setEditTags] = useState('')
  const [editReason, setEditReason] = useState('')
  const [saving, setSaving] = useState(false)
  const [editorTab, setEditorTab] = useState<'write' | 'preview'>('write')

  useEffect(() => {
    setInfo(null); setLoadError(null); setSelectedDoc(null); setView('browse')
    api.kbInfo(kbRef).then(setInfo).catch(e => setLoadError(e instanceof Error ? e.message : 'Failed to load'))
  }, [kbRef])

  const loadDocs = useCallback(async () => {
    setLoading(true)
    try {
      const [docList, tagList] = await Promise.all([
        api.kbList(kbRef, tagFilter ?? undefined),
        api.kbTags(kbRef),
      ])
      setDocs(docList)
      setTags(tagList)
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : 'Failed to load')
    } finally {
      setLoading(false)
    }
  }, [kbRef, tagFilter])

  useEffect(() => { loadDocs() }, [loadDocs])

  // Debounced search
  useEffect(() => {
    if (!searchQuery.trim()) { setSearchResults([]); return }
    const t = setTimeout(async () => {
      setSearching(true)
      try { setSearchResults(await api.kbSearch(kbRef, searchQuery)) }
      catch { setSearchResults([]) }
      finally { setSearching(false) }
    }, 300)
    return () => clearTimeout(t)
  }, [searchQuery, kbRef])

  const openDoc = async (path: string) => {
    setActionError(null)
    try {
      setSelectedDoc(await api.kbGet(kbRef, path))
      setView('doc')
      setSearchQuery('') // close search results
    } catch (e) {
      setActionError(e instanceof Error ? e.message : 'Failed to open the document')
    }
  }

  const openEditor = (doc?: KBDoc | null) => {
    if (doc) {
      setEditPath(doc.path); setEditTitle(doc.title)
      setEditContent(doc.content); setEditTags(doc.tags.join(', ')); setEditReason(doc.reason ?? '')
    } else {
      setEditPath(''); setEditTitle(''); setEditContent(''); setEditTags(''); setEditReason('')
    }
    setActionError(null)
    setEditorTab('write')
    setView('editor')
  }

  const saveDoc = async () => {
    if (!editPath || !editTitle || !editContent) return
    setSaving(true)
    setActionError(null)
    try {
      const parsedTags = editTags.split(',').map(t => t.trim()).filter(Boolean)
      await api.kbWrite(kbRef, {
        path: editPath, title: editTitle, content: editContent, tags: parsedTags,
        reason: shared ? editReason.trim() : undefined,
      })
      await loadDocs()
      const doc = await api.kbGet(kbRef, editPath)
      setSelectedDoc(doc); setView('doc')
    } catch (e) {
      setActionError(e instanceof Error ? e.message : 'Save failed')
    } finally { setSaving(false) }
  }

  const deleteDoc = async (path: string) => {
    if (!confirm(`Delete "${path}"?`)) return
    setActionError(null)
    try {
      await api.kbDelete(kbRef, path)
      setSelectedDoc(null); setView('browse'); loadDocs()
    } catch (e) {
      setActionError(e instanceof Error ? e.message : 'Delete failed')
    }
  }

  const newDocButton = canWrite ? (
    <button
      onClick={() => openEditor(null)}
      className="amp-btn"
    >
      <Plus size={12} />
      New Doc
    </button>
  ) : info ? (
    <span className="flex items-center gap-1.5 text-xs text-ink-2" title={info.scope === 'org' ? 'Only org owners and admins edit the org KB; you can annotate.' : undefined}>
      <Lock size={11} />Read-only{info.scope === 'org' ? ' · you can annotate' : ''}
    </span>
  ) : null

  if (loadError && !info) {
    return (
      <div className="flex flex-col h-full" style={{ background: 'var(--bg)' }}>
        {pid ? <ProjectNav projectId={pid} currentView="kb" /> : <ScopeHeader info={null} />}
        <div className="flex-1 flex items-center justify-center p-8">
          <p className="text-sm text-danger-ink max-w-md text-center">{loadError}</p>
        </div>
      </div>
    )
  }

  const tree = buildTree(docs)
  const isSearching = searchQuery.trim().length > 0

  return (
    <div className="flex flex-col h-full" style={{ background: 'var(--bg)' }}>
      {/* Header */}
      {pid
        ? <ProjectNav projectId={pid} currentView="kb" rightSlot={newDocButton} />
        : <ScopeHeader info={info} rightSlot={newDocButton} />}

      <div className="flex flex-1 overflow-hidden">
        {/* ── Sidebar ────────────────────────────────────────────────── */}
        <aside className="flex flex-col flex-shrink-0 overflow-hidden"
          style={{ width: 248, borderRight: '1px solid var(--line)', background: 'var(--surface)' }}>

          {/* Search */}
          <div className="px-3 pt-3 pb-2 flex-shrink-0">
            <div className="relative">
              <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-ink-3 pointer-events-none" />
              <input
                type="text"
                placeholder="Search docs…"
                value={searchQuery}
                onChange={e => setSearchQuery(e.target.value)}
                className="w-full text-xs rounded-lg pl-8 pr-8 py-2 transition-all focus:outline-none"
                style={{
                  background: 'var(--bg)',
                  border: '1px solid var(--line)',
                  color: 'var(--ink)',
                  fontFamily: 'inherit',
                }}
                onFocus={e => (e.target.style.borderColor = 'var(--primary)')}
                onBlur={e => (e.target.style.borderColor = 'var(--line)')}
              />
              {searching && <Loader2 size={12} className="absolute right-2.5 top-1/2 -translate-y-1/2 text-primary animate-spin" />}
              {searchQuery && !searching && (
                <button onClick={() => setSearchQuery('')}
                  className="absolute right-2.5 top-1/2 -translate-y-1/2 text-ink-3 hover:text-ink cursor-pointer">
                  <X size={11} />
                </button>
              )}
            </div>
          </div>

          {/* Tags */}
          {tags.length > 0 && !isSearching && (
            <div className="px-3 pb-2 flex-shrink-0">
              <div className="text-[10px] font-bold text-ink-3 uppercase tracking-widest mb-2 flex items-center gap-1.5">
                <Tag size={9} />
                Tags
              </div>
              <div className="flex flex-wrap gap-1">
                <button
                  onClick={() => setTagFilter(null)}
                  className={`text-xs px-2 py-0.5 rounded-md transition-all cursor-pointer font-medium ${
                    !tagFilter
                      ? 'bg-surface-3 text-ink'
                      : 'text-ink-3 hover:text-ink-2'
                  }`}
                >all</button>
                {tags.slice(0, 12).map(t => {
                  const tc = tagColor()
                  const isActive = tagFilter === t.tag
                  return (
                    <button
                      key={t.tag}
                      onClick={() => setTagFilter(tagFilter === t.tag ? null : t.tag)}
                      className="text-xs px-2 py-0.5 rounded-md transition-all cursor-pointer font-medium flex items-center gap-1"
                      style={{
                        background: isActive ? tc.bg : 'transparent',
                        color: isActive ? tc.text : 'var(--ink-3)',
                        border: isActive ? `1px solid ${tc.ring}` : '1px solid transparent',
                      }}
                      onMouseEnter={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = tc.text }}
                      onMouseLeave={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = 'var(--ink-3)' }}
                    >
                      {t.tag}
                      <span style={{ opacity: 0.6 }}>{t.count}</span>
                    </button>
                  )
                })}
              </div>
            </div>
          )}

          {/* Divider */}
          {!isSearching && <div className="mx-3 border-t border-line-sub mb-1 flex-shrink-0" />}

          {/* File tree / search results */}
          <div className="flex-1 overflow-y-auto px-2 py-1">
            {isSearching ? (
              <SearchResultsList
                query={searchQuery}
                results={searchResults}
                searching={searching}
                onSelect={openDoc}
                selectedPath={selectedDoc?.path}
              />
            ) : loading ? (
              <div className="flex justify-center py-6">
                <Loader2 size={15} className="animate-spin text-ink-2" />
              </div>
            ) : docs.length === 0 ? (
              <div className="text-center py-8 px-3">
                <BookOpen size={20} className="text-line-str mx-auto mb-2" />
                <p className="text-xs text-ink-3">No docs yet</p>
                {canWrite && <p className="text-xs text-ink-3 mt-0.5">Click "New Doc" to start.</p>}
              </div>
            ) : (
              <TreeNode
                node={tree} depth={0}
                expanded={expandedDirs}
                onToggleDir={(dir) => setExpandedDirs(prev => {
                  const s = new Set(prev); s.has(dir) ? s.delete(dir) : s.add(dir); return s
                })}
                onSelectDoc={openDoc}
                selectedPath={selectedDoc?.path}
              />
            )}
          </div>

          {pid != null && <OtherKBs projectId={pid} />}
        </aside>

        {/* ── Main content ───────────────────────────────────────────── */}
        <main className="flex-1 overflow-y-auto">
          {actionError && view !== 'editor' && (
            <p className="mx-6 mt-4 text-xs text-danger-ink">{actionError}</p>
          )}
          {view === 'browse' && (
            <>
              {shared && info && (
                <div className="mx-6 mt-6 flex items-start gap-2.5 rounded-xl px-4 py-3 max-w-4xl"
                  style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}>
                  <Info size={13} className="text-ink-2 mt-0.5 flex-shrink-0" />
                  <p className="text-xs text-ink-2 leading-relaxed">
                    {SCOPE_PURPOSE[info.scope]} Anything specific to one project belongs in that project's KB.
                  </p>
                </div>
              )}
              <BrowseView docs={docs} loading={loading} tagFilter={tagFilter} onOpen={openDoc} onNew={canWrite ? () => openEditor(null) : undefined} />
            </>
          )}
          {view === 'doc' && selectedDoc && (
            <DocView doc={selectedDoc} canWrite={canWrite} onEdit={() => openEditor(selectedDoc)} onDelete={() => deleteDoc(selectedDoc.path)} onTagClick={tag => setTagFilter(tag)} kbRef={kbRef} setSelectedDoc={setSelectedDoc} />
          )}
          {view === 'editor' && (
            <EditorView
              path={editPath} title={editTitle} content={editContent} tags={editTags}
              saving={saving} tab={editorTab}
              onPath={setEditPath} onTitle={setEditTitle} onContent={setEditContent} onTags={setEditTags}
              onTab={setEditorTab} onSave={saveDoc}
              onCancel={() => setView(selectedDoc ? 'doc' : 'browse')}
              shared={shared ? info : null} reason={editReason} onReason={setEditReason} error={actionError}
            />
          )}
        </main>
      </div>
    </div>
  )
}

/* ── Team / org KB header ──────────────────────────────────────────────────── */
function ScopeHeader({ info, rightSlot }: { info: KBInfo | null; rightSlot?: React.ReactNode }) {
  return (
    <PageHeader
      crumb={<Link to="/admin/orgs" className="hover:text-ink">{info?.scope === 'org' ? 'Org knowledge base' : info?.scope === 'team' ? 'Team knowledge base' : 'Knowledge base'}</Link>}
      title={info?.name ?? 'Knowledge base'}
      actions={rightSlot}
    />
  )
}

/* ── Other knowledge bases around a project ───────────────────────────────── */
function OtherKBs({ projectId }: { projectId: number }) {
  const [sources, setSources] = useState<KBSource[]>([])
  useEffect(() => {
    api.kbSources(projectId).then(s => setSources(s.filter(x => x.scope !== 'project'))).catch(() => setSources([]))
  }, [projectId])
  if (sources.length === 0) return null
  return (
    <div className="flex-shrink-0 px-3 py-3" style={{ borderTop: '1px solid var(--line-sub)' }}>
      <div className="text-[10px] font-bold text-ink-3 uppercase tracking-widest mb-2">Shared knowledge</div>
      <div className="space-y-0.5">
        {sources.map(s => {
          const Icon = s.scope === 'org' ? Building2 : Users
          const label = (
            <>
              <Icon size={12} className="flex-shrink-0" />
              <span className="truncate flex-1">{s.name}</span>
              {s.scope === 'team' && s.team_role === 'owner' && <span className="text-[10px] text-ink-3">owner</span>}
              {s.can_read ? <span className="text-[10px] text-ink-3">{s.docs ?? 0}</span> : <Lock size={10} />}
            </>
          )
          return s.can_read ? (
            <Link key={`${s.scope}-${s.scope_id}`} to={s.scope === 'org' ? `/orgs/${s.scope_id}/kb` : `/teams/${s.scope_id}/kb`}
              className="flex items-center gap-2 px-2 py-1.5 rounded-lg text-xs text-ink-2 hover:text-ink hover:bg-surface-3 transition-colors">
              {label}
            </Link>
          ) : (
            <div key={`${s.scope}-${s.scope_id}`} title="Only members of this team can read its KB"
              className="flex items-center gap-2 px-2 py-1.5 text-xs text-ink-3">
              {label}
            </div>
          )
        })}
      </div>
    </div>
  )
}

/* ── File tree ────────────────────────────────────────────────────────────── */
interface TreeNodeData {
  name: string; fullPath: string; isDir: boolean
  children?: TreeNodeData[]; doc?: KBDocSummary
}

function buildTree(docs: KBDocSummary[]): TreeNodeData {
  const root: TreeNodeData = { name: '', fullPath: '', isDir: true, children: [] }
  for (const doc of docs) {
    const parts = doc.path.split('/')
    let node = root
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i], isLast = i === parts.length - 1
      const fullPath = parts.slice(0, i + 1).join('/')
      if (!node.children) node.children = []
      let child = node.children.find(c => c.name === part)
      if (!child) {
        child = { name: part, fullPath, isDir: !isLast, children: isLast ? undefined : [] }
        if (isLast) child.doc = doc
        node.children.push(child)
      }
      node = child
    }
  }
  return root
}

function TreeNode({ node, depth, expanded, onToggleDir, onSelectDoc, selectedPath }: {
  node: TreeNodeData; depth: number; expanded: Set<string>
  onToggleDir: (path: string) => void; onSelectDoc: (path: string) => void; selectedPath?: string
}) {
  const children = node.children ?? []
  const isExpanded = !node.fullPath || expanded.has(node.fullPath)

  if (node.isDir && node.fullPath) {
    return (
      <div>
        <button
          onClick={() => onToggleDir(node.fullPath)}
          className="flex items-center gap-1.5 w-full px-2 py-1.5 rounded-md text-xs transition-colors cursor-pointer"
          style={{ paddingLeft: `${8 + depth * 14}px`, color: 'var(--ink-2)' }}
          onMouseEnter={e => (e.currentTarget as HTMLElement).style.color = 'var(--ink)'}
          onMouseLeave={e => (e.currentTarget as HTMLElement).style.color = 'var(--ink-2)'}
        >
          {isExpanded
            ? <><FolderOpen size={12} className="text-warning-ink flex-shrink-0" /><ChevronDown size={10} className="flex-shrink-0" /></>
            : <><Folder size={12} className="text-warning-ink flex-shrink-0" /><ChevronRight size={10} className="flex-shrink-0" /></>
          }
          <span className="font-semibold truncate">{node.name}</span>
          <span className="ml-auto text-ink-3 tabular-nums">
            {children.length}
          </span>
        </button>
        {isExpanded && children.map(child => (
          <TreeNode key={child.fullPath} node={child} depth={depth + 1}
            expanded={expanded} onToggleDir={onToggleDir}
            onSelectDoc={onSelectDoc} selectedPath={selectedPath} />
        ))}
      </div>
    )
  }

  if (!node.isDir) {
    const isSelected = selectedPath === node.fullPath
    return (
      <button
        onClick={() => onSelectDoc(node.fullPath)}
        className="flex items-center gap-1.5 w-full px-2 py-1.5 rounded-md text-xs transition-all cursor-pointer"
        style={{
          paddingLeft: `${8 + depth * 14}px`,
          background: isSelected ? 'color-mix(in srgb, var(--link) 12%, transparent)' : 'transparent',
          color: isSelected ? 'var(--link)' : 'var(--ink-2)',
          border: isSelected ? '1px solid color-mix(in srgb, var(--link) 20%, transparent)' : '1px solid transparent',
        }}
        onMouseEnter={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.color = 'var(--ink)' }}
        onMouseLeave={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.color = 'var(--ink-2)' }}
      >
        <FileText size={11} className="flex-shrink-0" />
        <span className="truncate flex-1 text-left">{node.name.replace(/\.md$/, '')}</span>
      </button>
    )
  }

  return <>{children.map(child => (
    <TreeNode key={child.fullPath} node={child} depth={depth}
      expanded={expanded} onToggleDir={onToggleDir}
      onSelectDoc={onSelectDoc} selectedPath={selectedPath} />
  ))}</>
}

/* ── Search results (in sidebar) ─────────────────────────────────────────── */
function SearchResultsList({ query, results, searching, onSelect, selectedPath }: {
  query: string; results: KBSearchResult[]; searching: boolean
  onSelect: (path: string) => void; selectedPath?: string
}) {
  if (searching) return (
    <div className="flex justify-center py-6">
      <Loader2 size={14} className="animate-spin text-primary" />
    </div>
  )
  if (query && results.length === 0) return (
    <div className="text-center py-6">
      <p className="text-xs text-ink-3">No results for "{query}"</p>
    </div>
  )
  return (
    <div className="space-y-1">
      {results.map((r, i) => {
        const isSelected = selectedPath === r.path
        return (
          <button key={i} onClick={() => onSelect(r.path)}
            className="w-full text-left px-2 py-2 rounded-lg transition-all cursor-pointer"
            style={{
              background: isSelected ? 'color-mix(in srgb, var(--link) 12%, transparent)' : 'transparent',
              border: isSelected ? '1px solid color-mix(in srgb, var(--link) 20%, transparent)' : '1px solid transparent',
            }}
            onMouseEnter={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.background = 'color-mix(in srgb, var(--ink) 3%, transparent)' }}
            onMouseLeave={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.background = 'transparent' }}
          >
            <div className="flex items-center gap-1.5 mb-0.5">
              <FileText size={10} className="text-primary flex-shrink-0" />
              <span className="text-xs font-semibold text-ink truncate">{r.title}</span>
            </div>
            {r.excerpt && (
              <p className="text-xs text-ink-3 leading-relaxed line-clamp-2 pl-4">{r.excerpt}</p>
            )}
            {r.tags.length > 0 && (
              <div className="flex gap-1 mt-1 pl-4 flex-wrap">
                {r.tags.slice(0, 3).map(t => {
                  const tc = tagColor()
                  return <span key={t} className="text-[10px] px-1.5 py-0.5 rounded" style={{ background: tc.bg, color: tc.text }}>{t}</span>
                })}
              </div>
            )}
            {r.annotation_count !== undefined && r.annotation_count > 0 && (
              <div className="flex items-center gap-1 mt-1 pl-4">
                <MessageSquare size={10} className="text-warning-ink" />
                <span className="text-[10px] text-ink-3">
                  {r.annotation_count} annotation{r.annotation_count !== 1 ? 's' : ''}
                  {r.latest_annotation && <> — {r.latest_annotation}</>}
                </span>
              </div>
            )}
          </button>
        )
      })}
    </div>
  )
}

/* ── Browse view (default landing) ───────────────────────────────────────── */
function BrowseView({ docs, loading, tagFilter, onOpen, onNew }: {
  docs: KBDocSummary[]; loading: boolean; tagFilter: string | null
  onOpen: (path: string) => void; onNew?: () => void
}) {
  if (loading) return (
    <div className="flex justify-center py-20">
      <Loader2 size={20} className="text-primary animate-spin" />
    </div>
  )

  if (docs.length === 0) return (
    <div className="flex flex-col items-center justify-center h-full py-24 gap-5">
      <div className="w-16 h-16 rounded-2xl flex items-center justify-center"
        style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}>
        <BookOpen size={26} className="text-line-str" />
      </div>
      <div className="text-center">
        <p className="text-base font-semibold text-ink">No documents yet</p>
        <p className="text-sm text-ink-2 mt-2 max-w-xs leading-relaxed">
          {onNew ? 'Agents write docs here, or you can create one manually.' : "There's nothing here yet."}
        </p>
        {onNew && <button onClick={onNew}
          className="amp-btn mt-4">
          <Plus size={14} />
          Create first document
        </button>}
      </div>
    </div>
  )

  return (
    <div className="p-6 max-w-4xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h2 className="text-lg font-bold text-ink">
            {tagFilter ? `Tagged "${tagFilter}"` : 'All documents'}
          </h2>
          <p className="text-sm text-ink-3 mt-0.5">{docs.length} doc{docs.length !== 1 ? 's' : ''}</p>
        </div>
      </div>
      <div className="grid gap-2.5">
        {docs.map(doc => (
          <DocListItem key={doc.id} doc={doc} onOpen={() => onOpen(doc.path)} />
        ))}
      </div>
    </div>
  )
}

function DocListItem({ doc, onOpen }: { doc: KBDocSummary; onOpen: () => void }) {
  // Extract a rough word count from path guess — we don't have content here
  return (
    <div
      onClick={onOpen}
      className="flex items-start gap-4 p-4 rounded-xl cursor-pointer transition-all duration-150 group"
      style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}
      onMouseEnter={e => {
        (e.currentTarget as HTMLElement).style.borderColor = 'color-mix(in srgb, var(--link) 30%, transparent)'
        ;(e.currentTarget as HTMLElement).style.transform = 'translateY(-1px)'
        ;(e.currentTarget as HTMLElement).style.boxShadow = '0 4px 16px color-mix(in srgb, var(--ink) 15%, transparent)'
      }}
      onMouseLeave={e => {
        (e.currentTarget as HTMLElement).style.borderColor = 'var(--line)'
        ;(e.currentTarget as HTMLElement).style.transform = ''
        ;(e.currentTarget as HTMLElement).style.boxShadow = ''
      }}
    >
      <div className="w-9 h-9 flex-shrink-0 flex items-center justify-center mt-0.5 border border-line bg-surface-2">
        <FileText size={15} className="text-ink-2" />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-start justify-between gap-3">
          <h3 className="text-sm font-semibold text-ink group-hover:text-white transition-colors leading-snug">
            {doc.title}
          </h3>
          <span className="text-[10px] text-ink-3 font-mono flex-shrink-0 mt-0.5">{doc.author}</span>
        </div>
        <p className="text-xs text-ink-3 font-mono mt-0.5 truncate">{doc.path}</p>
        {doc.tags.length > 0 && (
          <div className="flex gap-1 mt-2 flex-wrap">
            {doc.tags.map(t => {
              const tc = tagColor()
              return (
                <span key={t} className="text-xs px-2 py-0.5 rounded-md font-medium"
                  style={{ background: tc.bg, color: tc.text, border: `1px solid ${tc.ring}` }}>
                  {t}
                </span>
              )
            })}
          </div>
        )}
      </div>
    </div>
  )
}

/* ── Doc viewer ───────────────────────────────────────────────────────────── */
function DocView({ doc, canWrite, onEdit, onDelete, onTagClick, kbRef, setSelectedDoc }: {
  doc: KBDoc; canWrite: boolean; onEdit: () => void; onDelete: () => void; onTagClick: (tag: string) => void
  kbRef: KBRef; setSelectedDoc: (doc: KBDoc | null) => void
}) {
  const pathParts = doc.path.split('/')
  const rt = readingTime(doc.content)
  const date = doc.updated_at > 0 ? new Date(doc.updated_at * 1000).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' }) : null
  const [annExpanded, setAnnExpanded] = useState(true)
  const [annText, setAnnText] = useState('')
  const [annSaving, setAnnSaving] = useState(false)
  const [annError, setAnnError] = useState<string | null>(null)

  const handleAddAnnotation = async () => {
    if (!annText.trim()) return
    setAnnSaving(true)
    setAnnError(null)
    try {
      await api.kbAnnotate(kbRef, doc.path, annText)
      const updatedDoc = await api.kbGet(kbRef, doc.path)
      setSelectedDoc(updatedDoc)
      setAnnText('')
    } catch (e) {
      setAnnError(e instanceof Error ? e.message : 'Annotation failed')
    } finally {
      setAnnSaving(false)
    }
  }

  const [resolving, setResolving] = useState<number | null>(null)
  const setResolved = async (index: number, resolved: boolean) => {
    setResolving(index)
    setAnnError(null)
    try {
      setSelectedDoc(await api.kbResolveAnnotation(kbRef, doc.path, index, resolved))
    } catch (e) {
      setAnnError(e instanceof Error ? e.message : 'Update failed')
    } finally {
      setResolving(null)
    }
  }

  const formatDate = (timestamp: number) => {
    const date = new Date(timestamp * 1000)
    const now = new Date()
    const diffMs = now.getTime() - date.getTime()
    const diffMins = Math.floor(diffMs / 60000)
    const diffHours = Math.floor(diffMs / 3600000)
    const diffDays = Math.floor(diffMs / 86400000)
    
    if (diffMins < 1) return 'just now'
    if (diffMins < 60) return `${diffMins}m ago`
    if (diffHours < 24) return `${diffHours}h ago`
    if (diffDays < 7) return `${diffDays}d ago`
    return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' })
  }

  return (
    <div className="p-8 max-w-3xl mx-auto">
      {/* Breadcrumb */}
      <div className="flex items-center gap-1.5 text-xs text-ink-3 mb-5 flex-wrap">
        {pathParts.map((part, i) => (
          <span key={i} className="flex items-center gap-1.5">
            {i > 0 && <ChevronRight size={10} />}
            <span className={i === pathParts.length - 1 ? 'text-ink-2' : ''}>{part}</span>
          </span>
        ))}
      </div>

      {/* Title + actions */}
      <div className="flex items-start justify-between gap-4 mb-4">
        <div className="flex-1 min-w-0">
          <h1 className="text-2xl font-bold text-ink leading-tight mb-3">{doc.title}</h1>
          <div className="flex items-center gap-3 flex-wrap">
            <span className="text-xs text-ink-3 flex items-center gap-1">
              <Clock size={10} />
              {rt}
            </span>
            {date && <span className="text-xs text-ink-3">{date}</span>}
            {doc.author && <span className="text-xs text-ink-3">by {doc.author}</span>}
          </div>
        </div>
        {canWrite && <div className="flex items-center gap-2 flex-shrink-0">
          <button onClick={onEdit} className="amp-btn amp-btn-sm">
            <Edit3 size={12} />Edit
          </button>
          <button onClick={onDelete} className="amp-btn amp-btn-sm amp-btn-danger" aria-label="Delete document" title="Delete document">
            <Trash2 size={12} />
          </button>
        </div>}
      </div>

      {/* Why a team or org doc lives here */}
      {doc.reason && (
        <div className="flex items-start gap-2.5 rounded-xl px-4 py-3 mb-5"
          style={{ background: 'var(--surface)', border: '1px solid var(--line)', boxShadow: 'inset 3px 0 0 var(--ink)' }}>
          <Info size={13} className="text-ink-2 mt-0.5 flex-shrink-0" />
          <div className="text-xs leading-relaxed">
            <span className="font-semibold text-ink">Why it's in the {doc.scope === 'org' ? 'org' : 'team'} KB: </span>
            <span className="text-ink-2">{doc.reason}</span>
          </div>
        </div>
      )}

      {/* Tags */}
      {doc.tags.length > 0 && (
        <div className="flex flex-wrap gap-1.5 mb-6">
          {doc.tags.map(tag => {
            const tc = tagColor()
            return (
              <button key={tag} onClick={() => onTagClick(tag)}
                className="flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-medium transition-all cursor-pointer"
                style={{ background: tc.bg, color: tc.text, border: `1px solid ${tc.ring}` }}>
                <Tag size={9} />{tag}
              </button>
            )
          })}
        </div>
      )}

      {/* Divider */}
      <div className="border-t border-line-sub mb-6" />

      {/* Markdown content */}
      <div className="prose-amp">
        <ReactMarkdown
          components={{
            h1: ({children}) => <h1 className="amp-heading text-[22px] text-ink mt-8 mb-3 pb-2 border-b border-line first:mt-0">{children}</h1>,
            h2: ({children}) => <h2 className="text-[17px] font-semibold text-ink mt-7 mb-2">{children}</h2>,
            h3: ({children}) => <h3 className="font-label text-[13px] font-semibold uppercase tracking-[0.12em] text-ink mt-5 mb-1.5">{children}</h3>,
            p: ({children}) => <p className="text-sm text-ink-2 leading-7 mb-4">{children}</p>,
            code: ({children, className}) => {
              const isBlock = className?.includes('language-')
              if (isBlock) return (
                <pre className="rounded-xl p-4 text-xs leading-relaxed overflow-x-auto mb-4 font-mono"
                  style={{ background: 'var(--bg)', border: '1px solid var(--line-sub)' }}>
                  <code style={{ color: 'var(--ink)' }}>{children}</code>
                </pre>
              )
              return <code className="rounded-md px-1.5 py-0.5 text-xs font-mono" style={{ background: 'var(--surface-2)', color: 'var(--ink)', border: '1px solid var(--line-sub)' }}>{children}</code>
            },
            ul: ({children}) => <ul className="text-sm text-ink-2 list-disc list-outside mb-4 space-y-1.5 pl-5">{children}</ul>,
            ol: ({children}) => <ol className="text-sm text-ink-2 list-decimal list-outside mb-4 space-y-1.5 pl-5">{children}</ol>,
            li: ({children}) => <li className="leading-7">{children}</li>,
            blockquote: ({children}) => (
              <blockquote className="pl-4 my-4 text-sm italic text-ink-2"
                style={{ borderLeft: '3px solid var(--primary)' }}>{children}</blockquote>
            ),
            strong: ({children}) => <strong className="text-ink font-semibold">{children}</strong>,
            a: ({children, href}) => <a href={href} className="text-link hover:text-primary underline underline-offset-2 transition-colors">{children}</a>,
            hr: () => <hr className="border-line-sub my-6" />,
            table: ({children}) => (
              <div className="overflow-x-auto mb-4">
                <table className="w-full text-sm border-collapse">{children}</table>
              </div>
            ),
            th: ({children}) => <th className="text-left px-4 py-2.5 text-xs font-semibold text-ink-2 uppercase tracking-wide" style={{ background: 'var(--surface)', borderBottom: '1px solid var(--line)' }}>{children}</th>,
            td: ({children}) => <td className="px-4 py-2.5 text-ink-2 text-sm" style={{ borderBottom: '1px solid var(--line-sub)' }}>{children}</td>,
          }}
        >
          {doc.content}
        </ReactMarkdown>
      </div>

      {/* Annotations Panel */}
      <div className="mt-8 pt-6 border-t border-line-sub">
        <button
          onClick={() => setAnnExpanded(!annExpanded)}
          className="flex items-center gap-2 text-xs font-semibold text-ink-2 hover:text-ink transition-colors mb-4"
        >
          {annExpanded ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
          <MessageSquare size={12} />
          Annotations
          <span className="ml-1.5 px-1.5 py-0.5 rounded text-[10px] font-bold" style={{ background: 'color-mix(in srgb, var(--link) 12%, transparent)', color: 'var(--link)' }}>
            {doc.annotations?.length ?? 0}
          </span>
        </button>
        
        {annExpanded && (
          <div className="space-y-4">
            {/* Annotation List */}
            {doc.annotations && doc.annotations.length > 0 ? (
              <div className="space-y-3">
                {doc.annotations.map((ann, idx) => (
                  <div key={idx} className="p-3 rounded-lg text-xs" style={{ background: 'color-mix(in srgb, var(--ink) 3%, transparent)', border: '1px solid var(--line)' }}>
                    <div className="flex items-center justify-between mb-2">
                      <div className="flex items-center gap-2">
                        <span className="font-semibold text-ink">{ann.author}</span>
                        <span className="text-ink-3">{formatDate(ann.created_at)}</span>
                      </div>
                      <div className="flex items-center gap-2">
                        <span className={`px-1.5 py-0.5 rounded text-[10px] font-medium ${
                          ann.is_resolved
                            ? 'text-success'
                            : 'text-warning'
                        }`} style={{ background: ann.is_resolved ? 'color-mix(in srgb, var(--success) 12%, transparent)' : 'color-mix(in srgb, var(--warning) 12%, transparent)' }}>
                          {ann.is_resolved ? 'Resolved' : 'Unresolved'}
                        </span>
                        {canWrite && (
                          <button
                            onClick={() => setResolved(idx, !ann.is_resolved)}
                            disabled={resolving !== null}
                            title={ann.is_resolved ? 'Reopen this note' : 'Mark resolved once the doc reflects this note'}
                            className="flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] font-semibold text-ink-2 hover:text-ink hover:bg-surface-3 transition-colors cursor-pointer disabled:opacity-50"
                          >
                            {resolving === idx ? <Loader2 size={10} className="animate-spin" /> : ann.is_resolved ? <RotateCcw size={10} /> : <CheckCircle2 size={10} />}
                            {ann.is_resolved ? 'Reopen' : 'Resolve'}
                          </button>
                        )}
                      </div>
                    </div>
                    <p className={`leading-relaxed whitespace-pre-wrap ${ann.is_resolved ? 'text-ink-3' : 'text-ink-2'}`}>{ann.text}</p>
                    {ann.is_resolved && ann.resolved_by && (
                      <p className="text-[10px] text-ink-3 mt-1.5">
                        Resolved by {ann.resolved_by}{ann.resolved_at ? ` · ${formatDate(ann.resolved_at)}` : ''}
                      </p>
                    )}
                  </div>
                ))}
              </div>
            ) : (
              <div className="text-center py-6">
                <p className="text-xs text-ink-3">No annotations yet</p>
              </div>
            )}

            {/* Add Annotation Form */}
            <div className="pt-4 border-t border-line-sub">
              <div className="flex items-center gap-2 mb-2">
                <MessageSquare size={12} className="text-primary" />
                <span className="text-xs font-semibold text-ink">Add Annotation</span>
              </div>
              <div className="space-y-2">
                <textarea
                  value={annText}
                  onChange={(e) => setAnnText(e.target.value)}
                  placeholder="Add your annotation here..."
                  className="w-full bg-bg border border-line rounded-lg px-3 py-2 text-xs text-ink placeholder-ink-3 focus:outline-none focus:border-primary focus:ring-2 focus:ring-primary/20 transition-all font-[inherit] resize-none"
                  rows={3}
                />
                <div className="flex justify-end items-center gap-3">
                  {annError && <span className="text-xs text-danger-ink">{annError}</span>}
                  <button
                    onClick={handleAddAnnotation}
                    disabled={!annText.trim() || annSaving}
                    className="amp-btn"
                    onMouseEnter={(e) => {
                      if (!annSaving && annText.trim()) {
                        (e.currentTarget as HTMLElement).style.background = 'color-mix(in srgb, var(--link) 20%, transparent)';
                        (e.currentTarget as HTMLElement).style.borderColor = 'color-mix(in srgb, var(--link) 40%, transparent)';
                      }
                    }}
                    onMouseLeave={(e) => {
                      if (!annSaving && annText.trim()) {
                        (e.currentTarget as HTMLElement).style.background = 'color-mix(in srgb, var(--link) 12%, transparent)';
                        (e.currentTarget as HTMLElement).style.borderColor = 'color-mix(in srgb, var(--link) 25%, transparent)';
                      }
                    }}
                  >
                    {annSaving ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}
                    {annSaving ? 'Adding...' : 'Add Annotation'}
                  </button>
                </div>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

/* ── Editor ───────────────────────────────────────────────────────────────── */
function EditorView({ path, title, content, tags, saving, tab, onPath, onTitle, onContent, onTags, onTab, onSave, onCancel, shared, reason, onReason, error }: {
  path: string; title: string; content: string; tags: string; saving: boolean
  tab: 'write' | 'preview'
  onPath: (v: string) => void; onTitle: (v: string) => void; onContent: (v: string) => void; onTags: (v: string) => void
  onTab: (t: 'write' | 'preview') => void
  onSave: () => void; onCancel: () => void
  /** the team or org KB being written to; null for a project KB */
  shared: KBInfo | null
  reason: string; onReason: (v: string) => void
  error: string | null
}) {
  const canSave = !!path && !!title && !!content && !saving
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  useEffect(() => {
    if (tab === 'write') textareaRef.current?.focus()
  }, [tab])

  return (
    <div className="flex flex-col h-full">
      {/* Editor header */}
      <div className="flex items-center justify-between px-6 py-3 flex-shrink-0"
        style={{ borderBottom: '1px solid var(--line)', background: 'var(--surface)' }}>
        <div className="flex items-center gap-1">
          {(['write', 'preview'] as const).map(t => (
            <button key={t} onClick={() => onTab(t)}
              className={`flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold transition-all cursor-pointer ${
                tab === t ? 'text-ink' : 'text-ink-3 hover:text-ink-2'
              }`}
              style={tab === t ? { background: 'var(--surface-3)', border: '1px solid var(--line-str)' } : {}}>
              {t === 'write' ? <Code2 size={12} /> : <Eye size={12} />}
              {t === 'write' ? 'Write' : 'Preview'}
            </button>
          ))}
        </div>
        <div className="flex items-center gap-2">
          {error && <span className="text-xs text-danger-ink max-w-md truncate" title={error}>{error}</span>}
          <button onClick={onCancel}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium transition-colors cursor-pointer"
            style={{ border: '1px solid var(--line)', color: 'var(--ink-2)', background: 'transparent' }}
            onMouseEnter={e => (e.currentTarget as HTMLElement).style.color = 'var(--ink)'}
            onMouseLeave={e => (e.currentTarget as HTMLElement).style.color = 'var(--ink-2)'}>
            <X size={12} />Cancel
          </button>
          <button onClick={onSave} disabled={!canSave}
            className="flex items-center gap-1.5 px-4 py-1.5 rounded-lg text-xs font-semibold transition-all active:scale-[0.97] cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed"

            onMouseEnter={e => { if (canSave) (e.currentTarget as HTMLElement).style.background = 'var(--primary)' }}
            onMouseLeave={e => { if (canSave) (e.currentTarget as HTMLElement).style.background = 'var(--primary)' }}>
            {saving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
            {saving ? 'Saving…' : 'Save doc'}
          </button>
        </div>
      </div>

      {/* Meta fields */}
      <div className="px-6 py-4 flex-shrink-0 grid grid-cols-3 gap-3" style={{ borderBottom: '1px solid var(--line-sub)' }}>
        <div className="col-span-1">
          <label className="amp-label block mb-2">Path</label>
          <input value={path} onChange={e => onPath(e.target.value)} placeholder="folder/doc.md"
            className={`${inputCls} font-mono text-xs`} />
        </div>
        <div className="col-span-1">
          <label className="amp-label block mb-2">Title</label>
          <input value={title} onChange={e => onTitle(e.target.value)} placeholder="Document title"
            className={inputCls} />
        </div>
        <div className="col-span-1">
          <label className="amp-label block mb-2">Tags</label>
          <input value={tags} onChange={e => onTags(e.target.value)} placeholder="auth, api, guide"
            className={inputCls} />
        </div>
        {shared && (
          <div className="col-span-3">
            <label className="amp-label block mb-2">
              Why does this belong in the {shared.scope} KB?
            </label>
            <input value={reason} onChange={e => onReason(e.target.value)}
              placeholder={shared.scope === 'org'
                ? 'e.g. Brand colours apply to every product in the org'
                : 'e.g. Every project this team runs deploys this way'}
              className={inputCls} />
            <p className="text-[11px] text-ink-3 mt-1">{SCOPE_PURPOSE[shared.scope]} Shown with the doc so others know why it's shared.</p>
          </div>
        )}
      </div>

      {/* Write / Preview pane */}
      <div className="flex-1 overflow-hidden">
        {tab === 'write' ? (
          <textarea
            ref={textareaRef}
            value={content}
            onChange={e => onContent(e.target.value)}
            placeholder={"# Title\n\nWrite your document in Markdown…"}
            className="w-full h-full p-6 text-sm text-ink font-mono resize-none focus:outline-none"
            style={{ background: 'var(--bg)', lineHeight: '1.7', caretColor: 'var(--primary)' }}
          />
        ) : (
          <div className="p-8 overflow-y-auto h-full max-w-3xl">
            {content ? (
              <ReactMarkdown
                components={{
                  h1: ({children}) => <h1 className="amp-heading text-[22px] text-ink mt-6 mb-3 pb-2 border-b border-line first:mt-0">{children}</h1>,
                  h2: ({children}) => <h2 className="text-[17px] font-semibold text-ink mt-6 mb-2">{children}</h2>,
                  h3: ({children}) => <h3 className="font-label text-[13px] font-semibold uppercase tracking-[0.12em] text-ink mt-5 mb-1.5">{children}</h3>,
                  p: ({children}) => <p className="text-sm text-ink-2 leading-7 mb-4">{children}</p>,
                  code: ({children, className}) => {
                    const isBlock = className?.includes('language-')
                    if (isBlock) return <pre className="rounded-xl p-4 text-xs overflow-x-auto mb-4 font-mono" style={{ background: 'var(--bg)', border: '1px solid var(--line-sub)' }}><code style={{ color: 'var(--ink)' }}>{children}</code></pre>
                    return <code className="rounded px-1.5 py-0.5 text-xs font-mono" style={{ background: 'var(--surface-2)', color: 'var(--ink)' }}>{children}</code>
                  },
                  ul: ({children}) => <ul className="text-sm text-ink-2 list-disc list-outside mb-4 space-y-1 pl-5">{children}</ul>,
                  li: ({children}) => <li className="leading-7">{children}</li>,
                  strong: ({children}) => <strong className="text-ink font-semibold">{children}</strong>,
                  blockquote: ({children}) => <blockquote className="pl-4 my-4 italic text-ink-2" style={{ borderLeft: '3px solid var(--primary)' }}>{children}</blockquote>,
                }}>
                {content}
              </ReactMarkdown>
            ) : (
              <p className="text-sm text-ink-3 italic">Nothing to preview yet.</p>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
