/**
 * DAG View — Interactive dependency graph with:
 *   - Draggable nodes
 *   - Canvas pan (drag empty space)
 *   - Wheel zoom
 *   - Click-to-select with persistent full chain highlight
 *   - Bottom info strip for selected task
 *   - Hide-completed toggle
 */
import { useMemo, useState, useCallback, useRef, useEffect } from 'react'
import { useParams } from 'react-router-dom'
import {
  Loader2, ZoomIn, ZoomOut, Maximize2, GitBranch,
  EyeOff, Eye, X, ExternalLink, ArrowRight,
} from 'lucide-react'
import * as dagre from '@dagrejs/dagre'
import { useBoardData } from '../hooks/useBoardData'
import { TaskDrawer } from '../components/TaskDrawer'
import { StatusBadge } from '../components/StatusBadge'
import { ProjectNav } from '../components/ProjectNav'
import type { Task } from '../types'

// ─── Dimensions ───────────────────────────────────────────────────────────────
const NODE_W   = 224
const NODE_H   = 96
const RANK_SEP = 140
const NODE_SEP = 20

// ─── Colour system ────────────────────────────────────────────────────────────
const STATE_C = {
  backlog:     { border: '#1E2C45', bg: '#08101F',  text: '#7E91A8', bar: '#283A57' },
  in_progress: { border: '#4F46E5', bg: '#0A0B22',  text: '#818CF8', bar: '#6366F1' },
  completed:   { border: '#065F46', bg: '#021A0F',  text: '#10B981', bar: '#059669' },
  blocked:     { border: '#991B1B', bg: '#180404',  text: '#F87171', bar: '#EF4444' },
} as const
type SK = keyof typeof STATE_C
const sc = (s: string) => STATE_C[s as SK] ?? STATE_C.backlog

const EPIC_PALETTE = [
  '#6366F1','#10B981','#F59E0B','#EF4444','#A78BFA','#2DD4BF','#FB923C','#34D399',
]
const epicColor = (i: number) => EPIC_PALETTE[i % EPIC_PALETTE.length]

// ─── Layout ───────────────────────────────────────────────────────────────────
interface LayoutResult {
  nodes: Map<number, { x: number; y: number; task: Task }>
  edges: Array<{
    from: number; to: number
    points: Array<{ x: number; y: number }>
    kind: 'same-story' | 'cross-story' | 'cross-epic'
    blocked: boolean
  }>
  width: number; height: number
}

function runLayout(tasks: Task[]): LayoutResult {
  const g = new dagre.graphlib.Graph()
  g.setGraph({ rankdir:'LR', ranksep:RANK_SEP, nodesep:NODE_SEP, marginx:56, marginy:56, acyclicer:'greedy', ranker:'network-simplex' })
  g.setDefaultEdgeLabel(() => ({}))
  for (const t of tasks) g.setNode(String(t.id), { width:NODE_W, height:NODE_H })
  for (const t of tasks) for (const d of t.dependency_ids ?? []) if (g.hasNode(String(d))) g.setEdge(String(d), String(t.id))
  dagre.layout(g)

  const byId = new Map(tasks.map(t => [t.id, t]))
  const nodes = new Map<number, { x:number; y:number; task:Task }>()
  for (const id of g.nodes()) {
    const n = g.node(id); const t = byId.get(Number(id))
    if (t && n) nodes.set(t.id, { x: n.x, y: n.y, task: t })
  }
  const edges: LayoutResult['edges'] = []
  for (const e of g.edges()) {
    const ed = g.edge(e), from = byId.get(Number(e.v)), to = byId.get(Number(e.w))
    if (!from || !to) continue
    edges.push({
      from: from.id, to: to.id, points: ed?.points ?? [],
      kind: from.epic_id !== to.epic_id ? 'cross-epic' : from.story_id !== to.story_id ? 'cross-story' : 'same-story',
      blocked: to.state === 'blocked' && (to.blocked_by_ids ?? []).includes(from.id),
    })
  }
  const gr = g.graph()
  return { nodes, edges, width:(gr.width??800)+112, height:(gr.height??600)+112 }
}

function edgePath(
  from: {x:number; y:number},
  to:   {x:number; y:number},
  points: Array<{x:number; y:number}>,
): string {
  const all = [from, ...points, to]
  if (all.length < 2) return ''
  let d = `M ${all[0].x} ${all[0].y}`
  for (let i = 1; i < all.length; i++) {
    const p0=all[Math.max(0,i-2)], p1=all[i-1], p2=all[i], p3=all[Math.min(all.length-1,i+1)]
    const cp1x=p1.x+(p2.x-p0.x)/6, cp1y=p1.y+(p2.y-p0.y)/6
    const cp2x=p2.x-(p3.x-p1.x)/6, cp2y=p2.y-(p3.y-p1.y)/6
    d += ` C ${cp1x} ${cp1y}, ${cp2x} ${cp2y}, ${p2.x} ${p2.y}`
  }
  return d
}

// Resolve a node's effective position (dagre base + any user drag override)
function nodePos(
  id: number,
  layoutNodes: LayoutResult['nodes'],
  overrides: Map<number, {x:number; y:number}>,
): {x:number; y:number} {
  const ov = overrides.get(id)
  const base = layoutNodes.get(id)
  if (!base) return { x:0, y:0 }
  return ov ?? { x: base.x, y: base.y }
}

// ─── Component ────────────────────────────────────────────────────────────────
export function DAGView() {
  const { projectId } = useParams<{ projectId:string }>()
  const pid = Number(projectId)
  const { epics, stories, tasks, loading, error } = useBoardData(pid)

  // ── filters & display state ────────────────────────────────────────────────
  const [filterEpicId,  setFilterEpicId]  = useState<number|null>(null)
  const [hideCompleted, setHideCompleted] = useState(false)
  const [zoom,          setZoom]          = useState(1)
  const [pan,           setPan]           = useState({ x: 0, y: 0 })

  // ── hover / select ─────────────────────────────────────────────────────────
  const [hoveredId,  setHoveredId]  = useState<number|null>(null)
  const [selectedId, setSelectedId] = useState<number|null>(null)
  const [drawerTask, setDrawerTask] = useState<Task|null>(null)

  // ── drag state ─────────────────────────────────────────────────────────────
  // nodeOverrides: user-repositioned nodes
  const [nodeOverrides, setNodeOverrides] = useState<Map<number, {x:number; y:number}>>(new Map())
  const draggingNode = useRef<{id:number; startMouseX:number; startMouseY:number; startNodeX:number; startNodeY:number}|null>(null)
  const panningRef   = useRef<{startMouseX:number; startMouseY:number; startPanX:number; startPanY:number}|null>(null)
  const dragMoved    = useRef(false)

  const containerRef = useRef<HTMLDivElement>(null)

  // ── derived maps ───────────────────────────────────────────────────────────
  const epicIdx   = useMemo(() => new Map(epics.map((e,i)=>[e.id,i])),  [epics])
  const epicById  = useMemo(() => new Map(epics.map(e=>[e.id,e])),       [epics])
  const storyById = useMemo(() => new Map(stories.map(s=>[s.id,s])),     [stories])
  const taskById  = useMemo(() => new Map(tasks.map(t=>[t.id,t])),       [tasks])

  const reverseDeps = useMemo(() => {
    const m = new Map<number,number[]>()
    tasks.forEach(t => (t.dependency_ids??[]).forEach(d => {
      if (!m.has(d)) m.set(d,[])
      m.get(d)!.push(t.id)
    }))
    return m
  }, [tasks])

  const visibleTasks = useMemo(() => {
    let r = filterEpicId ? tasks.filter(t=>t.epic_id===filterEpicId) : [...tasks]
    if (hideCompleted) r = r.filter(t=>t.state!=='completed')
    return r
  }, [tasks, filterEpicId, hideCompleted])

  const completedCount = useMemo(() => tasks.filter(t=>t.state==='completed').length, [tasks])
  const layout         = useMemo(() => runLayout(visibleTasks), [visibleTasks])

  // Reset overrides & pan when layout changes significantly (task list changed)
  const prevTaskIds = useRef<string>('')
  useEffect(() => {
    const ids = visibleTasks.map(t=>t.id).sort().join(',')
    if (ids !== prevTaskIds.current) {
      prevTaskIds.current = ids
      setNodeOverrides(new Map())
      // centre the graph
      if (containerRef.current) {
        const cw = containerRef.current.clientWidth
        const ch = containerRef.current.clientHeight
        setPan({ x: Math.max(0, (cw - layout.width)  / 2),
                 y: Math.max(0, (ch - layout.height) / 2) })
      }
    }
  }, [visibleTasks, layout])

  // ── chain computation for hover and select ─────────────────────────────────
  const computeChain = useCallback((focusId: number) => {
    const deps = new Set<number>(), dependents = new Set<number>()
    function up(id:number, vis=new Set<number>()) {
      if (vis.has(id)) return; vis.add(id)
      taskById.get(id)?.dependency_ids?.forEach(d => { deps.add(d); up(d, vis) })
    }
    function down(id:number, vis=new Set<number>()) {
      if (vis.has(id)) return; vis.add(id)
      reverseDeps.get(id)?.forEach(c => { dependents.add(c); down(c, vis) })
    }
    up(focusId); down(focusId)
    return { deps, dependents }
  }, [taskById, reverseDeps])

  const selectedChain = useMemo(() => selectedId !== null ? computeChain(selectedId) : null, [selectedId, computeChain])
  const hoveredChain  = useMemo(() => hoveredId  !== null && hoveredId !== selectedId ? computeChain(hoveredId)  : null, [hoveredId, selectedId, computeChain])

  // ── zoom helpers ───────────────────────────────────────────────────────────
  const zoomIn    = useCallback(() => setZoom(z=>Math.min(z+0.15,3)),    [])
  const zoomOut   = useCallback(() => setZoom(z=>Math.max(z-0.15,0.2)),  [])
  const zoomReset = useCallback(() => {
    setZoom(1)
    if (containerRef.current) {
      const cw = containerRef.current.clientWidth
      const ch = containerRef.current.clientHeight
      setPan({ x: Math.max(0,(cw-layout.width)/2), y: Math.max(0,(ch-layout.height)/2) })
    }
  }, [layout])

  const fitToView = useCallback(() => {
    if (!containerRef.current) return
    const cw = containerRef.current.clientWidth
    const ch = containerRef.current.clientHeight
    const scaleX = cw  / layout.width
    const scaleY = ch  / layout.height
    const newZoom = Math.min(scaleX, scaleY, 1) * 0.9
    setZoom(newZoom)
    setPan({ x:(cw - layout.width  * newZoom)/2, y:(ch - layout.height * newZoom)/2 })
  }, [layout])

  // ── wheel zoom ─────────────────────────────────────────────────────────────
  const handleWheel = useCallback((e: React.WheelEvent) => {
    e.preventDefault()
    const delta = -e.deltaY * 0.001
    setZoom(z => Math.min(3, Math.max(0.2, z + delta * z)))
  }, [])

  // ── pointer events ─────────────────────────────────────────────────────────
  const handleNodeMouseDown = useCallback((e: React.MouseEvent, id: number) => {
    e.stopPropagation()
    dragMoved.current = false
    const pos = nodePos(id, layout.nodes, nodeOverrides)
    draggingNode.current = {
      id,
      startMouseX: e.clientX,
      startMouseY: e.clientY,
      startNodeX:  pos.x,
      startNodeY:  pos.y,
    }
  }, [layout.nodes, nodeOverrides])

  const handleCanvasMouseDown = useCallback((e: React.MouseEvent) => {
    // Only pan on primary button, not on a node
    if (e.button !== 0) return
    dragMoved.current = false
    panningRef.current = {
      startMouseX: e.clientX,
      startMouseY: e.clientY,
      startPanX:   pan.x,
      startPanY:   pan.y,
    }
  }, [pan])

  const handleMouseMove = useCallback((e: React.MouseEvent) => {
    if (draggingNode.current) {
      const dx = (e.clientX - draggingNode.current.startMouseX) / zoom
      const dy = (e.clientY - draggingNode.current.startMouseY) / zoom
      if (Math.abs(dx) > 3 || Math.abs(dy) > 3) dragMoved.current = true
      const id = draggingNode.current.id
      setNodeOverrides(prev => {
        const next = new Map(prev)
        next.set(id, {
          x: draggingNode.current!.startNodeX + dx,
          y: draggingNode.current!.startNodeY + dy,
        })
        return next
      })
    } else if (panningRef.current) {
      const dx = e.clientX - panningRef.current.startMouseX
      const dy = e.clientY - panningRef.current.startMouseY
      if (Math.abs(dx) > 3 || Math.abs(dy) > 3) dragMoved.current = true
      setPan({
        x: panningRef.current.startPanX + dx,
        y: panningRef.current.startPanY + dy,
      })
    }
  }, [zoom])

  const handleMouseUp = useCallback(() => {
    draggingNode.current = null
    panningRef.current   = null
  }, [])

  const handleNodeClick = useCallback((e: React.MouseEvent, task: Task) => {
    e.stopPropagation()
    if (dragMoved.current) return // was a drag, not a click
    setSelectedId(prev => prev === task.id ? null : task.id)
  }, [])

  const handleCanvasClick = useCallback(() => {
    if (!dragMoved.current) setSelectedId(null)
  }, [])

  // ─── Early returns (ALL hooks above this line) ─────────────────────────────
  if (loading) return (
    <div className="flex items-center justify-center h-screen" style={{ background: '#08101F' }}>
      <Loader2 size={22} className="text-[#6366F1] animate-spin"/>
    </div>
  )
  if (error) return (
    <div className="flex items-center justify-center h-screen text-[#F87171] text-sm" style={{ background: '#08101F' }}>
      {error}
    </div>
  )

  // ── Edge classification ────────────────────────────────────────────────────
  const focusId    = selectedId ?? hoveredId
  const focusChain = selectedChain ?? hoveredChain
  const { nodes, edges } = layout
  const bgEdges: typeof edges = [], hlEdges: typeof edges = []
  for (const edge of edges) {
    const onChain = focusChain ? (
      (focusChain.deps.has(edge.from)       && focusChain.deps.has(edge.to))        ||
      (focusChain.deps.has(edge.from)       && edge.to   === focusId)               ||
      (edge.from === focusId               && focusChain.dependents.has(edge.to))   ||
      (focusChain.dependents.has(edge.from) && focusChain.dependents.has(edge.to))  ||
      (focusChain.deps.has(edge.from)       && focusChain.dependents.has(edge.to))
    ) : false
    if (focusChain) { onChain ? hlEdges.push(edge) : bgEdges.push(edge) }
    else bgEdges.push(edge)
  }

  const selectedTaskObj = selectedId !== null ? taskById.get(selectedId) : null
  const isDragging = draggingNode.current !== null || panningRef.current !== null

  return (
    <div className="flex flex-col h-screen overflow-hidden" style={{ background: '#08101F' }}>
      {/* ── Nav bar ── */}
      <ProjectNav
        projectId={pid}
        currentView="dag"
        rightSlot={
          <>
            {/* Legend */}
            <div className="hidden xl:flex items-center gap-4 text-xs text-[#7E91A8]">
              <LegendLine stroke="#283A57"  label="Same story"/>
              <LegendLine stroke="#6366F1"  dash="6 3" label="Cross story"/>
              <LegendLine stroke="#A78BFA"  dash="9 4" label="Cross epic"/>
              <LegendLine stroke="#EF4444"  dash="4 3" label="Blocking"/>
            </div>

            {/* Hide completed */}
            {completedCount > 0 && (
              <button
                onClick={() => setHideCompleted(h=>!h)}
                className="flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg text-xs font-semibold transition-all cursor-pointer"
                style={hideCompleted
                  ? { background:'rgba(16,185,129,0.1)', border:'1px solid rgba(16,185,129,0.25)', color:'#10B981' }
                  : { background:'transparent', border:'1px solid #1E2C45', color:'#3D5068' }}
              >
                {hideCompleted ? <Eye size={12}/> : <EyeOff size={12}/>}
                <span>{hideCompleted ? `${completedCount} hidden` : 'Hide done'}</span>
              </button>
            )}

            {/* Zoom controls */}
            <div className="flex items-center gap-0.5 rounded-lg p-1"
              style={{ background:'#08101F', border:'1px solid #1E2C45' }}>
              <button onClick={zoomOut}   className="p-1.5 rounded-md text-[#3D5068] hover:text-[#DDE6F0] hover:bg-[#172540] transition-colors cursor-pointer"><ZoomOut   size={12}/></button>
              <button onClick={zoomReset} className="px-2 py-1 rounded-md text-xs text-[#3D5068] hover:text-[#DDE6F0] hover:bg-[#172540] transition-colors font-mono tabular-nums cursor-pointer min-w-[44px] text-center">{Math.round(zoom*100)}%</button>
              <button onClick={zoomIn}    className="p-1.5 rounded-md text-[#3D5068] hover:text-[#DDE6F0] hover:bg-[#172540] transition-colors cursor-pointer"><ZoomIn    size={12}/></button>
              <div className="w-px h-4 bg-[#192238] mx-0.5"/>
              <button onClick={fitToView} className="p-1.5 rounded-md text-[#3D5068] hover:text-[#DDE6F0] hover:bg-[#172540] transition-colors cursor-pointer" title="Fit to view"><Maximize2 size={12}/></button>
            </div>

            {/* Epic filter */}
            {epics.length > 1 && (
              <select
                value={filterEpicId??''}
                onChange={e=>setFilterEpicId(e.target.value?Number(e.target.value):null)}
                className="rounded-lg px-3 py-1.5 text-xs text-[#DDE6F0] focus:outline-none appearance-none cursor-pointer"
                style={{ background:'#08101F', border:'1px solid #1E2C45', fontFamily:'inherit' }}
                onFocus={e=>(e.target.style.borderColor='#6366F1')}
                onBlur={e=>(e.target.style.borderColor='#1E2C45')}
              >
                <option value="">All epics</option>
                {epics.map(e=><option key={e.id} value={e.id}>{e.name}</option>)}
              </select>
            )}
          </>
        }
      />

      {/* ── Epic key bar ── */}
      {epics.length > 0 && (
        <div className="flex items-center gap-2 px-5 py-2 flex-shrink-0 overflow-x-auto"
          style={{ borderBottom:'1px solid #192238', background:'#08101F' }}>
          <span className="text-[10px] font-bold text-[#3D5068] uppercase tracking-widest flex-shrink-0">Epics</span>
          <div className="w-px h-4 bg-[#192238]"/>
          {epics.map((e,i)=>{
            const col=epicColor(i), isActive=filterEpicId===e.id
            return (
              <button key={e.id}
                onClick={()=>setFilterEpicId(filterEpicId===e.id?null:e.id)}
                className="flex items-center gap-1.5 px-2.5 py-1 rounded-lg text-xs flex-shrink-0 transition-all cursor-pointer"
                style={{ opacity:filterEpicId&&!isActive?0.35:1, background:isActive?`${col}18`:'transparent', border:isActive?`1px solid ${col}40`:'1px solid transparent', color:col }}>
                <span className="w-2 h-2 rounded-full" style={{background:col}}/>
                {e.name}
              </button>
            )
          })}
        </div>
      )}

      {/* ── Canvas ── */}
      <div
        ref={containerRef}
        className="flex-1 relative overflow-hidden select-none"
        style={{ background:'#08101F', cursor: isDragging ? 'grabbing' : panningRef.current ? 'grab' : 'default' }}
        onMouseDown={handleCanvasMouseDown}
        onMouseMove={handleMouseMove}
        onMouseUp={handleMouseUp}
        onMouseLeave={handleMouseUp}
        onClick={handleCanvasClick}
        onWheel={handleWheel}
      >
        {visibleTasks.length === 0 ? (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-4">
            <div className="w-14 h-14 rounded-2xl flex items-center justify-center"
              style={{ background:'#0D1726', border:'1px solid #1E2C45' }}>
              <GitBranch size={22} className="text-[#283A57]"/>
            </div>
            <p className="text-sm text-[#7E91A8]">No tasks to display</p>
          </div>
        ) : (
          <svg
            width={layout.width * zoom}
            height={layout.height * zoom}
            viewBox={`0 0 ${layout.width} ${layout.height}`}
            style={{ display:'block', transform:`translate(${pan.x}px,${pan.y}px)`, transformOrigin:'0 0', cursor:'inherit' }}
          >
            <defs>
              {([
                ['arr-grey',   '#283A57'],
                ['arr-indigo', '#6366F1'],
                ['arr-purple', '#A78BFA'],
                ['arr-red',    '#EF4444'],
                ['arr-gold',   '#FBBF24'],
                ['arr-lit',    '#818CF8'],
              ] as const).map(([id,fill])=>(
                <marker key={id} id={id} markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto">
                  <path d="M0,0 L0,8 L8,4 z" fill={fill} opacity="0.9"/>
                </marker>
              ))}
              {/* Selected node glow */}
              <filter id="glow-sel" x="-40%" y="-40%" width="180%" height="180%">
                <feGaussianBlur in="SourceAlpha" stdDeviation="7" result="b"/>
                <feFlood floodColor="#FBBF24" floodOpacity="0.45" result="c"/>
                <feComposite in="c" in2="b" operator="in" result="g"/>
                <feMerge><feMergeNode in="g"/><feMergeNode in="SourceGraphic"/></feMerge>
              </filter>
              {/* Dep chain glow */}
              <filter id="glow-dep" x="-30%" y="-30%" width="160%" height="160%">
                <feGaussianBlur in="SourceAlpha" stdDeviation="4" result="b"/>
                <feFlood floodColor="#6366F1" floodOpacity="0.4" result="c"/>
                <feComposite in="c" in2="b" operator="in" result="g"/>
                <feMerge><feMergeNode in="g"/><feMergeNode in="SourceGraphic"/></feMerge>
              </filter>
              <filter id="glow-dep2" x="-30%" y="-30%" width="160%" height="160%">
                <feGaussianBlur in="SourceAlpha" stdDeviation="4" result="b"/>
                <feFlood floodColor="#A78BFA" floodOpacity="0.4" result="c"/>
                <feComposite in="c" in2="b" operator="in" result="g"/>
                <feMerge><feMergeNode in="g"/><feMergeNode in="SourceGraphic"/></feMerge>
              </filter>
            </defs>

            {/* ── Layer 1: dimmed edges ── */}
            {bgEdges.map(edge => {
              const fn = nodePos(edge.from, nodes, nodeOverrides)
              const tn = nodePos(edge.to,   nodes, nodeOverrides)
              const st = EDGE_STYLE[edge.kind]
              const stroke  = edge.blocked ? '#EF4444' : st.stroke
              const opacity = focusChain   ? 0.04      : edge.blocked ? 0.8 : 0.35
              const marker  = edge.blocked ? 'arr-red' : edge.kind==='cross-epic' ? 'arr-purple' : edge.kind==='cross-story' ? 'arr-indigo' : 'arr-grey'
              return (
                <path key={`bg-${edge.from}-${edge.to}`}
                  d={edgePath(fn, tn, edge.points)} fill="none"
                  stroke={stroke} strokeWidth={st.width}
                  strokeDasharray={edge.blocked?'4 3':st.dash}
                  strokeOpacity={opacity} markerEnd={`url(#${marker})`}/>
              )
            })}

            {/* ── Layer 2: nodes ── */}
            {Array.from(nodes.entries()).map(([id, {task}]) => {
              const {x,y} = nodePos(id, nodes, nodeOverrides)
              const colors    = sc(task.state)
              const eIdx      = epicIdx.get(task.epic_id) ?? 0
              const eColor    = epicColor(eIdx)
              const L = x - NODE_W/2, T = y - NODE_H/2

              const isSelected   = selectedId === task.id
              const isDep        = focusChain?.deps.has(task.id)
              const isDependent  = focusChain?.dependents.has(task.id)
              const isFocusNode  = focusId === task.id
              const isDimmed     = !!focusChain && !isFocusNode && !isDep && !isDependent

              const borderColor = isSelected  ? '#FBBF24'
                                : isDep       ? '#818CF8'
                                : isDependent ? '#A78BFA'
                                : isFocusNode ? '#FBBF24'
                                : colors.border
              const borderW = (isSelected || isDep || isDependent || isFocusNode) ? 2 : 1
              const filter  = isSelected    ? 'url(#glow-sel)'
                            : isDep         ? 'url(#glow-dep)'
                            : isDependent   ? 'url(#glow-dep2)'
                            : undefined

              return (
                <g key={id}
                  opacity={isDimmed ? 0.2 : 1}
                  style={{ cursor: 'pointer' }}
                  onMouseDown={e => handleNodeMouseDown(e, id)}
                  onMouseEnter={() => setHoveredId(id)}
                  onMouseLeave={() => setHoveredId(null)}
                  onClick={e => handleNodeClick(e, task)}
                  filter={filter}
                >
                  {/* Card */}
                  <rect x={L} y={T} width={NODE_W} height={NODE_H} rx={10} fill={colors.bg} stroke={borderColor} strokeWidth={borderW}/>
                  {/* Epic top strip */}
                  <rect x={L+1} y={T+1} width={NODE_W-2} height={3} rx={2} fill={eColor} opacity={0.85}/>
                  {/* State bar */}
                  <rect x={L} y={T+4} width={4} height={NODE_H-8} rx={2} fill={colors.bar}/>
                  {/* Task name */}
                  <foreignObject x={L+14} y={T+10} width={NODE_W-22} height={44}>
                    <div style={{
                      fontSize: 12,
                      fontFamily: "'Plus Jakarta Sans', system-ui, sans-serif",
                      fontWeight: 600,
                      color: isSelected || isFocusNode ? '#FFFFFF' : '#DDE6F0',
                      lineHeight: '16px',
                      overflow: 'hidden',
                      display: '-webkit-box',
                      WebkitLineClamp: 3,
                      WebkitBoxOrient: 'vertical',
                    }}>
                      {task.name}
                    </div>
                  </foreignObject>
                  {/* Divider */}
                  <line x1={L+8} y1={T+NODE_H-24} x2={L+NODE_W-8} y2={T+NODE_H-24} stroke={colors.border} strokeWidth={0.5} opacity={0.5}/>
                  {/* Footer */}
                  <text x={L+14} y={T+NODE_H-9} fontSize={10} fill="#3D5068" fontFamily="monospace">#{task.id}</text>
                  <text x={L+NODE_W/2} y={T+NODE_H-9} fontSize={10} fill={colors.text} fontFamily="system-ui" textAnchor="middle">{task.state.replace('_',' ')}</text>
                  <circle cx={L+NODE_W-12} cy={T+NODE_H-14} r={5} fill={eColor} opacity={0.9}>
                    <title>{epicById.get(task.epic_id)?.name}</title>
                  </circle>
                  {/* Selected indicator */}
                  {isSelected && (
                    <circle cx={L+NODE_W/2} cy={T-8} r={4} fill="#FBBF24" opacity={0.9}/>
                  )}
                  {/* Blocked badge */}
                  {task.state==='blocked' && (task.blocked_by_ids?.length??0)>0 && (
                    <g>
                      <rect x={x-50} y={T-22} width={100} height={18} rx={6} fill="#180404" stroke="#EF4444" strokeWidth={0.75}/>
                      <text x={x} y={T-9} fontSize={10} fill="#F87171" fontFamily="system-ui" textAnchor="middle">
                        blocked by {task.blocked_by_ids!.length}
                      </text>
                    </g>
                  )}
                </g>
              )
            })}

            {/* ── Layer 3: highlighted chain edges ── */}
            {hlEdges.map(edge => {
              const fn = nodePos(edge.from, nodes, nodeOverrides)
              const tn = nodePos(edge.to,   nodes, nodeOverrides)
              const st = EDGE_STYLE[edge.kind]
              const isFromSel  = edge.from === focusId
              const isToSel    = edge.to   === focusId
              const stroke = edge.blocked ? '#EF4444'
                           : isFromSel || isToSel ? '#FBBF24'
                           : focusChain?.deps.has(edge.from) ? '#818CF8'
                           : '#A78BFA'
              const marker = edge.blocked ? 'arr-red'
                           : isFromSel || isToSel ? 'arr-gold'
                           : focusChain?.deps.has(edge.from) ? 'arr-lit'
                           : 'arr-purple'
              return (
                <path key={`hl-${edge.from}-${edge.to}`}
                  d={edgePath(fn, tn, edge.points)} fill="none"
                  stroke={stroke} strokeWidth={st.width + 1}
                  strokeDasharray={edge.blocked?'4 3':st.dash}
                  strokeOpacity={1} markerEnd={`url(#${marker})`}/>
              )
            })}

            {/* ── Layer 4: hover tooltip ── */}
            {hoveredId !== null && hoveredId !== selectedId && (() => {
              const pos  = nodePos(hoveredId, nodes, nodeOverrides)
              const task = nodes.get(hoveredId)?.task
              if (!task) return null
              const epic  = epicById.get(task.epic_id)
              const story = storyById.get(task.story_id)
              const eColor = epicColor(epicIdx.get(task.epic_id) ?? 0)
              const L = pos.x - NODE_W/2
              const T = pos.y - NODE_H/2
              const ttH = 46
              return (
                <g pointerEvents="none">
                  <rect x={L} y={T+NODE_H+6} width={NODE_W} height={ttH} rx={8} fill="#0D1726" stroke="#1E2C45" strokeWidth={1}/>
                  <text x={L+12} y={T+NODE_H+22} fontSize={11} fontWeight={600} fill={eColor} fontFamily="system-ui">{epic?.name}</text>
                  {story && <text x={L+12} y={T+NODE_H+38} fontSize={10} fill="#7E91A8" fontFamily="system-ui">{story.name}</text>}
                </g>
              )
            })()}
          </svg>
        )}
      </div>

      {/* ── Selected task info strip ── */}
      {selectedTaskObj && selectedChain && (
        <div
          className="flex-shrink-0 flex items-center gap-4 px-5 py-3"
          style={{ background:'#0D1726', borderTop:'1px solid #1E2C45', minHeight:56 }}
        >
          {/* Breadcrumb */}
          <div className="flex items-center gap-1.5 text-xs min-w-0 flex-1 overflow-hidden">
            <span className="text-[#3D5068] font-mono tabular-nums flex-shrink-0">#{selectedTaskObj.id}</span>
            <ArrowRight size={10} className="text-[#283A57] flex-shrink-0"/>
            <span className="text-[#FBBF24] font-semibold truncate" style={{ maxWidth:120 }}>
              {epicById.get(selectedTaskObj.epic_id)?.name}
            </span>
            <ArrowRight size={10} className="text-[#283A57] flex-shrink-0"/>
            <span className="text-[#7E91A8] truncate" style={{ maxWidth:140 }}>
              {storyById.get(selectedTaskObj.story_id)?.name}
            </span>
            <ArrowRight size={10} className="text-[#283A57] flex-shrink-0"/>
            <span className="text-[#DDE6F0] font-semibold truncate">{selectedTaskObj.name}</span>
          </div>

          {/* Chain legend */}
          <div className="flex items-center gap-3 flex-shrink-0">
            {selectedChain.deps.size > 0 && (
              <span className="flex items-center gap-1.5 text-xs font-medium" style={{ color:'#818CF8' }}>
                <span className="w-2 h-2 rounded-full bg-[#818CF8]"/>
                {selectedChain.deps.size} dep{selectedChain.deps.size!==1?'s':''}
              </span>
            )}
            {selectedChain.dependents.size > 0 && (
              <span className="flex items-center gap-1.5 text-xs font-medium" style={{ color:'#A78BFA' }}>
                <span className="w-2 h-2 rounded-full bg-[#A78BFA]"/>
                {selectedChain.dependents.size} dependent{selectedChain.dependents.size!==1?'s':''}
              </span>
            )}
            <StatusBadge state={selectedTaskObj.state}/>
          </div>

          {/* Actions */}
          <div className="flex items-center gap-2 flex-shrink-0">
            <button
              onClick={() => setDrawerTask(selectedTaskObj)}
              className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold cursor-pointer transition-all active:scale-[0.97]"
              style={{ background:'rgba(99,102,241,0.12)', border:'1px solid rgba(99,102,241,0.25)', color:'#818CF8' }}
            >
              <ExternalLink size={11}/>
              Details
            </button>
            <button
              onClick={() => setSelectedId(null)}
              className="p-1.5 rounded-lg text-[#3D5068] hover:text-[#DDE6F0] hover:bg-[#172540] transition-colors cursor-pointer"
            >
              <X size={13}/>
            </button>
          </div>
        </div>
      )}

      {/* Task drawer — opened from info strip */}
      <TaskDrawer task={drawerTask} onClose={() => setDrawerTask(null)}/>
    </div>
  )
}

// ─── Edge styles ──────────────────────────────────────────────────────────────
const EDGE_STYLE = {
  'same-story':  { stroke:'#283A57', dash:undefined, width:1.5 },
  'cross-story': { stroke:'#6366F1', dash:'6 3',     width:1.5 },
  'cross-epic':  { stroke:'#A78BFA', dash:'9 4',     width:2   },
} as const

// ─── Legend line ──────────────────────────────────────────────────────────────
function LegendLine({ stroke, dash, label }: { stroke:string; dash?:string; label:string }) {
  return (
    <span className="flex items-center gap-1.5">
      <svg width="28" height="10" className="flex-shrink-0">
        <line x1="2" y1="5" x2="22" y2="5" stroke={stroke} strokeWidth="1.5" strokeDasharray={dash}/>
        <polygon points="20,2 28,5 20,8" fill={stroke} opacity="0.85"/>
      </svg>
      <span>{label}</span>
    </span>
  )
}
