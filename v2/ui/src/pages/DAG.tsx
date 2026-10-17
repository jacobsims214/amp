/**
 * DAG view — how the work depends on itself, built to stay readable on big
 * projects:
 *   - Overview first: each story is one node (progress, states, blockers);
 *     expand a story to see its tickets, grouped in a cluster.
 *   - Focus: click a ticket to light up what it waits on and what waits on
 *     it; "Focus chain" shows only that chain, framed to fit.
 *   - Search jumps to any ticket. Filters match the board (epic, mine,
 *     hide done).
 *   - "What changed" highlights tickets that moved today or this week, with
 *     a list to jump through them.
 */
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import {
  Background, Controls, Handle, MiniMap, Position, ReactFlow, ReactFlowProvider, useReactFlow,
  type Edge, type Node, type NodeProps,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import {
  ChevronDown, ChevronRight, Clock, Crosshair, ExternalLink, EyeOff, Eye, Layers, Loader2, Search, User, X,
} from 'lucide-react'
import { api } from '../api/client'
import { useBoardData } from '../hooks/useBoardData'
import { useAuth } from '../hooks/useAuth'
import { TaskDrawer } from '../components/TaskDrawer'
import { ProjectNav } from '../components/ProjectNav'
import {
  buildGraph, chainOf, countStates, ticketNodeId, STORY_H, STORY_W, TICKET_H, TICKET_W, type StateCounts,
} from '../lib/dagModel'
import type { ActivityLog, Epic, Story, Task } from '../types'

// ─── Colours ──────────────────────────────────────────────────────────────────
const STATE_C = {
  backlog:     { border: 'var(--line-str)', bg: 'var(--surface-2)', text: 'var(--ink-2)', bar: 'var(--ink-3)', label: 'Backlog' },
  in_progress: { border: 'var(--primary)', bg: 'var(--surface-2)', text: 'var(--link)', bar: 'var(--primary)', label: 'In progress' },
  completed:   { border: 'var(--success)', bg: 'var(--surface-2)', text: 'var(--success)', bar: 'var(--success)', label: 'Done' },
  blocked:     { border: 'var(--danger)', bg: 'var(--surface-2)', text: 'var(--danger-ink)', bar: 'var(--danger)', label: 'Blocked' },
} as const
type SK = keyof typeof STATE_C
const sc = (s: string) => STATE_C[s as SK] ?? STATE_C.backlog
const CHANGED = 'var(--warning)'
const EPIC_PALETTE = ['var(--primary)', 'var(--success)', 'var(--warning)', 'var(--danger)', 'var(--violet)', 'var(--teal)', 'var(--orange)', 'var(--success-ink)']

/** Expand every story by default on small projects; start collapsed on big ones. */
const EXPAND_ALL_UP_TO = 40

type Changes = 'off' | '1d' | '7d'
const changesSince = (c: Changes) => {
  if (c === 'off') return null
  const d = new Date()
  if (c === '1d') d.setHours(0, 0, 0, 0)
  else d.setDate(d.getDate() - 7)
  return d.toISOString()
}

// ─── Node components ──────────────────────────────────────────────────────────
type Mark = 'selected' | 'up' | 'down' | 'dim' | null

interface TicketData extends Record<string, unknown> {
  task: Task; epicColor: string; mark: Mark; changed: boolean; onOpen: (t: Task) => void
}
interface StoryData extends Record<string, unknown> {
  story: Story; epic?: Epic; epicColor: string; tasks: Task[]; counts: StateCounts
  dim: boolean; changed: number; onExpand: (storyId: number) => void
}
interface ClusterData extends Record<string, unknown> {
  story: Story; epic?: Epic; epicColor: string; counts: StateCounts; onCollapse: (storyId: number) => void
}

const hidden = { opacity: 0, width: 6, height: 6, border: 'none', background: 'transparent' }

const TicketNode = memo(function TicketNode({ data }: NodeProps<Node<TicketData>>) {
  const { task, epicColor, mark, changed, onOpen } = data
  const c = sc(task.state)
  const ring =
    mark === 'selected' ? '0 0 0 2px var(--ink)' :
    mark === 'up' ? '0 0 0 2px var(--link)' :
    mark === 'down' ? '0 0 0 2px var(--violet)' :
    changed ? `0 0 0 2px ${CHANGED}` : undefined
  return (
    <div onDoubleClick={() => onOpen(task)}
      className="rounded-xl overflow-hidden transition-opacity"
      style={{ width: TICKET_W, height: TICKET_H, background: c.bg, border: `1px solid ${c.border}`, boxShadow: ring, opacity: mark === 'dim' ? 0.25 : 1 }}>
      <Handle type="target" position={Position.Left} style={hidden} />
      <div className="h-1" style={{ background: epicColor }} />
      <div className="px-3 py-2 flex flex-col gap-1 h-[calc(100%-4px)]">
        <div className="flex items-center gap-1.5 text-[10px] font-semibold">
          <span className="font-mono text-ink-2">#{task.id}</span>
          <span style={{ color: c.text }}>{c.label}</span>
          {changed && <span className="ml-auto" style={{ color: CHANGED }} title="Changed recently"><Clock size={10} /></span>}
        </div>
        <div className="text-xs font-semibold text-ink leading-snug line-clamp-2">{task.name}</div>
        {task.state === 'blocked' && task.block_reason && (
          <div className="text-[10px] text-danger-ink truncate" title={task.block_reason}>{task.block_reason}</div>
        )}
      </div>
      <Handle type="source" position={Position.Right} style={hidden} />
    </div>
  )
})

function ProgressBar({ counts, total }: { counts: StateCounts; total: number }) {
  return (
    <div className="flex h-1.5 rounded-full overflow-hidden bg-surface-3">
      {(['completed', 'in_progress', 'blocked', 'backlog'] as const).map(s =>
        counts[s] ? <div key={s} style={{ width: `${(counts[s] / total) * 100}%`, background: sc(s).bar }} /> : null)}
    </div>
  )
}

const StoryNode = memo(function StoryNode({ data }: NodeProps<Node<StoryData>>) {
  const { story, epic, epicColor, tasks, counts, dim, changed, onExpand } = data
  const total = tasks.length
  return (
    <div onClick={() => onExpand(story.id)} title="Click to show its tickets"
      className="rounded-xl overflow-hidden cursor-pointer transition-opacity"
      style={{ width: STORY_W, height: STORY_H, background: 'var(--surface)', border: `1px solid ${counts.blocked ? 'var(--danger)' : 'var(--line-str)'}`,
        boxShadow: changed ? `0 0 0 2px ${CHANGED}` : undefined, opacity: dim ? 0.25 : 1 }}>
      <Handle type="target" position={Position.Left} style={hidden} />
      <div className="h-1" style={{ background: epicColor }} />
      <div className="px-3 py-2 space-y-1.5">
        <div className="flex items-center gap-1.5 text-[10px]">
          <ChevronRight size={11} className="text-ink-2" />
          <span className="text-ink-2 truncate">{epic?.name ?? 'Epic'}</span>
          {changed > 0 && <span className="ml-auto font-semibold" style={{ color: CHANGED }}>{changed} changed</span>}
        </div>
        <div className="text-xs font-semibold text-ink truncate">{story.name}</div>
        <ProgressBar counts={counts} total={total} />
        <div className="flex gap-2 text-[10px] text-ink-2">
          <span>{counts.completed}/{total} done</span>
          {counts.in_progress > 0 && <span style={{ color: STATE_C.in_progress.text }}>{counts.in_progress} running</span>}
          {counts.blocked > 0 && <span style={{ color: STATE_C.blocked.text }}>{counts.blocked} blocked</span>}
        </div>
      </div>
      <Handle type="source" position={Position.Right} style={hidden} />
    </div>
  )
})

const ClusterNode = memo(function ClusterNode({ data }: NodeProps<Node<ClusterData>>) {
  const { story, epic, epicColor, counts, onCollapse } = data
  const total = counts.backlog + counts.in_progress + counts.blocked + counts.completed
  return (
    <div className="w-full h-full rounded-2xl" style={{ background: 'var(--surface-2)', border: `1px dashed ${epicColor}55` }}>
      <button onClick={() => onCollapse(story.id)} title="Collapse into one node"
        className="flex items-center gap-1.5 px-2.5 h-5 text-[10px] text-ink-2 hover:text-ink cursor-pointer max-w-full">
        <ChevronDown size={11} className="flex-shrink-0" />
        <span className="w-1.5 h-1.5 rounded-full flex-shrink-0" style={{ background: epicColor }} />
        <span className="truncate">{epic ? `${epic.name} › ` : ''}<span className="text-ink font-semibold">{story.name}</span></span>
        <span className="flex-shrink-0 text-ink-3">{counts.completed}/{total}</span>
      </button>
    </div>
  )
})

const nodeTypes = { ticket: TicketNode, story: StoryNode, cluster: ClusterNode }

// ─── Page ─────────────────────────────────────────────────────────────────────
export function DAGView() {
  return (
    <ReactFlowProvider>
      <DAGCanvas />
    </ReactFlowProvider>
  )
}

function DAGCanvas() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)
  const { epics, stories, tasks, loading, error, refresh } = useBoardData(pid)
  const { me } = useAuth()
  const meId = me?.user_id
  const flow = useReactFlow()

  const [epicFilter, setEpicFilter] = useState<number | null>(null)
  const [hideDone, setHideDone] = useState(false)
  const [mineOnly, setMineOnly] = useState(false)
  const [expandedSet, setExpanded] = useState<Set<number> | null>(null) // null: the default for the project's size
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [focusChain, setFocusChain] = useState(false)
  const [changes, setChanges] = useState<Changes>('off')
  const [query, setQuery] = useState('')
  const [drawerTask, setDrawerTask] = useState<Task | null>(null)

  const taskById = useMemo(() => new Map(tasks.map(t => [t.id, t])), [tasks])
  const epicColor = useMemo(() => {
    const m = new Map(epics.map((e, i) => [e.id, EPIC_PALETTE[i % EPIC_PALETTE.length]]))
    return (id: number) => m.get(id) ?? EPIC_PALETTE[0]
  }, [epics])

  const expanded = useMemo(
    () => expandedSet ?? new Set(tasks.length <= EXPAND_ALL_UP_TO ? stories.map(s => s.id) : []),
    [expandedSet, tasks.length, stories],
  )

  // Recent changes (refetched as tickets change while it's on).
  const [fetched, setActivity] = useState<ActivityLog[]>([])
  useEffect(() => {
    const since = changesSince(changes)
    if (since) api.getProjectActivity(pid, since).then(setActivity).catch(() => setActivity([]))
  }, [pid, changes, tasks])
  const activity = useMemo(() => (changes === 'off' ? [] : fetched), [changes, fetched])
  const changed = useMemo(() => new Set(activity.map(a => a.task_id)), [activity])

  const chain = useMemo(() => (selectedId != null ? chainOf(selectedId, tasks) : null), [selectedId, tasks])

  // What's shown: the focused chain, or everything that passes the filters.
  const visible = useMemo(() => {
    if (focusChain && selectedId != null && chain) return new Set([selectedId, ...chain.up, ...chain.down])
    return new Set(tasks.filter(t =>
      (epicFilter == null || t.epic_id === epicFilter) &&
      (!hideDone || t.state !== 'completed' || t.id === selectedId) &&
      (!mineOnly || meId == null || t.owner_user_id === meId),
    ).map(t => t.id))
  }, [tasks, focusChain, selectedId, chain, epicFilter, hideDone, mineOnly, meId])

  const effectiveExpanded = useMemo(() => {
    if (!focusChain) return expanded
    return new Set([...visible].map(id => taskById.get(id)?.story_id).filter((x): x is number => x != null))
  }, [focusChain, expanded, visible, taskById])

  const model = useMemo(
    () => buildGraph({ epics, stories, tasks, visible, expanded: effectiveExpanded }),
    [epics, stories, tasks, visible, effectiveExpanded],
  )

  const openTicket = useCallback((t: Task) => setDrawerTask(t), [])
  const expandStory = useCallback((id: number) => setExpanded(prev => {
    const s = new Set(prev ?? (tasks.length <= EXPAND_ALL_UP_TO ? stories.map(x => x.id) : []))
    s.add(id)
    return s
  }), [tasks.length, stories])
  const collapseStory = useCallback((id: number) => setExpanded(prev => {
    const s = new Set(prev ?? (tasks.length <= EXPAND_ALL_UP_TO ? stories.map(x => x.id) : []))
    s.delete(id)
    return s
  }), [tasks.length, stories])

  const markOf = useCallback((id: number): Mark => {
    if (selectedId == null || !chain) return null
    if (id === selectedId) return 'selected'
    if (chain.up.has(id)) return 'up'
    if (chain.down.has(id)) return 'down'
    return 'dim'
  }, [selectedId, chain])

  const nodes: Node[] = useMemo(() => [
    ...model.clusters.map(c => ({
      id: c.id, type: 'cluster', position: { x: c.x, y: c.y }, zIndex: -1, selectable: false, draggable: false,
      width: c.width, height: c.height,
      data: { story: c.story, epic: c.epic, epicColor: epicColor(c.story.epic_id), counts: countStates(c.tasks), onCollapse: collapseStory } satisfies ClusterData,
    })),
    ...model.stories.map(s => ({
      id: s.id, type: 'story', position: { x: s.x, y: s.y }, draggable: false, width: STORY_W, height: STORY_H,
      data: {
        story: s.story, epic: s.epic, epicColor: epicColor(s.story.epic_id), tasks: s.tasks, counts: s.counts,
        dim: selectedId != null && !s.tasks.some(t => markOf(t.id) !== 'dim'),
        changed: s.tasks.filter(t => changed.has(t.id)).length, onExpand: expandStory,
      } satisfies StoryData,
    })),
    ...model.tickets.map(t => ({
      id: t.id, type: 'ticket', position: { x: t.x, y: t.y }, draggable: false, width: TICKET_W, height: TICKET_H,
      data: { task: t.task, epicColor: epicColor(t.task.epic_id), mark: markOf(t.task.id), changed: changed.has(t.task.id), onOpen: openTicket } satisfies TicketData,
    })),
  ], [model, epicColor, selectedId, markOf, changed, expandStory, collapseStory, openTicket])

  const edges: Edge[] = useMemo(() => model.edges.map(e => {
    const lit = selectedId != null && e.from != null && e.to != null && markOf(e.from) !== 'dim' && markOf(e.to) !== 'dim'
    const dim = selectedId != null && !lit
    const stroke = e.blocked ? 'var(--danger)' : lit ? 'var(--ink)' : e.crossEpic ? 'var(--violet)' : 'var(--ink-3)'
    return {
      id: e.id, source: e.source, target: e.target,
      label: e.count > 1 ? String(e.count) : undefined,
      labelStyle: { fill: 'var(--ink-2)', fontSize: 10 }, labelBgStyle: { fill: 'var(--surface)' },
      style: { stroke, strokeWidth: lit ? 2 : 1.4, opacity: dim ? 0.15 : 1, strokeDasharray: e.crossEpic ? '6 3' : undefined },
      animated: e.to != null && taskById.get(e.to)?.state === 'in_progress',
    }
  }), [model, selectedId, markOf, taskById])

  // Frame everything when what's shown changes, or the ticket just jumped
  // to. Every node has a fixed size (below), so this needs no measuring —
  // just a beat for React Flow to take in the new nodes.
  const shape = `${[...visible].length}|${[...effectiveExpanded].sort().join(',')}|${focusChain}`
  const frameTicket = useRef<number | null>(null)
  const [jumps, setJumps] = useState(0)
  useEffect(() => {
    if (!nodes.length) return
    const t = setTimeout(() => {
      const id = frameTicket.current
      frameTicket.current = null
      if (id != null) flow.fitView({ nodes: [{ id: ticketNodeId(id) }], duration: 400, maxZoom: 1.1, padding: 0.4 })
      else flow.fitView({ padding: 0.12, duration: 300, maxZoom: 1.2 })
    }, 50)
    return () => clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shape, jumps])

  const jumpTo = (t: Task) => {
    setSelectedId(t.id)
    setQuery('')
    if (!expanded.has(t.story_id)) expandStory(t.story_id)
    frameTicket.current = t.id
    setJumps(n => n + 1)
  }

  const results = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return []
    return tasks.filter(t => t.name.toLowerCase().includes(q) || `#${t.id}` === q || String(t.id) === q).slice(0, 8)
  }, [query, tasks])

  const selected = selectedId != null ? taskById.get(selectedId) : undefined
  const recent = useMemo(() => {
    const latest = new Map<number, ActivityLog>()
    for (const a of activity) {
      const cur = latest.get(a.task_id)
      if (!cur || cur.created_at < a.created_at) latest.set(a.task_id, a)
    }
    return [...latest.values()].sort((a, b) => b.created_at.localeCompare(a.created_at))
  }, [activity])

  const pill = (on: boolean) =>
    `flex items-center gap-1.5 px-2.5 py-1 rounded-lg text-xs font-semibold transition-all cursor-pointer ${on ? 'text-ink bg-surface-3 border border-line-str' : 'text-ink-2 hover:text-ink border border-transparent'}`

  return (
    <div className="flex flex-col h-full" style={{ background: 'var(--bg)' }}>
      <ProjectNav projectId={pid} currentView="dag" />

      {/* Toolbar */}
      <div className="flex items-center gap-2 px-3 py-2 flex-wrap flex-shrink-0" style={{ borderBottom: '1px solid var(--line)', background: 'var(--surface-2)' }}>
        <div className="relative">
          <Search size={12} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-3" />
          <input value={query} onChange={e => setQuery(e.target.value)} placeholder="Find a ticket…"
            onKeyDown={e => { if (e.key === 'Enter' && results[0]) jumpTo(results[0]) }}
            className="w-56 bg-bg border border-line rounded-lg pl-7 pr-2 py-1 text-xs text-ink placeholder-ink-3 focus:outline-none focus:border-primary" />
          {results.length > 0 && (
            <div className="absolute z-20 mt-1 w-80 rounded-xl overflow-hidden shadow-2xl" style={{ background: 'var(--surface)', border: '1px solid var(--line-str)' }}>
              {results.map(t => (
                <button key={t.id} onClick={() => jumpTo(t)}
                  className="w-full text-left px-3 py-2 text-xs hover:bg-surface-3 flex items-center gap-2 cursor-pointer">
                  <span className="font-mono text-ink-2">#{t.id}</span>
                  <span className="text-ink truncate flex-1">{t.name}</span>
                  <span style={{ color: sc(t.state).text }}>{sc(t.state).label}</span>
                </button>
              ))}
            </div>
          )}
        </div>

        <select value={epicFilter ?? ''} onChange={e => setEpicFilter(e.target.value ? Number(e.target.value) : null)}
          className="bg-bg border border-line rounded-lg px-2 py-1 text-xs text-ink focus:outline-none cursor-pointer">
          <option value="">All epics</option>
          {epics.map(e => <option key={e.id} value={e.id}>{e.name}</option>)}
        </select>
        {meId != null && (
          <button onClick={() => setMineOnly(v => !v)} className={pill(mineOnly)} title="Only tickets you own">
            <User size={12} />Mine
          </button>
        )}
        <button onClick={() => setHideDone(v => !v)} className={pill(hideDone)}>
          {hideDone ? <EyeOff size={12} /> : <Eye size={12} />}Hide done
        </button>

        <div className="w-px h-5 bg-line mx-1" />
        <button onClick={() => setExpanded(new Set(stories.map(s => s.id)))} className={pill(false)} title="Show every ticket">
          <Layers size={12} />Expand all
        </button>
        <button onClick={() => setExpanded(new Set())} className={pill(false)} title="One node per story">
          Collapse all
        </button>

        <div className="w-px h-5 bg-line mx-1" />
        <span className="text-xs text-ink-3">What changed:</span>
        {(['off', '1d', '7d'] as const).map(c => (
          <button key={c} onClick={() => setChanges(c)} className={pill(changes === c)}>
            {c === 'off' ? 'Off' : c === '1d' ? 'Today' : 'This week'}
          </button>
        ))}

        <div className="ml-auto flex items-center gap-3 text-[10px] text-ink-3">
          <span className="flex items-center gap-1"><span className="w-3 h-0.5 bg-link" />waits on</span>
          <span className="flex items-center gap-1"><span className="w-3 h-0.5 bg-violet" />waited on by</span>
          <span className="flex items-center gap-1"><span className="w-3 h-0.5 bg-danger" />blocking</span>
        </div>
      </div>

      <div className="flex flex-1 overflow-hidden">
        <div className="flex-1 relative">
          {loading && tasks.length === 0 ? (
            <div className="flex items-center justify-center h-full"><Loader2 size={20} className="text-primary animate-spin" /></div>
          ) : error ? (
            <div className="flex items-center justify-center h-full text-sm text-danger-ink">{error}</div>
          ) : tasks.length === 0 ? (
            <div className="flex items-center justify-center h-full text-sm text-ink-3">No tickets yet.</div>
          ) : (
            <ReactFlow
              nodes={nodes} edges={edges} nodeTypes={nodeTypes}
              colorMode="dark" minZoom={0.05} maxZoom={2} fitView
              nodesDraggable={false} nodesConnectable={false} elementsSelectable={false}
              onNodeClick={(_, n) => {
                if (n.type === 'ticket') {
                  const id = Number(n.id.slice(1))
                  setSelectedId(cur => (cur === id ? null : id))
                }
              }}
              onPaneClick={() => { if (!focusChain) setSelectedId(null) }}
              proOptions={{ hideAttribution: true }}
              style={{ background: 'var(--bg)' }}
            >
              <Background color="var(--surface-3)" bgColor="var(--bg)" gap={22} />
              <Controls showInteractive={false} />
              <MiniMap pannable zoomable bgColor="var(--surface)"
                style={{ border: '1px solid var(--line)' }} maskColor="color-mix(in srgb, var(--ink) 8%, transparent)"
                nodeColor={n => n.type === 'ticket' ? sc((n.data as TicketData).task.state).bar : n.type === 'story' ? 'var(--ink-3)' : 'transparent'}
                nodeStrokeColor={n => n.type === 'cluster' ? 'var(--line-str)' : 'transparent'} />
            </ReactFlow>
          )}

          {/* Selected ticket */}
          {selected && chain && (
            <div className="absolute left-1/2 -translate-x-1/2 bottom-4 flex items-center gap-3 px-4 py-2.5 rounded-xl shadow-2xl"
              style={{ background: 'var(--surface)', border: '1px solid var(--line-str)' }}>
              <span className="font-mono text-xs text-ink-2">#{selected.id}</span>
              <span className="text-sm text-ink font-semibold max-w-[320px] truncate">{selected.name}</span>
              <span className="text-xs" style={{ color: sc(selected.state).text }}>{sc(selected.state).label}</span>
              <span className="text-xs text-link">waits on {chain.up.size}</span>
              <span className="text-xs text-violet">{chain.down.size} wait on it</span>
              <button onClick={() => setFocusChain(v => !v)} className={pill(focusChain)} title="Show only this chain">
                <Crosshair size={12} />Focus chain
              </button>
              <button onClick={() => openTicket(selected)} className={pill(false)}>
                <ExternalLink size={12} />Open
              </button>
              <button onClick={() => { setSelectedId(null); setFocusChain(false) }} className="text-ink-2 hover:text-ink cursor-pointer" title="Clear">
                <X size={14} />
              </button>
            </div>
          )}
        </div>

        {/* What changed */}
        {changes !== 'off' && (
          <aside className="w-72 flex-shrink-0 overflow-y-auto" style={{ borderLeft: '1px solid var(--line)', background: 'var(--surface)' }}>
            <div className="px-4 py-3 text-xs font-semibold text-ink flex items-center gap-1.5" style={{ borderBottom: '1px solid var(--line-sub)' }}>
              <Clock size={12} style={{ color: CHANGED }} />
              {recent.length} ticket{recent.length === 1 ? '' : 's'} changed {changes === '1d' ? 'today' : 'this week'}
            </div>
            {recent.map(a => {
              const t = taskById.get(a.task_id)
              if (!t) return null
              return (
                <button key={a.task_id} onClick={() => jumpTo(t)}
                  className="w-full text-left px-4 py-2.5 hover:bg-surface-3 cursor-pointer" style={{ borderBottom: '1px solid var(--surface-2)' }}>
                  <div className="flex items-center gap-1.5 text-[10px]">
                    <span className="font-mono text-ink-2">#{t.id}</span>
                    <span className="text-ink-2">{a.action.replace('_', ' ')}{a.to_state ? ` → ${a.to_state.replace('_', ' ')}` : ''}</span>
                    <span className="ml-auto text-ink-3">{new Date(a.created_at).toLocaleString(undefined, { weekday: changes === '7d' ? 'short' : undefined, hour: 'numeric', minute: '2-digit' })}</span>
                  </div>
                  <div className="text-xs text-ink truncate mt-0.5">{t.name}</div>
                </button>
              )
            })}
            {recent.length === 0 && <p className="px-4 py-6 text-xs text-ink-3 text-center">Nothing changed.</p>}
          </aside>
        )}
      </div>

      <TaskDrawer task={drawerTask} onClose={() => setDrawerTask(null)} onRefresh={refresh} />
    </div>
  )
}
