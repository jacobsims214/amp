import { useState, useEffect, useCallback } from 'react'
import { useParams, Link } from 'react-router-dom'
import { ArrowLeft, Plus, Search, FileText, Tag, Trash2, Edit3, Save, X, Loader2, ChevronRight, ChevronDown, BookOpen } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import { api } from '../api/client'
import type { KBDoc, KBDocSummary, KBSearchResult, KBTagCount } from '../types'

// ---- Views ----
type View = 'list' | 'doc' | 'editor' | 'search'

export function KB() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)

  const [view, setView] = useState<View>('list')
  const [docs, setDocs] = useState<KBDocSummary[]>([])
  const [tags, setTags] = useState<KBTagCount[]>([])
  const [selectedDoc, setSelectedDoc] = useState<KBDoc | null>(null)
  const [searchQuery, setSearchQuery] = useState('')
  const [searchResults, setSearchResults] = useState<KBSearchResult[]>([])
  const [searching, setSearching] = useState(false)
  const [loading, setLoading] = useState(true)
  const [tagFilter, setTagFilter] = useState<string | null>(null)
  const [expandedDirs, setExpandedDirs] = useState<Set<string>>(new Set())

  // Editor state
  const [editPath, setEditPath] = useState('')
  const [editTitle, setEditTitle] = useState('')
  const [editContent, setEditContent] = useState('')
  const [editTags, setEditTags] = useState('')
  const [saving, setSaving] = useState(false)

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

  const openDoc = async (path: string) => {
    const doc = await api.kbGet(pid, path)
    setSelectedDoc(doc)
    setView('doc')
  }

  const openEditor = (doc?: KBDoc | null) => {
    if (doc) {
      setEditPath(doc.path)
      setEditTitle(doc.title)
      setEditContent(doc.content)
      setEditTags(doc.tags.join(', '))
    } else {
      setEditPath('')
      setEditTitle('')
      setEditContent('')
      setEditTags('')
    }
    setView('editor')
  }

  const saveDoc = async () => {
    if (!editPath || !editTitle || !editContent) return
    setSaving(true)
    try {
      const tags = editTags.split(',').map(t => t.trim()).filter(Boolean)
      await api.kbWrite(pid, editPath, editTitle, editContent, tags)
      await loadDocs()
      const doc = await api.kbGet(pid, editPath)
      setSelectedDoc(doc)
      setView('doc')
    } finally {
      setSaving(false)
    }
  }

  const deleteDoc = async (path: string) => {
    if (!confirm(`Delete "${path}"?`)) return
    await api.kbDelete(pid, path)
    setSelectedDoc(null)
    setView('list')
    loadDocs()
  }

  const doSearch = async (q: string) => {
    if (!q.trim()) { setSearchResults([]); return }
    setSearching(true)
    try {
      const results = await api.kbSearch(pid, q)
      setSearchResults(results)
    } finally {
      setSearching(false)
    }
  }

  useEffect(() => {
    const t = setTimeout(() => { if (view === 'search') doSearch(searchQuery) }, 350)
    return () => clearTimeout(t)
  }, [searchQuery, view])

  // Build file tree from paths
  const tree = buildTree(docs)

  return (
    <div className="flex flex-col h-screen bg-[#0d1117]">
      {/* Header */}
      <header className="flex items-center gap-3 px-5 py-3 border-b border-[#21262d] bg-[#161b22] flex-shrink-0">
        <Link to={`/project/${pid}`} className="flex items-center gap-1.5 text-[#8b949e] hover:text-[#e6edf3] transition-colors">
          <ArrowLeft size={14} />
        </Link>
        <div className="w-px h-4 bg-[#30363d]" />
        <BookOpen size={14} className="text-[#8b949e]" />
        <span className="text-sm font-semibold text-[#e6edf3]">Knowledge Base</span>
        <div className="ml-auto flex items-center gap-2">
          <button
            onClick={() => { setView('search'); setSearchQuery('') }}
            className={`flex items-center gap-1.5 px-2.5 py-1.5 rounded-md text-xs border transition-colors ${
              view === 'search'
                ? 'bg-[#58a6ff]/10 border-[#58a6ff]/30 text-[#58a6ff]'
                : 'border-[#30363d] text-[#8b949e] hover:text-[#e6edf3] hover:border-[#484f58]'
            }`}
          >
            <Search size={12} />
            Search
          </button>
          <button
            onClick={() => openEditor(null)}
            className="flex items-center gap-1.5 px-2.5 py-1.5 rounded-md text-xs bg-[#238636] hover:bg-[#2ea043] text-white border border-[#2ea043] transition-colors"
          >
            <Plus size={12} />
            New Doc
          </button>
        </div>
      </header>

      <div className="flex flex-1 overflow-hidden">
        {/* Sidebar — file tree */}
        <aside className="w-56 flex-shrink-0 border-r border-[#21262d] bg-[#0d1117] overflow-y-auto">
          {/* Tag filter */}
          {tags.length > 0 && (
            <div className="px-3 pt-3 pb-2">
              <div className="text-xs text-[#484f58] uppercase tracking-wide mb-2">Tags</div>
              <div className="flex flex-wrap gap-1">
                <button
                  onClick={() => setTagFilter(null)}
                  className={`text-xs px-2 py-0.5 rounded-full transition-colors ${
                    !tagFilter ? 'bg-[#21262d] text-[#e6edf3]' : 'text-[#8b949e] hover:text-[#e6edf3]'
                  }`}
                >
                  all
                </button>
                {tags.slice(0, 10).map(t => (
                  <button
                    key={t.tag}
                    onClick={() => setTagFilter(tagFilter === t.tag ? null : t.tag)}
                    className={`text-xs px-2 py-0.5 rounded-full transition-colors ${
                      tagFilter === t.tag
                        ? 'bg-[#58a6ff]/20 text-[#58a6ff]'
                        : 'text-[#8b949e] hover:text-[#e6edf3]'
                    }`}
                  >
                    {t.tag}
                    <span className="ml-1 text-[#484f58]">{t.count}</span>
                  </button>
                ))}
              </div>
            </div>
          )}

          {/* File tree */}
          <div className="px-2 py-2">
            {loading ? (
              <div className="flex justify-center py-4">
                <Loader2 size={16} className="animate-spin text-[#8b949e]" />
              </div>
            ) : docs.length === 0 ? (
              <p className="text-xs text-[#484f58] px-2 py-4 text-center">
                No docs yet.<br />Click "New Doc" to start.
              </p>
            ) : (
              <TreeNode
                node={tree}
                depth={0}
                expanded={expandedDirs}
                onToggleDir={(dir) => setExpandedDirs(prev => {
                  const s = new Set(prev)
                  s.has(dir) ? s.delete(dir) : s.add(dir)
                  return s
                })}
                onSelectDoc={openDoc}
                selectedPath={selectedDoc?.path}
              />
            )}
          </div>
        </aside>

        {/* Main content */}
        <main className="flex-1 overflow-y-auto">
          {view === 'search' && (
            <SearchPanel
              query={searchQuery}
              onQuery={setSearchQuery}
              results={searchResults}
              searching={searching}
              onSelect={(path) => openDoc(path)}
            />
          )}

          {view === 'list' && (
            <div className="p-6">
              <h2 className="text-sm font-semibold text-[#e6edf3] mb-4">
                {tagFilter ? `Docs tagged "${tagFilter}"` : 'All documents'}
                <span className="ml-2 text-xs text-[#484f58] font-normal">({docs.length})</span>
              </h2>
              {docs.length === 0 && !loading && (
                <div className="flex flex-col items-center justify-center py-12 gap-3">
                  <BookOpen size={32} className="text-[#484f58]" />
                  <p className="text-sm text-[#8b949e]">No documents yet</p>
                  <p className="text-xs text-[#484f58]">Agents will write docs here, or click "New Doc" to add one yourself.</p>
                </div>
              )}
              <div className="grid gap-2">
                {docs.map(doc => (
                  <div
                    key={doc.id}
                    onClick={() => openDoc(doc.path)}
                    className="flex items-start gap-3 p-3 rounded-lg border border-[#21262d] bg-[#161b22] hover:border-[#30363d] hover:bg-[#1c2333] cursor-pointer transition-colors"
                  >
                    <FileText size={14} className="text-[#8b949e] mt-0.5 flex-shrink-0" />
                    <div className="flex-1 min-w-0">
                      <div className="text-sm font-medium text-[#e6edf3] truncate">{doc.title}</div>
                      <div className="text-xs text-[#484f58] font-mono truncate">{doc.path}</div>
                      {doc.tags.length > 0 && (
                        <div className="flex gap-1 mt-1 flex-wrap">
                          {doc.tags.map(t => (
                            <span key={t} className="text-xs px-1.5 py-0.5 rounded bg-[#21262d] text-[#8b949e]">{t}</span>
                          ))}
                        </div>
                      )}
                    </div>
                    <span className="text-xs text-[#484f58] flex-shrink-0">{doc.author}</span>
                  </div>
                ))}
              </div>
            </div>
          )}

          {view === 'doc' && selectedDoc && (
            <DocView
              doc={selectedDoc}
              onEdit={() => openEditor(selectedDoc)}
              onDelete={() => deleteDoc(selectedDoc.path)}
              onTagClick={(tag) => { setTagFilter(tag); setView('list') }}
            />
          )}

          {view === 'editor' && (
            <EditorView
              path={editPath}
              title={editTitle}
              content={editContent}
              tags={editTags}
              saving={saving}
              onPath={setEditPath}
              onTitle={setEditTitle}
              onContent={setEditContent}
              onTags={setEditTags}
              onSave={saveDoc}
              onCancel={() => setView(selectedDoc ? 'doc' : 'list')}
            />
          )}
        </main>
      </div>
    </div>
  )
}

// ---- File Tree ----
interface TreeNodeData {
  name: string
  fullPath: string
  isDir: boolean
  children?: TreeNodeData[]
  doc?: KBDocSummary
}

function buildTree(docs: KBDocSummary[]): TreeNodeData {
  const root: TreeNodeData = { name: '', fullPath: '', isDir: true, children: [] }

  for (const doc of docs) {
    const parts = doc.path.split('/')
    let node = root
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i]
      const isLast = i === parts.length - 1
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
  node: TreeNodeData
  depth: number
  expanded: Set<string>
  onToggleDir: (path: string) => void
  onSelectDoc: (path: string) => void
  selectedPath?: string
}) {
  const children = node.children ?? []
  const isExpanded = !node.fullPath || expanded.has(node.fullPath)

  if (node.isDir && node.fullPath) {
    return (
      <div>
        <button
          onClick={() => onToggleDir(node.fullPath)}
          className="flex items-center gap-1 w-full px-2 py-1 rounded text-xs text-[#8b949e] hover:text-[#e6edf3] hover:bg-[#161b22] transition-colors"
          style={{ paddingLeft: `${8 + depth * 12}px` }}
        >
          {isExpanded ? <ChevronDown size={11} /> : <ChevronRight size={11} />}
          <span className="font-medium">{node.name}</span>
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
        className={`flex items-center gap-1.5 w-full px-2 py-1 rounded text-xs transition-colors ${
          isSelected
            ? 'bg-[#58a6ff]/10 text-[#58a6ff]'
            : 'text-[#8b949e] hover:text-[#e6edf3] hover:bg-[#161b22]'
        }`}
        style={{ paddingLeft: `${8 + depth * 12}px` }}
      >
        <FileText size={10} className="flex-shrink-0" />
        <span className="truncate">{node.name}</span>
      </button>
    )
  }

  // Root node — just render children
  return (
    <>
      {children.map(child => (
        <TreeNode key={child.fullPath} node={child} depth={depth}
          expanded={expanded} onToggleDir={onToggleDir}
          onSelectDoc={onSelectDoc} selectedPath={selectedPath} />
      ))}
    </>
  )
}

// ---- Doc Viewer ----
function DocView({ doc, onEdit, onDelete, onTagClick }: {
  doc: KBDoc
  onEdit: () => void
  onDelete: () => void
  onTagClick: (tag: string) => void
}) {
  return (
    <div className="p-6 max-w-3xl">
      <div className="flex items-start justify-between mb-4">
        <div>
          <h1 className="text-xl font-semibold text-[#e6edf3] mb-1">{doc.title}</h1>
          <div className="flex items-center gap-3 text-xs text-[#484f58]">
            <span className="font-mono">{doc.path}</span>
            <span>·</span>
            <span>{doc.author}</span>
            {doc.updated_at > 0 && (
              <>
                <span>·</span>
                <span>{new Date(doc.updated_at * 1000).toLocaleDateString()}</span>
              </>
            )}
          </div>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={onEdit}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs border border-[#30363d] text-[#8b949e] hover:text-[#e6edf3] hover:border-[#484f58] transition-colors"
          >
            <Edit3 size={12} />
            Edit
          </button>
          <button
            onClick={onDelete}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs border border-[#f85149]/30 text-[#f85149] hover:bg-[#f85149]/10 transition-colors"
          >
            <Trash2 size={12} />
          </button>
        </div>
      </div>

      {doc.tags.length > 0 && (
        <div className="flex flex-wrap gap-1.5 mb-5">
          {doc.tags.map(tag => (
            <button
              key={tag}
              onClick={() => onTagClick(tag)}
              className="flex items-center gap-1 px-2 py-0.5 rounded-full text-xs bg-[#21262d] text-[#8b949e] hover:text-[#e6edf3] transition-colors"
            >
              <Tag size={9} />
              {tag}
            </button>
          ))}
        </div>
      )}

      <div className="prose prose-sm prose-invert max-w-none">
        <ReactMarkdown
          components={{
            h1: ({children}) => <h1 className="text-lg font-bold text-[#e6edf3] mt-6 mb-3 pb-2 border-b border-[#21262d]">{children}</h1>,
            h2: ({children}) => <h2 className="text-base font-semibold text-[#e6edf3] mt-5 mb-2">{children}</h2>,
            h3: ({children}) => <h3 className="text-sm font-semibold text-[#e6edf3] mt-4 mb-1.5">{children}</h3>,
            p: ({children}) => <p className="text-sm text-[#8b949e] leading-relaxed mb-3">{children}</p>,
            code: ({children, className}) => {
              const isBlock = className?.includes('language-')
              if (isBlock) return (
                <code className="block bg-[#161b22] border border-[#21262d] rounded-md p-3 text-xs text-[#e6edf3] font-mono overflow-x-auto mb-3">
                  {children}
                </code>
              )
              return <code className="bg-[#161b22] px-1.5 py-0.5 rounded text-xs text-[#58a6ff] font-mono">{children}</code>
            },
            ul: ({children}) => <ul className="text-sm text-[#8b949e] list-disc list-inside mb-3 space-y-1">{children}</ul>,
            ol: ({children}) => <ol className="text-sm text-[#8b949e] list-decimal list-inside mb-3 space-y-1">{children}</ol>,
            li: ({children}) => <li className="leading-relaxed">{children}</li>,
            blockquote: ({children}) => (
              <blockquote className="border-l-2 border-[#388bfd] pl-4 my-3 text-[#8b949e] text-sm italic">{children}</blockquote>
            ),
            strong: ({children}) => <strong className="text-[#e6edf3] font-semibold">{children}</strong>,
            a: ({children, href}) => <a href={href} className="text-[#58a6ff] hover:underline">{children}</a>,
            hr: () => <hr className="border-[#21262d] my-4" />,
          }}
        >
          {doc.content}
        </ReactMarkdown>
      </div>
    </div>
  )
}

// ---- Editor ----
function EditorView({ path, title, content, tags, saving, onPath, onTitle, onContent, onTags, onSave, onCancel }: {
  path: string
  title: string
  content: string
  tags: string
  saving: boolean
  onPath: (v: string) => void
  onTitle: (v: string) => void
  onContent: (v: string) => void
  onTags: (v: string) => void
  onSave: () => void
  onCancel: () => void
}) {
  return (
    <div className="p-6 max-w-3xl">
      <div className="flex items-center justify-between mb-5">
        <h2 className="text-sm font-semibold text-[#e6edf3]">{path ? `Edit: ${path}` : 'New document'}</h2>
        <div className="flex items-center gap-2">
          <button onClick={onCancel} className="flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs border border-[#30363d] text-[#8b949e] hover:text-[#e6edf3] transition-colors">
            <X size={12} />Cancel
          </button>
          <button
            onClick={onSave}
            disabled={saving || !path || !title || !content}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs bg-[#238636] hover:bg-[#2ea043] disabled:opacity-50 text-white transition-colors"
          >
            {saving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
            {saving ? 'Saving…' : 'Save'}
          </button>
        </div>
      </div>

      <div className="space-y-4">
        <div>
          <label className="block text-xs text-[#8b949e] mb-1">Path <span className="text-[#484f58]">(e.g. architecture/auth.md)</span></label>
          <input
            value={path}
            onChange={e => onPath(e.target.value)}
            placeholder="architecture/auth.md"
            className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-2 text-sm text-[#e6edf3] font-mono placeholder-[#484f58] focus:outline-none focus:border-[#58a6ff]/60 focus:ring-1 focus:ring-[#58a6ff]/30"
          />
        </div>
        <div>
          <label className="block text-xs text-[#8b949e] mb-1">Title</label>
          <input
            value={title}
            onChange={e => onTitle(e.target.value)}
            placeholder="Document title"
            className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-2 text-sm text-[#e6edf3] placeholder-[#484f58] focus:outline-none focus:border-[#58a6ff]/60 focus:ring-1 focus:ring-[#58a6ff]/30"
          />
        </div>
        <div>
          <label className="block text-xs text-[#8b949e] mb-1">Tags <span className="text-[#484f58]">(comma-separated)</span></label>
          <input
            value={tags}
            onChange={e => onTags(e.target.value)}
            placeholder="auth, jwt, middleware"
            className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-2 text-sm text-[#e6edf3] placeholder-[#484f58] focus:outline-none focus:border-[#58a6ff]/60 focus:ring-1 focus:ring-[#58a6ff]/30"
          />
        </div>
        <div>
          <label className="block text-xs text-[#8b949e] mb-1">Content <span className="text-[#484f58]">(markdown)</span></label>
          <textarea
            value={content}
            onChange={e => onContent(e.target.value)}
            placeholder="# Title&#10;&#10;Document content in markdown..."
            rows={20}
            className="w-full bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-2 text-sm text-[#e6edf3] font-mono placeholder-[#484f58] focus:outline-none focus:border-[#58a6ff]/60 focus:ring-1 focus:ring-[#58a6ff]/30 resize-y"
          />
        </div>
      </div>
    </div>
  )
}

// ---- Search Panel ----
function SearchPanel({ query, onQuery, results, searching, onSelect }: {
  query: string
  onQuery: (q: string) => void
  results: KBSearchResult[]
  searching: boolean
  onSelect: (path: string) => void
}) {
  return (
    <div className="p-6 max-w-3xl">
      <div className="relative mb-5">
        <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-[#8b949e]" />
        <input
          autoFocus
          value={query}
          onChange={e => onQuery(e.target.value)}
          placeholder="Search documents semantically…"
          className="w-full bg-[#0d1117] border border-[#30363d] rounded-md pl-9 pr-4 py-2.5 text-sm text-[#e6edf3] placeholder-[#484f58] focus:outline-none focus:border-[#58a6ff]/60 focus:ring-1 focus:ring-[#58a6ff]/30"
        />
        {searching && <Loader2 size={14} className="absolute right-3 top-1/2 -translate-y-1/2 text-[#58a6ff] animate-spin" />}
      </div>

      {query && !searching && results.length === 0 && (
        <p className="text-sm text-[#8b949e] text-center py-8">No results found</p>
      )}

      <div className="space-y-3">
        {results.map((result, i) => (
          <div
            key={i}
            onClick={() => onSelect(result.path)}
            className="p-4 rounded-lg border border-[#21262d] bg-[#161b22] hover:border-[#30363d] hover:bg-[#1c2333] cursor-pointer transition-colors"
          >
            <div className="flex items-start justify-between gap-2 mb-1.5">
              <span className="text-sm font-medium text-[#e6edf3]">{result.title}</span>
              <span className="text-xs text-[#484f58] font-mono flex-shrink-0">{result.path}</span>
            </div>
            {result.excerpt && (
              <p className="text-xs text-[#8b949e] leading-relaxed line-clamp-3">{result.excerpt}</p>
            )}
            {result.tags.length > 0 && (
              <div className="flex gap-1 mt-2">
                {result.tags.map(t => (
                  <span key={t} className="text-xs px-1.5 py-0.5 rounded bg-[#21262d] text-[#8b949e]">{t}</span>
                ))}
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}
