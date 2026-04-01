import { useState, useEffect, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { Loader2, LayoutDashboard, Clock, Layers, Wifi } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
import type { Project, SSEEvent } from '../types'

function relativeTime(iso: string) {
  const diff = Date.now() - new Date(iso).getTime()
  const days = Math.floor(diff / 86400000)
  const hrs = Math.floor(diff / 3600000)
  const mins = Math.floor(diff / 60000)
  if (days > 0) return `${days}d ago`
  if (hrs > 0) return `${hrs}h ago`
  if (mins > 0) return `${mins}m ago`
  return 'just now'
}

export function Projects() {
  const [projects, setProjects] = useState<Project[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [live, setLive] = useState(false)

  useEffect(() => {
    api.listProjects()
      .then(setProjects)
      .catch(e => setError(e.message))
      .finally(() => setLoading(false))
  }, [])

  // SSE: add new projects the moment the manager creates them
  useSSE(null, useCallback((event: SSEEvent) => {
    if (event.type === 'connected') {
      setLive(true)
      return
    }
    if (event.type === 'project.created' && event.payload) {
      const p = event.payload as Project
      setProjects(prev => prev.find(x => x.id === p.id) ? prev : [...prev, p])
    }
  }, []))

  return (
    <div className="min-h-screen bg-[#0d1117]">
      <header className="border-b border-[#21262d] bg-[#161b22]">
        <div className="max-w-5xl mx-auto px-6 py-4 flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div className="w-7 h-7 rounded-md bg-[#58a6ff]/20 border border-[#58a6ff]/30 flex items-center justify-center">
              <LayoutDashboard size={14} className="text-[#58a6ff]" />
            </div>
            <span className="text-base font-semibold text-[#e6edf3]">AMP</span>
            <span className="text-xs text-[#484f58] bg-[#21262d] px-2 py-0.5 rounded-full font-mono">v2</span>
          </div>
          <div className="flex items-center gap-3">
            <div className={`flex items-center gap-1.5 text-xs ${live ? 'text-[#3fb950]' : 'text-[#484f58]'}`}>
              <Wifi size={12} />
              <span>{live ? 'Live' : 'Connecting…'}</span>
            </div>
            <span className="text-xs text-[#8b949e]">
              {projects.length} project{projects.length !== 1 ? 's' : ''}
            </span>
          </div>
        </div>
      </header>

      <main className="max-w-5xl mx-auto px-6 py-8">
        <div className="mb-6">
          <h1 className="text-xl font-semibold text-[#e6edf3]">Projects</h1>
          <p className="text-sm text-[#8b949e] mt-1">Select a project to open its board</p>
        </div>

        {loading && (
          <div className="flex justify-center py-16">
            <Loader2 size={24} className="text-[#58a6ff] animate-spin" />
          </div>
        )}

        {error && (
          <div className="rounded-lg border border-[#f85149]/30 bg-[#f85149]/10 px-4 py-3 text-sm text-[#f85149]">
            {error} — is amp-api running on port 3001?
          </div>
        )}

        {!loading && !error && projects.length === 0 && (
          <div className="flex flex-col items-center justify-center py-20 gap-4">
            <div className="w-14 h-14 rounded-xl bg-[#161b22] border border-[#30363d] flex items-center justify-center">
              <Layers size={24} className="text-[#484f58]" />
            </div>
            <div className="text-center">
              <p className="text-sm font-medium text-[#e6edf3]">No projects yet</p>
              <p className="text-xs text-[#8b949e] mt-1.5 max-w-xs">
                Ask the <code className="bg-[#161b22] px-1.5 py-0.5 rounded text-[#58a6ff] font-mono text-xs">amp-manager</code> to create a project — it will appear here instantly.
              </p>
            </div>
          </div>
        )}

        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {projects.map(p => (
            <Link
              key={p.id}
              to={`/project/${p.id}`}
              className="group block rounded-xl border border-[#30363d] bg-[#161b22] p-5 hover:border-[#58a6ff]/40 hover:bg-[#1c2333] transition-all duration-150 hover:shadow-lg hover:shadow-black/20"
            >
              <div className="flex items-start justify-between gap-3 mb-3">
                <div className="w-9 h-9 rounded-lg bg-[#58a6ff]/10 border border-[#58a6ff]/20 flex items-center justify-center flex-shrink-0">
                  <span className="text-sm font-bold text-[#58a6ff]">
                    {p.code.slice(0, 2).toUpperCase()}
                  </span>
                </div>
                <span className="text-xs text-[#484f58] font-mono mt-1">#{p.id}</span>
              </div>
              <h3 className="text-sm font-semibold text-[#e6edf3] group-hover:text-white transition-colors mb-1 leading-snug">
                {p.name}
              </h3>
              <p className="text-xs text-[#8b949e] font-mono mb-3">{p.code}</p>
              {p.description && (
                <p className="text-xs text-[#8b949e] mb-3 line-clamp-2 leading-relaxed">{p.description}</p>
              )}
              <div className="flex items-center gap-1.5 text-xs text-[#484f58]">
                <Clock size={11} />
                <span>Updated {relativeTime(p.updated_at)}</span>
              </div>
            </Link>
          ))}
        </div>
      </main>
    </div>
  )
}
