import { useState, useEffect, useCallback, useRef } from 'react'
import { useParams } from 'react-router-dom'
import {
  Plus, Search, FileText, Tag, Trash2, Edit3, Save, X,
  Loader2, ChevronRight, ChevronDown, BookOpen, Eye, Code2, FolderOpen, Folder, Clock, MessageSquare,
} from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import { api } from '../api/client'
import { ProjectNav } from '../components/ProjectNav'
import type { KBDoc, KBDocSummary, KBSearchResult, KBTagCount } from '../types'

type View = 'browse' | 'doc' | 'editor'

// Tag color palette — cycles through a small set of indigo-adjacent hues
const TAG_COLORS = [
  { bg: 'rgba(99,102,241,0.12)',  text: '#818CF8', ring: 'rgba(99,102,241,0.25)' },
  { bg: 'rgba(16,185,129,0.12)', text: '#34D399', ring: 'rgba(16,185,129,0.25)' },
  { bg: 'rgba(245,158,11,0.12)', text: '#FBBF24', ring: 'rgba(245,158,11,0.25)' },
  { bg: 'rgba(239,68,68,0.12)',  text: '#F87171', ring: 'rgba(239,68,68,0.25)' },
  { bg: 'rgba(139,92,246,0.12)', text: '#A78BFA', ring: 'rgba(139,92,246,0.25)' },
  { bg: 'rgba(20,184,166,0.12)', text: '#2DD4BF', ring: 'rgba(20,184,166,0.25)' },
]
const tagColor = (tag: string) => TAG_COLORS[Math.abs(hashStr(tag)) % TAG_COLORS.length]
function hashStr(s: string) {
  let h = 0; for (const c of s) h = (Math.imul(31, h) + c.charCodeAt(0)) | 0; return h
}

function readingTime(content: string) {
  const words = content.trim().split(/\s+/).length
  const mins = Math.max(1, Math.round(words / 200))
  return `${mins} min read`
}

const inputCls =
  'w-full bg-[#08101F] border border-[#1E2C45] rounded-lg px-3 py-2 text-sm text-[#DDE6F0] ' +
  'placeholder-[#3D5068] focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all font-[inherit]'

export function KB() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)

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
  const [saving, setSaving] = useState(false)
  const [editorTab, setEditorTab] = useState<'write' | 'preview'>('write')

  const loadDocs = useCallback(async () => {
    setLoading(true)
    try {
      const [docList, tagList] = await Promise.all([
        api.kbList(pid, tagFilter ?? undefined),
        api.kbTags(pid),
      ])
      setDocs(docList)
      setTags(tagList)
    } finally {
      setLoading(false)
    }
  }, [pid, tagFilter])

  useEffect(() => { loadDocs() }, [loadDocs])

  // Debounced search
  useEffect(() => {
    if (!searchQuery.trim()) { setSearchResults([]); return }
    const t = setTimeout(async () => {
      setSearching(true)
      try { setSearchResults(await api.kbSearch(pid, searchQuery)) }
      finally { setSearching(false) }
    }, 300)
    return () => clearTimeout(t)
  }, [searchQuery, pid])

  const openDoc = async (path: string) => {
    const doc = await api.kbGet(pid, path)
    setSelectedDoc(doc)
    setView('doc')
    setSearchQuery('') // close search results
  }

  const openEditor = (doc?: KBDoc | null) => {
    if (doc) {
      setEditPath(doc.path); setEditTitle(doc.title)
      setEditContent(doc.content); setEditTags(doc.tags.join(', '))
    } else {
      setEditPath(''); setEditTitle(''); setEditContent(''); setEditTags('')
    }
    setEditorTab('write')
    setView('editor')
  }

  const saveDoc = async () => {
    if (!editPath || !editTitle || !editContent) return
    setSaving(true)
    try {
      const parsedTags = editTags.split(',').map(t => t.trim()).filter(Boolean)
      await api.kbWrite(pid, editPath, editTitle, editContent, parsedTags)
      await loadDocs()
      const doc = await api.kbGet(pid, editPath)
      setSelectedDoc(doc); setView('doc')
    } finally { setSaving(false) }
  }

  const deleteDoc = async (path: string) => {
    if (!confirm(`Delete "${path}"?`)) return
    await api.kbDelete(pid, path)
    setSelectedDoc(null); setView('browse'); loadDocs()
  }

  const tree = buildTree(docs)
  const isSearching = searchQuery.trim().length > 0

  return (
    <div className="flex flex-col h-screen" style={{ background: '#08101F' }}>
      {/* Header */}
      <ProjectNav
        projectId={pid}
        currentView="kb"
        rightSlot={
          <button
            onClick={() => openEditor(null)}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold transition-all active:scale-[0.97] cursor-pointer"
            style={{ background: 'rgba(99,102,241,0.12)', border: '1px solid rgba(99,102,241,0.25)', color: '#818CF8' }}
          >
            <Plus size={12} />
            New Doc
          </button>
        }
      />

      <div className="flex flex-1 overflow-hidden">
        {/* ── Sidebar ────────────────────────────────────────────────── */}
        <aside className="flex flex-col flex-shrink-0 overflow-hidden"
          style={{ width: 248, borderRight: '1px solid #1E2C45', background: '#0D1726' }}>

          {/* Search */}
          <div className="px-3 pt-3 pb-2 flex-shrink-0">
            <div className="relative">
              <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-[#3D5068] pointer-events-none" />
              <input
                type="text"
                placeholder="Search docs…"
                value={searchQuery}
                onChange={e => setSearchQuery(e.target.value)}
                className="w-full text-xs rounded-lg pl-8 pr-8 py-2 transition-all focus:outline-none"
                style={{
                  background: '#08101F',
                  border: '1px solid #1E2C45',
                  color: '#DDE6F0',
                  fontFamily: 'inherit',
                }}
                onFocus={e => (e.target.style.borderColor = '#6366F1')}
                onBlur={e => (e.target.style.borderColor = '#1E2C45')}
              />
              {searching && <Loader2 size={12} className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[#6366F1] animate-spin" />}
              {searchQuery && !searching && (
                <button onClick={() => setSearchQuery('')}
                  className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[#3D5068] hover:text-[#DDE6F0] cursor-pointer">
                  <X size={11} />
                </button>
              )}
            </div>
          </div>

          {/* Tags */}
          {tags.length > 0 && !isSearching && (
            <div className="px-3 pb-2 flex-shrink-0">
              <div className="text-[10px] font-bold text-[#3D5068] uppercase tracking-widest mb-2 flex items-center gap-1.5">
                <Tag size={9} />
                Tags
              </div>
              <div className="flex flex-wrap gap-1">
                <button
                  onClick={() => setTagFilter(null)}
                  className={`text-xs px-2 py-0.5 rounded-md transition-all cursor-pointer font-medium ${
                    !tagFilter
                      ? 'bg-[#172540] text-[#DDE6F0]'
                      : 'text-[#3D5068] hover:text-[#7E91A8]'
                  }`}
                >all</button>
                {tags.slice(0, 12).map(t => {
                  const tc = tagColor(t.tag)
                  const isActive = tagFilter === t.tag
                  return (
                    <button
                      key={t.tag}
                      onClick={() => setTagFilter(tagFilter === t.tag ? null : t.tag)}
                      className="text-xs px-2 py-0.5 rounded-md transition-all cursor-pointer font-medium flex items-center gap-1"
                      style={{
                        background: isActive ? tc.bg : 'transparent',
                        color: isActive ? tc.text : '#3D5068',
                        border: isActive ? `1px solid ${tc.ring}` : '1px solid transparent',
                      }}
                      onMouseEnter={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = tc.text }}
                      onMouseLeave={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = '#3D5068' }}
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
          {!isSearching && <div className="mx-3 border-t border-[#192238] mb-1 flex-shrink-0" />}

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
                <Loader2 size={15} className="animate-spin text-[#7E91A8]" />
              </div>
            ) : docs.length === 0 ? (
              <div className="text-center py-8 px-3">
                <BookOpen size={20} className="text-[#283A57] mx-auto mb-2" />
                <p className="text-xs text-[#3D5068]">No docs yet</p>
                <p className="text-xs text-[#3D5068] mt-0.5">Click "New Doc" to start.</p>
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
        </aside>

        {/* ── Main content ───────────────────────────────────────────── */}
        <main className="flex-1 overflow-y-auto">
          {view === 'browse' && <BrowseView docs={docs} loading={loading} tagFilter={tagFilter} onOpen={openDoc} onNew={() => openEditor(null)} />}
          {view === 'doc' && selectedDoc && (
            <DocView doc={selectedDoc} onEdit={() => openEditor(selectedDoc)} onDelete={() => deleteDoc(selectedDoc.path)} onTagClick={tag => setTagFilter(tag)} pid={pid} setSelectedDoc={setSelectedDoc} />
          )}
          {view === 'editor' && (
            <EditorView
              path={editPath} title={editTitle} content={editContent} tags={editTags}
              saving={saving} tab={editorTab}
              onPath={setEditPath} onTitle={setEditTitle} onContent={setEditContent} onTags={setEditTags}
              onTab={setEditorTab} onSave={saveDoc}
              onCancel={() => setView(selectedDoc ? 'doc' : 'browse')}
            />
          )}
        </main>
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
          style={{ paddingLeft: `${8 + depth * 14}px`, color: '#7E91A8' }}
          onMouseEnter={e => (e.currentTarget as HTMLElement).style.color = '#DDE6F0'}
          onMouseLeave={e => (e.currentTarget as HTMLElement).style.color = '#7E91A8'}
        >
          {isExpanded
            ? <><FolderOpen size={12} className="text-[#FBBF24] flex-shrink-0" /><ChevronDown size={10} className="flex-shrink-0" /></>
            : <><Folder size={12} className="text-[#FBBF24] flex-shrink-0" /><ChevronRight size={10} className="flex-shrink-0" /></>
          }
          <span className="font-semibold truncate">{node.name}</span>
          <span className="ml-auto text-[#3D5068] tabular-nums">
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
          background: isSelected ? 'rgba(99,102,241,0.12)' : 'transparent',
          color: isSelected ? '#818CF8' : '#7E91A8',
          border: isSelected ? '1px solid rgba(99,102,241,0.2)' : '1px solid transparent',
        }}
        onMouseEnter={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.color = '#DDE6F0' }}
        onMouseLeave={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.color = '#7E91A8' }}
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
      <Loader2 size={14} className="animate-spin text-[#6366F1]" />
    </div>
  )
  if (query && results.length === 0) return (
    <div className="text-center py-6">
      <p className="text-xs text-[#3D5068]">No results for "{query}"</p>
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
              background: isSelected ? 'rgba(99,102,241,0.12)' : 'transparent',
              border: isSelected ? '1px solid rgba(99,102,241,0.2)' : '1px solid transparent',
            }}
            onMouseEnter={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.background = 'rgba(255,255,255,0.03)' }}
            onMouseLeave={e => { if (!isSelected) (e.currentTarget as HTMLElement).style.background = 'transparent' }}
          >
            <div className="flex items-center gap-1.5 mb-0.5">
              <FileText size={10} className="text-[#6366F1] flex-shrink-0" />
              <span className="text-xs font-semibold text-[#DDE6F0] truncate">{r.title}</span>
            </div>
            {r.excerpt && (
              <p className="text-xs text-[#3D5068] leading-relaxed line-clamp-2 pl-4">{r.excerpt}</p>
            )}
            {r.tags.length > 0 && (
              <div className="flex gap-1 mt-1 pl-4 flex-wrap">
                {r.tags.slice(0, 3).map(t => {
                  const tc = tagColor(t)
                  return <span key={t} className="text-[10px] px-1.5 py-0.5 rounded" style={{ background: tc.bg, color: tc.text }}>{t}</span>
                })}
              </div>
            )}
            {r.annotation_count !== undefined && r.annotation_count > 0 && (
              <div className="flex items-center gap-1 mt-1 pl-4">
                <MessageSquare size={10} className="text-[#FBBF24]" />
                <span className="text-[10px] text-[#3D5068]">
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
  onOpen: (path: string) => void; onNew: () => void
}) {
  if (loading) return (
    <div className="flex justify-center py-20">
      <Loader2 size={20} className="text-[#6366F1] animate-spin" />
    </div>
  )

  if (docs.length === 0) return (
    <div className="flex flex-col items-center justify-center h-full py-24 gap-5">
      <div className="w-16 h-16 rounded-2xl flex items-center justify-center"
        style={{ background: '#0D1726', border: '1px solid #1E2C45' }}>
        <BookOpen size={26} className="text-[#283A57]" />
      </div>
      <div className="text-center">
        <p className="text-base font-semibold text-[#DDE6F0]">No documents yet</p>
        <p className="text-sm text-[#7E91A8] mt-2 max-w-xs leading-relaxed">
          Agents write docs here automatically, or you can create one manually.
        </p>
        <button onClick={onNew}
          className="mt-4 flex items-center gap-1.5 px-4 py-2 rounded-xl text-sm font-semibold mx-auto cursor-pointer transition-all active:scale-[0.97]"
          style={{ background: 'rgba(99,102,241,0.12)', border: '1px solid rgba(99,102,241,0.25)', color: '#818CF8' }}>
          <Plus size={14} />
          Create first document
        </button>
      </div>
    </div>
  )

  return (
    <div className="p-6 max-w-4xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h2 className="text-lg font-bold text-[#DDE6F0]">
            {tagFilter ? `Tagged "${tagFilter}"` : 'All documents'}
          </h2>
          <p className="text-sm text-[#3D5068] mt-0.5">{docs.length} doc{docs.length !== 1 ? 's' : ''}</p>
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
      style={{ background: '#0D1726', border: '1px solid #1E2C45' }}
      onMouseEnter={e => {
        (e.currentTarget as HTMLElement).style.borderColor = 'rgba(99,102,241,0.3)'
        ;(e.currentTarget as HTMLElement).style.transform = 'translateY(-1px)'
        ;(e.currentTarget as HTMLElement).style.boxShadow = '0 4px 16px rgba(0,0,0,0.3)'
      }}
      onMouseLeave={e => {
        (e.currentTarget as HTMLElement).style.borderColor = '#1E2C45'
        ;(e.currentTarget as HTMLElement).style.transform = ''
        ;(e.currentTarget as HTMLElement).style.boxShadow = ''
      }}
    >
      <div className="w-9 h-9 rounded-xl flex-shrink-0 flex items-center justify-center mt-0.5"
        style={{ background: 'rgba(99,102,241,0.1)', border: '1px solid rgba(99,102,241,0.2)' }}>
        <FileText size={15} className="text-[#818CF8]" />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-start justify-between gap-3">
          <h3 className="text-sm font-semibold text-[#DDE6F0] group-hover:text-white transition-colors leading-snug">
            {doc.title}
          </h3>
          <span className="text-[10px] text-[#3D5068] font-mono flex-shrink-0 mt-0.5">{doc.author}</span>
        </div>
        <p className="text-xs text-[#3D5068] font-mono mt-0.5 truncate">{doc.path}</p>
        {doc.tags.length > 0 && (
          <div className="flex gap-1 mt-2 flex-wrap">
            {doc.tags.map(t => {
              const tc = tagColor(t)
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
function DocView({ doc, onEdit, onDelete, onTagClick, pid, setSelectedDoc }: {
  doc: KBDoc; onEdit: () => void; onDelete: () => void; onTagClick: (tag: string) => void
  pid: number; setSelectedDoc: (doc: KBDoc | null) => void
}) {
  const pathParts = doc.path.split('/')
  const rt = readingTime(doc.content)
  const date = doc.updated_at > 0 ? new Date(doc.updated_at * 1000).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' }) : null
  const [annExpanded, setAnnExpanded] = useState(true)
  const [annText, setAnnText] = useState('')
  const [annSaving, setAnnSaving] = useState(false)

  const handleAddAnnotation = async () => {
    if (!annText.trim()) return
    setAnnSaving(true)
    try {
      await api.kbAnnotate(pid, doc.path, annText, 'user')
      const updatedDoc = await api.kbGet(pid, doc.path)
      setSelectedDoc(updatedDoc)
      setAnnText('')
    } finally {
      setAnnSaving(false)
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
      <div className="flex items-center gap-1.5 text-xs text-[#3D5068] mb-5 flex-wrap">
        {pathParts.map((part, i) => (
          <span key={i} className="flex items-center gap-1.5">
            {i > 0 && <ChevronRight size={10} />}
            <span className={i === pathParts.length - 1 ? 'text-[#7E91A8]' : ''}>{part}</span>
          </span>
        ))}
      </div>

      {/* Title + actions */}
      <div className="flex items-start justify-between gap-4 mb-4">
        <div className="flex-1 min-w-0">
          <h1 className="text-2xl font-bold text-[#DDE6F0] leading-tight mb-3">{doc.title}</h1>
          <div className="flex items-center gap-3 flex-wrap">
            <span className="text-xs text-[#3D5068] flex items-center gap-1">
              <Clock size={10} />
              {rt}
            </span>
            {date && <span className="text-xs text-[#3D5068]">{date}</span>}
            {doc.author && <span className="text-xs text-[#3D5068]">by {doc.author}</span>}
          </div>
        </div>
        <div className="flex items-center gap-2 flex-shrink-0">
          <button onClick={onEdit}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium transition-all cursor-pointer"
            style={{ border: '1px solid #1E2C45', color: '#7E91A8', background: 'transparent' }}
            onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#DDE6F0'; (e.currentTarget as HTMLElement).style.borderColor = '#283A57' }}
            onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#7E91A8'; (e.currentTarget as HTMLElement).style.borderColor = '#1E2C45' }}>
            <Edit3 size={12} />Edit
          </button>
          <button onClick={onDelete}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium transition-all cursor-pointer"
            style={{ border: '1px solid rgba(239,68,68,0.2)', color: '#EF4444', background: 'transparent' }}
            onMouseEnter={e => (e.currentTarget as HTMLElement).style.background = 'rgba(239,68,68,0.08)'}
            onMouseLeave={e => (e.currentTarget as HTMLElement).style.background = 'transparent'}>
            <Trash2 size={12} />
          </button>
        </div>
      </div>

      {/* Tags */}
      {doc.tags.length > 0 && (
        <div className="flex flex-wrap gap-1.5 mb-6">
          {doc.tags.map(tag => {
            const tc = tagColor(tag)
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
      <div className="border-t border-[#192238] mb-6" />

      {/* Markdown content */}
      <div className="prose-amp">
        <ReactMarkdown
          components={{
            h1: ({children}) => <h1 className="text-xl font-bold text-[#DDE6F0] mt-8 mb-3 pb-2 border-b border-[#192238] first:mt-0">{children}</h1>,
            h2: ({children}) => <h2 className="text-base font-bold text-[#DDE6F0] mt-6 mb-2">{children}</h2>,
            h3: ({children}) => <h3 className="text-sm font-bold text-[#DDE6F0] mt-4 mb-1.5">{children}</h3>,
            p: ({children}) => <p className="text-sm text-[#7E91A8] leading-7 mb-4">{children}</p>,
            code: ({children, className}) => {
              const isBlock = className?.includes('language-')
              if (isBlock) return (
                <pre className="rounded-xl p-4 text-xs leading-relaxed overflow-x-auto mb-4 font-mono"
                  style={{ background: '#08101F', border: '1px solid #192238' }}>
                  <code style={{ color: '#DDE6F0' }}>{children}</code>
                </pre>
              )
              return <code className="rounded-md px-1.5 py-0.5 text-xs font-mono" style={{ background: '#08101F', color: '#818CF8', border: '1px solid #192238' }}>{children}</code>
            },
            ul: ({children}) => <ul className="text-sm text-[#7E91A8] list-disc list-outside mb-4 space-y-1.5 pl-5">{children}</ul>,
            ol: ({children}) => <ol className="text-sm text-[#7E91A8] list-decimal list-outside mb-4 space-y-1.5 pl-5">{children}</ol>,
            li: ({children}) => <li className="leading-7">{children}</li>,
            blockquote: ({children}) => (
              <blockquote className="pl-4 my-4 text-sm italic text-[#7E91A8]"
                style={{ borderLeft: '3px solid #6366F1' }}>{children}</blockquote>
            ),
            strong: ({children}) => <strong className="text-[#DDE6F0] font-semibold">{children}</strong>,
            a: ({children, href}) => <a href={href} className="text-[#818CF8] hover:text-[#6366F1] underline underline-offset-2 transition-colors">{children}</a>,
            hr: () => <hr className="border-[#192238] my-6" />,
            table: ({children}) => (
              <div className="overflow-x-auto mb-4">
                <table className="w-full text-sm border-collapse">{children}</table>
              </div>
            ),
            th: ({children}) => <th className="text-left px-4 py-2.5 text-xs font-semibold text-[#7E91A8] uppercase tracking-wide" style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45' }}>{children}</th>,
            td: ({children}) => <td className="px-4 py-2.5 text-[#7E91A8] text-sm" style={{ borderBottom: '1px solid #192238' }}>{children}</td>,
          }}
        >
          {doc.content}
        </ReactMarkdown>
      </div>

      {/* Annotations Panel */}
      <div className="mt-8 pt-6 border-t border-[#192238]">
        <button
          onClick={() => setAnnExpanded(!annExpanded)}
          className="flex items-center gap-2 text-xs font-semibold text-[#7E91A8] hover:text-[#DDE6F0] transition-colors mb-4"
        >
          {annExpanded ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
          <MessageSquare size={12} />
          Annotations
          <span className="ml-1.5 px-1.5 py-0.5 rounded text-[10px] font-bold" style={{ background: 'rgba(99,102,241,0.12)', color: '#818CF8' }}>
            {doc.annotations?.length ?? 0}
          </span>
        </button>
        
        {annExpanded && (
          <div className="space-y-4">
            {/* Annotation List */}
            {doc.annotations && doc.annotations.length > 0 ? (
              <div className="space-y-3">
                {doc.annotations.map((ann, idx) => (
                  <div key={idx} className="p-3 rounded-lg text-xs" style={{ background: 'rgba(255,255,255,0.03)', border: '1px solid #1E2C45' }}>
                    <div className="flex items-center justify-between mb-2">
                      <div className="flex items-center gap-2">
                        <span className="font-semibold text-[#DDE6F0]">{ann.author}</span>
                        <span className="text-[#3D5068]">{formatDate(ann.created_at)}</span>
                      </div>
                      <span className={`px-1.5 py-0.5 rounded text-[10px] font-medium ${
                        ann.is_resolved 
                          ? 'text-[#10B981]' 
                          : 'text-[#F59E0B]'
                      }`} style={{ background: ann.is_resolved ? 'rgba(16,185,129,0.12)' : 'rgba(245,158,11,0.12)' }}>
                        {ann.is_resolved ? 'Resolved' : 'Unresolved'}
                      </span>
                    </div>
                    <p className="text-[#7E91A8] leading-relaxed whitespace-pre-wrap">{ann.text}</p>
                  </div>
                ))}
              </div>
            ) : (
              <div className="text-center py-6">
                <p className="text-xs text-[#3D5068]">No annotations yet</p>
              </div>
            )}

            {/* Add Annotation Form */}
            <div className="pt-4 border-t border-[#192238]">
              <div className="flex items-center gap-2 mb-2">
                <MessageSquare size={12} className="text-[#6366F1]" />
                <span className="text-xs font-semibold text-[#DDE6F0]">Add Annotation</span>
              </div>
              <div className="space-y-2">
                <textarea
                  value={annText}
                  onChange={(e) => setAnnText(e.target.value)}
                  placeholder="Add your annotation here..."
                  className="w-full bg-[#08101F] border border-[#1E2C45] rounded-lg px-3 py-2 text-xs text-[#DDE6F0] placeholder-[#3D5068] focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all font-[inherit] resize-none"
                  rows={3}
                />
                <div className="flex justify-end">
                  <button
                    onClick={handleAddAnnotation}
                    disabled={!annText.trim() || annSaving}
                    className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold transition-all active:scale-[0.97] cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed"
                    style={{ background: 'rgba(99,102,241,0.12)', border: '1px solid rgba(99,102,241,0.25)', color: '#818CF8' }}
                    onMouseEnter={(e) => {
                      if (!annSaving && annText.trim()) {
                        (e.currentTarget as HTMLElement).style.background = 'rgba(99,102,241,0.2)';
                        (e.currentTarget as HTMLElement).style.borderColor = 'rgba(99,102,241,0.4)';
                      }
                    }}
                    onMouseLeave={(e) => {
                      if (!annSaving && annText.trim()) {
                        (e.currentTarget as HTMLElement).style.background = 'rgba(99,102,241,0.12)';
                        (e.currentTarget as HTMLElement).style.borderColor = 'rgba(99,102,241,0.25)';
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
function EditorView({ path, title, content, tags, saving, tab, onPath, onTitle, onContent, onTags, onTab, onSave, onCancel }: {
  path: string; title: string; content: string; tags: string; saving: boolean
  tab: 'write' | 'preview'
  onPath: (v: string) => void; onTitle: (v: string) => void; onContent: (v: string) => void; onTags: (v: string) => void
  onTab: (t: 'write' | 'preview') => void
  onSave: () => void; onCancel: () => void
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
        style={{ borderBottom: '1px solid #1E2C45', background: '#0D1726' }}>
        <div className="flex items-center gap-1">
          {(['write', 'preview'] as const).map(t => (
            <button key={t} onClick={() => onTab(t)}
              className={`flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold transition-all cursor-pointer ${
                tab === t ? 'text-[#DDE6F0]' : 'text-[#3D5068] hover:text-[#7E91A8]'
              }`}
              style={tab === t ? { background: '#172540', border: '1px solid #283A57' } : {}}>
              {t === 'write' ? <Code2 size={12} /> : <Eye size={12} />}
              {t === 'write' ? 'Write' : 'Preview'}
            </button>
          ))}
        </div>
        <div className="flex items-center gap-2">
          <button onClick={onCancel}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium transition-colors cursor-pointer"
            style={{ border: '1px solid #1E2C45', color: '#7E91A8', background: 'transparent' }}
            onMouseEnter={e => (e.currentTarget as HTMLElement).style.color = '#DDE6F0'}
            onMouseLeave={e => (e.currentTarget as HTMLElement).style.color = '#7E91A8'}>
            <X size={12} />Cancel
          </button>
          <button onClick={onSave} disabled={!canSave}
            className="flex items-center gap-1.5 px-4 py-1.5 rounded-lg text-xs font-semibold transition-all active:scale-[0.97] cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed"
            style={{ background: '#6366F1', color: '#fff', border: '1px solid #4F46E5' }}
            onMouseEnter={e => { if (canSave) (e.currentTarget as HTMLElement).style.background = '#4F46E5' }}
            onMouseLeave={e => { if (canSave) (e.currentTarget as HTMLElement).style.background = '#6366F1' }}>
            {saving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
            {saving ? 'Saving…' : 'Save doc'}
          </button>
        </div>
      </div>

      {/* Meta fields */}
      <div className="px-6 py-4 flex-shrink-0 grid grid-cols-3 gap-3" style={{ borderBottom: '1px solid #192238' }}>
        <div className="col-span-1">
          <label className="text-[10px] font-bold text-[#3D5068] uppercase tracking-widest block mb-1">Path</label>
          <input value={path} onChange={e => onPath(e.target.value)} placeholder="folder/doc.md"
            className={`${inputCls} font-mono text-xs`} />
        </div>
        <div className="col-span-1">
          <label className="text-[10px] font-bold text-[#3D5068] uppercase tracking-widest block mb-1">Title</label>
          <input value={title} onChange={e => onTitle(e.target.value)} placeholder="Document title"
            className={inputCls} />
        </div>
        <div className="col-span-1">
          <label className="text-[10px] font-bold text-[#3D5068] uppercase tracking-widest block mb-1">Tags</label>
          <input value={tags} onChange={e => onTags(e.target.value)} placeholder="auth, api, guide"
            className={inputCls} />
        </div>
      </div>

      {/* Write / Preview pane */}
      <div className="flex-1 overflow-hidden">
        {tab === 'write' ? (
          <textarea
            ref={textareaRef}
            value={content}
            onChange={e => onContent(e.target.value)}
            placeholder={"# Title\n\nWrite your document in Markdown…"}
            className="w-full h-full p-6 text-sm text-[#DDE6F0] font-mono resize-none focus:outline-none"
            style={{ background: '#08101F', lineHeight: '1.7', caretColor: '#6366F1' }}
          />
        ) : (
          <div className="p-8 overflow-y-auto h-full max-w-3xl">
            {content ? (
              <ReactMarkdown
                components={{
                  h1: ({children}) => <h1 className="text-xl font-bold text-[#DDE6F0] mt-6 mb-3 pb-2 border-b border-[#192238] first:mt-0">{children}</h1>,
                  h2: ({children}) => <h2 className="text-base font-bold text-[#DDE6F0] mt-5 mb-2">{children}</h2>,
                  h3: ({children}) => <h3 className="text-sm font-bold text-[#DDE6F0] mt-4 mb-1.5">{children}</h3>,
                  p: ({children}) => <p className="text-sm text-[#7E91A8] leading-7 mb-4">{children}</p>,
                  code: ({children, className}) => {
                    const isBlock = className?.includes('language-')
                    if (isBlock) return <pre className="rounded-xl p-4 text-xs overflow-x-auto mb-4 font-mono" style={{ background: '#08101F', border: '1px solid #192238' }}><code style={{ color: '#DDE6F0' }}>{children}</code></pre>
                    return <code className="rounded px-1.5 py-0.5 text-xs font-mono" style={{ background: '#08101F', color: '#818CF8' }}>{children}</code>
                  },
                  ul: ({children}) => <ul className="text-sm text-[#7E91A8] list-disc list-outside mb-4 space-y-1 pl-5">{children}</ul>,
                  li: ({children}) => <li className="leading-7">{children}</li>,
                  strong: ({children}) => <strong className="text-[#DDE6F0] font-semibold">{children}</strong>,
                  blockquote: ({children}) => <blockquote className="pl-4 my-4 italic text-[#7E91A8]" style={{ borderLeft: '3px solid #6366F1' }}>{children}</blockquote>,
                }}>
                {content}
              </ReactMarkdown>
            ) : (
              <p className="text-sm text-[#3D5068] italic">Nothing to preview yet.</p>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
