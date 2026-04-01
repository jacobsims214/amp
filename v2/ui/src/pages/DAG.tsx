/**
 * DAG View — Sugiyama hierarchical layout via dagre.
 * Improvements based on review:
 *   - NODE_H 88→104, correct name area height (3×16=48px, no clipping)
 *   - Tooltip moved to SVG portal (last child) to fix z-ordering
 *   - Trail edges split into bg/fg layers so lit edges draw over dimmed cards
 *   - Edge trail predicate fixed: all 5 cases (ancestor↔ancestor, ↔hover, hover↔descendant, descendant↔descendant, ancestor↔descendant)
 *   - Hover clear via onMouseMove on SVG checking e.target === e.currentTarget
 *   - Dimming 0.15→0.38
 *   - Tooltip fontSize 9→12/11, height 28→42, bg lighter, text lighter
 *   - Blocked badge height 14→20, fontSize 9→10
 *   - State bar: wider (4px), taller (top+4 to bottom-4)
 *   - Pre-built reverse-dep map so down() is O(degree) not O(V) per node
 *   - Background fill tint on dep/dependent cards
 */
import { useMemo, useState, useCallback, useRef } from 'react'
import { useParams, Link } from 'react-router-dom'
import { ArrowLeft, Loader2, ZoomIn, ZoomOut, Maximize2 } from 'lucide-react'
import * as dagre from '@dagrejs/dagre'
import { useBoardData } from '../hooks/useBoardData'
import { TaskDrawer } from '../components/TaskDrawer'
import type { Task } from '../types'

// ---- Dimensions ----
const NODE_W  = 240
const NODE_H  = 104
const RANK_SEP = 150
const NODE_SEP = 24

// ---- Colours ----
const STATE_C = {
  backlog:     { border: '#3d444d', bg: '#0d1117',  text: '#8b949e', bar: '#484f58' },
  in_progress: { border: '#1f6feb', bg: '#041d35',  text: '#58a6ff', bar: '#388bfd' },
  completed:   { border: '#238636', bg: '#031a06',  text: '#3fb950', bar: '#2ea043' },
  blocked:     { border: '#b62324', bg: '#1e0303',  text: '#f85149', bar: '#da3633' },
} as const
type SK = keyof typeof STATE_C
const sc = (s: string) => STATE_C[s as SK] ?? STATE_C.backlog

const EPIC_PALETTE = [
  '#388bfd','#a371f7','#3fb950','#e3b341','#f85149','#58a6ff','#bc8cff','#56d364',
]
const epicColor = (i: number) => EPIC_PALETTE[i % EPIC_PALETTE.length]

// ---- Layout ----
interface LayoutResult {
  nodes: Map<number, { x: number; y: number; task: Task }>
  edges: Array<{
    from: number; to: number
    points: Array<{ x: number; y: number }>
    kind: 'same-story' | 'cross-story' | 'cross-epic'
    blocked: boolean
  }>
  width: number
  height: number
}

function runLayout(tasks: Task[]): LayoutResult {
  const g = new dagre.graphlib.Graph()
  g.setGraph({ rankdir:'LR', ranksep:RANK_SEP, nodesep:NODE_SEP, marginx:48, marginy:48, edgesep:12, acyclicer:'greedy', ranker:'network-simplex' })
  g.setDefaultEdgeLabel(() => ({}))
  for (const t of tasks) g.setNode(String(t.id), { width:NODE_W, height:NODE_H, label:t.name })
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
      from: from.id, to: to.id,
      points: ed?.points ?? [],
      kind: from.epic_id !== to.epic_id ? 'cross-epic' : from.story_id !== to.story_id ? 'cross-story' : 'same-story',
      blocked: to.state === 'blocked' && (to.blocked_by_ids ?? []).includes(from.id),
    })
  }
  const gr = g.graph()
  return { nodes, edges, width:(gr.width??800)+96, height:(gr.height??600)+96 }
}

function edgePath(from:{x:number;y:number}, to:{x:number;y:number}, points:Array<{x:number;y:number}>): string {
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

const EDGE_STYLE = {
  'same-story':  { stroke:'#3d444d', dash:undefined, width:1.5 },
  'cross-story': { stroke:'#388bfd', dash:'7 4',     width:1.5 },
  'cross-epic':  { stroke:'#a371f7', dash:'9 5',     width:2   },
} as const

// ---- Component ----
export function DAGView() {
  const { projectId } = useParams<{ projectId:string }>()
  const pid = Number(projectId)
  const { epics, stories, tasks, loading, error } = useBoardData(pid)
  const [selectedTask, setSelectedTask] = useState<Task|null>(null)
  const [hoveredId, setHoveredId] = useState<number|null>(null)
  const [filterEpicId, setFilterEpicId] = useState<number|null>(null)
  const [zoom, setZoom] = useState(1)
  const clearTimer = useRef<ReturnType<typeof setTimeout>|null>(null)

  const epicIdx  = useMemo(() => new Map(epics.map((e,i)=>[e.id,i])), [epics])
  const epicById = useMemo(() => new Map(epics.map(e=>[e.id,e])), [epics])
  const storyById= useMemo(() => new Map(stories.map(s=>[s.id,s])), [stories])

  const visibleTasks = useMemo(() =>
    filterEpicId ? tasks.filter(t=>t.epic_id===filterEpicId) : tasks
  , [tasks, filterEpicId])

  const layout = useMemo(() => runLayout(visibleTasks), [visibleTasks])

  // Pre-build reverse dep map so down() is O(degree) not O(V)
  const reverseDeps = useMemo(() => {
    const m = new Map<number, number[]>()
    tasks.forEach(t => (t.dependency_ids??[]).forEach(d => {
      if (!m.has(d)) m.set(d, [])
      m.get(d)!.push(t.id)
    }))
    return m
  }, [tasks])

  const taskById = useMemo(() => new Map(tasks.map(t=>[t.id,t])), [tasks])

  // Full transitive chain
  const highlighted = useMemo(() => {
    if (hoveredId === null) return null
    const deps = new Set<number>()
    const dependents = new Set<number>()

    function up(id:number, vis=new Set<number>()) {
      if (vis.has(id)) return; vis.add(id)
      taskById.get(id)?.dependency_ids?.forEach(d => { deps.add(d); up(d, vis) })
    }
    function down(id:number, vis=new Set<number>()) {
      if (vis.has(id)) return; vis.add(id)
      reverseDeps.get(id)?.forEach(child => { dependents.add(child); down(child, vis) })
    }
    up(hoveredId); down(hoveredId)
    return { deps, dependents }
  }, [hoveredId, taskById, reverseDeps])

  // Clear hover: card enter → set, SVG background → clear
  const handleCardEnter = useCallback((id:number) => {
    if (clearTimer.current) clearTimeout(clearTimer.current)
    setHoveredId(id)
  }, [])
  const scheduleClear = useCallback(() => {
    clearTimer.current = setTimeout(() => setHoveredId(null), 80)
  }, [])
  const cancelClear = useCallback(() => {
    if (clearTimer.current) clearTimeout(clearTimer.current)
  }, [])

  const zoomIn    = useCallback(() => setZoom(z=>Math.min(z+0.15,2.5)), [])
  const zoomOut   = useCallback(() => setZoom(z=>Math.max(z-0.15,0.25)), [])
  const zoomReset = useCallback(() => setZoom(1), [])

  if (loading) return <div className="flex items-center justify-center h-screen bg-[#0d1117]"><Loader2 size={24} className="text-[#58a6ff] animate-spin"/></div>
  if (error)   return <div className="flex items-center justify-center h-screen bg-[#0d1117] text-[#f85149] text-sm">{error}</div>

  const { nodes, edges } = layout

  // Split edges into bg (dimmed) and trail (lit)
  const bgEdges: typeof edges = [], trailEdges: typeof edges = []
  for (const edge of edges) {
    const onTrail = highlighted ? (
      (highlighted.deps.has(edge.from)       && highlighted.deps.has(edge.to))       ||
      (highlighted.deps.has(edge.from)       && edge.to === hoveredId)              ||
      (edge.from === hoveredId               && highlighted.dependents.has(edge.to)) ||
      (highlighted.dependents.has(edge.from) && highlighted.dependents.has(edge.to))||
      (highlighted.deps.has(edge.from)       && highlighted.dependents.has(edge.to))
    ) : false
    if (highlighted) { onTrail ? trailEdges.push(edge) : bgEdges.push(edge) }
    else bgEdges.push(edge)
  }

  // Hover tooltip state (rendered as SVG portal — last element in SVG)
  const hoveredNode = hoveredId !== null ? nodes.get(hoveredId) : undefined

  return (
    <div className="flex flex-col h-screen bg-[#0d1117] overflow-hidden">
      {/* Header */}
      <header className="flex items-center gap-3 px-5 py-3 border-b border-[#21262d] bg-[#161b22] flex-shrink-0">
        <Link to={`/project/${pid}`} className="flex items-center gap-1.5 text-[#8b949e] hover:text-[#e6edf3] transition-colors">
          <ArrowLeft size={14}/>
        </Link>
        <div className="w-px h-4 bg-[#30363d]"/>
        <span className="text-sm font-semibold text-[#e6edf3]">Dependency Graph</span>
        <div className="ml-auto flex items-center gap-4">
          <div className="hidden sm:flex items-center gap-4 text-xs text-[#8b949e]">
            <LegendLine stroke="#3d444d" label="Same story"/>
            <LegendLine stroke="#388bfd" dash="7 4" label="Cross story"/>
            <LegendLine stroke="#a371f7" dash="9 5" label="Cross epic"/>
            <LegendLine stroke="#f85149" dash="5 3" label="Blocking"/>
          </div>
          <div className="flex items-center gap-1 bg-[#0d1117] border border-[#30363d] rounded-md p-1">
            <button onClick={zoomOut} className="p-1 rounded hover:bg-[#21262d] text-[#8b949e] hover:text-[#e6edf3]"><ZoomOut size={13}/></button>
            <button onClick={zoomReset} className="px-2 py-1 rounded text-xs text-[#8b949e] hover:text-[#e6edf3] hover:bg-[#21262d] font-mono">{Math.round(zoom*100)}%</button>
            <button onClick={zoomIn}  className="p-1 rounded hover:bg-[#21262d] text-[#8b949e] hover:text-[#e6edf3]"><ZoomIn size={13}/></button>
            <button onClick={zoomReset} className="p-1 rounded hover:bg-[#21262d] text-[#8b949e] hover:text-[#e6edf3]"><Maximize2 size={13}/></button>
          </div>
          {epics.length > 1 && (
            <select value={filterEpicId??''} onChange={e=>setFilterEpicId(e.target.value?Number(e.target.value):null)}
              className="bg-[#0d1117] border border-[#30363d] rounded-md px-3 py-1.5 text-xs text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]/60 appearance-none cursor-pointer">
              <option value="">All epics</option>
              {epics.map(e=><option key={e.id} value={e.id}>{e.name}</option>)}
            </select>
          )}
        </div>
      </header>

      {/* Epic key bar */}
      {epics.length > 0 && (
        <div className="flex items-center gap-3 px-5 py-2 border-b border-[#21262d] bg-[#0d1117] flex-shrink-0 overflow-x-auto">
          <span className="text-xs text-[#484f58] flex-shrink-0">Epics:</span>
          {epics.map((e,i)=>(
            <button key={e.id} onClick={()=>setFilterEpicId(filterEpicId===e.id?null:e.id)}
              className={`flex items-center gap-1.5 px-2 py-0.5 rounded text-xs flex-shrink-0 transition-opacity ${filterEpicId===e.id?'opacity-100':'opacity-60 hover:opacity-100'}`}>
              <span className="w-2 h-2 rounded-full" style={{background:epicColor(i)}}/>
              <span style={{color:epicColor(i)}}>{e.name}</span>
            </button>
          ))}
        </div>
      )}

      {/* Canvas */}
      <div className="flex-1 overflow-auto bg-[#0d1117]" onMouseLeave={scheduleClear}>
        {visibleTasks.length === 0
          ? <div className="flex items-center justify-center h-full text-[#8b949e] text-sm">No tasks to display</div>
          : (
          <div style={{padding:'28px', minWidth:'fit-content', minHeight:'fit-content'}}>
            <svg
              width={layout.width*zoom} height={layout.height*zoom}
              viewBox={`0 0 ${layout.width} ${layout.height}`}
              style={{display:'block'}}
              onMouseMove={e => { if (e.target === e.currentTarget) { cancelClear(); scheduleClear() }}}
              onMouseLeave={scheduleClear}
            >
              <defs>
                {([['arr-grey','#3d444d'],['arr-blue','#388bfd'],['arr-purple','#a371f7'],['arr-red','#f85149']] as const).map(([id,fill])=>(
                  <marker key={id} id={id} markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto">
                    <path d="M0,0 L0,8 L8,4 z" fill={fill} opacity="0.9"/>
                  </marker>
                ))}
                <filter id="glow-h" x="-30%" y="-30%" width="160%" height="160%">
                  <feGaussianBlur in="SourceAlpha" stdDeviation="5" result="b"/>
                  <feFlood floodColor="#e3b341" floodOpacity="0.5" result="c"/>
                  <feComposite in="c" in2="b" operator="in" result="g"/>
                  <feMerge><feMergeNode in="g"/><feMergeNode in="SourceGraphic"/></feMerge>
                </filter>
              </defs>

              {/* Layer 1 — background (dimmed) edges */}
              {bgEdges.map(edge => {
                const fn=nodes.get(edge.from), tn=nodes.get(edge.to)
                if (!fn||!tn) return null
                const st=EDGE_STYLE[edge.kind]
                const stroke=edge.blocked?'#f85149':st.stroke
                const opacity=highlighted?0.06:edge.blocked?0.85:edge.kind==='cross-epic'?0.6:0.45
                const marker=edge.blocked?'arr-red':edge.kind==='cross-epic'?'arr-purple':edge.kind==='cross-story'?'arr-blue':'arr-grey'
                return <path key={`bg-${edge.from}-${edge.to}`} d={edgePath(fn,tn,edge.points)} fill="none" stroke={stroke} strokeWidth={st.width} strokeDasharray={edge.blocked?'5 3':st.dash} strokeOpacity={opacity} markerEnd={`url(#${marker})`}/>
              })}

              {/* Layer 2 — cards (dimmed cards render here, lit cards same z as each other) */}
              {Array.from(nodes.values()).map(({x,y,task})=>{
                const colors=sc(task.state)
                const isHovered=hoveredId===task.id
                const isDep=highlighted?.deps.has(task.id)
                const isDependent=highlighted?.dependents.has(task.id)
                const isDimmed=highlighted&&!isDep&&!isDependent&&!isHovered

                const eIdx=epicIdx.get(task.epic_id)??0
                const eColor=epicColor(eIdx)
                const L=x-NODE_W/2, T=y-NODE_H/2

                const borderColor=isHovered?'#e3b341':isDep?'#388bfd':isDependent?'#a371f7':colors.border
                const borderW=(isHovered||isDep||isDependent)?2:1
                const bgTint=isDep?'rgba(56,139,253,0.07)':isDependent?'rgba(163,113,247,0.07)':'transparent'

                return (
                  <g key={task.id} opacity={isDimmed?0.38:1} style={{cursor:'pointer'}}
                    onMouseEnter={()=>{cancelClear();handleCardEnter(task.id)}}
                    onMouseLeave={scheduleClear}
                    onClick={()=>setSelectedTask(task)}
                    filter={isHovered?'url(#glow-h)':undefined}>

                    {/* Background tint for dep/dependent */}
                    {bgTint!=='transparent'&&<rect x={L} y={T} width={NODE_W} height={NODE_H} rx={7} fill={bgTint}/>}

                    {/* Card body */}
                    <rect x={L} y={T} width={NODE_W} height={NODE_H} rx={7} fill={colors.bg} stroke={borderColor} strokeWidth={borderW}/>

                    {/* Epic colour strip — full width top accent */}
                    <rect x={L+1} y={T+1} width={NODE_W-2} height={3} rx={2} fill={eColor} opacity={0.75}/>

                    {/* State bar — left edge, taller */}
                    <rect x={L} y={T+4} width={4} height={NODE_H-8} rx={2} fill={colors.bar}/>

                    {/* Task name — 3 lines, correct 48px height */}
                    <foreignObject x={L+14} y={T+10} width={NODE_W-24} height={48}>
                      <div style={{
                        fontSize:12, fontFamily:'system-ui,-apple-system,sans-serif', fontWeight:500,
                        color:'#e6edf3', lineHeight:'16px', overflow:'hidden',
                        display:'-webkit-box', WebkitLineClamp:3, WebkitBoxOrient:'vertical',
                        paddingRight:2,
                      }}>
                        {task.name}
                      </div>
                    </foreignObject>

                    {/* Divider */}
                    <line x1={L+8} y1={T+NODE_H-26} x2={L+NODE_W-8} y2={T+NODE_H-26} stroke={colors.border} strokeWidth={0.5} opacity={0.6}/>

                    {/* Footer: id | state | epic dot */}
                    <text x={L+14} y={T+NODE_H-11} fontSize={10} fill="#6e7681" fontFamily="monospace">#{task.id}</text>
                    <text x={L+NODE_W/2} y={T+NODE_H-11} fontSize={10} fill={colors.text} fontFamily="system-ui" textAnchor="middle">{task.state.replace('_',' ')}</text>
                    <circle cx={L+NODE_W-12} cy={T+NODE_H-14} r={5} fill={eColor} opacity={0.85}>
                      <title>{epicById.get(task.epic_id)?.name}</title>
                    </circle>

                    {/* Blocked badge above card */}
                    {task.state==='blocked'&&(task.blocked_by_ids?.length??0)>0&&(
                      <g>
                        <rect x={x-48} y={T-22} width={96} height={18} rx={4} fill="#1e0303" stroke="#da3633" strokeWidth={0.75}/>
                        <text x={x} y={T-9} fontSize={10} fill="#ff7b72" fontFamily="system-ui" textAnchor="middle">
                          waiting on {task.blocked_by_ids!.length}
                        </text>
                      </g>
                    )}
                  </g>
                )
              })}

              {/* Layer 3 — trail edges (over cards so they're always visible) */}
              {trailEdges.map(edge=>{
                const fn=nodes.get(edge.from), tn=nodes.get(edge.to)
                if (!fn||!tn) return null
                const st=EDGE_STYLE[edge.kind]
                const stroke=edge.blocked?'#f85149':st.stroke
                const marker=edge.blocked?'arr-red':edge.kind==='cross-epic'?'arr-purple':edge.kind==='cross-story'?'arr-blue':'arr-grey'
                return <path key={`tr-${edge.from}-${edge.to}`} d={edgePath(fn,tn,edge.points)} fill="none" stroke={stroke} strokeWidth={st.width+0.5} strokeDasharray={edge.blocked?'5 3':st.dash} strokeOpacity={1} markerEnd={`url(#${marker})`}/>
              })}

              {/* Layer 4 — tooltip portal (always on top, never clipped by sibling cards) */}
              {hoveredNode&&(()=>{
                const {x,y,task}=hoveredNode
                const epic=epicById.get(task.epic_id)
                const story=storyById.get(task.story_id)
                const eColor=epicColor(epicIdx.get(task.epic_id)??0)
                const L=x-NODE_W/2, T=y-NODE_H/2
                const ttH=story?44:26
                return (
                  <g pointerEvents="none">
                    <rect x={L} y={T+NODE_H+6} width={NODE_W} height={ttH} rx={5} fill="#21262d" stroke="#30363d" strokeWidth={1}/>
                    <text x={L+10} y={T+NODE_H+20} fontSize={12} fontWeight={500} fill={eColor} fontFamily="system-ui">{epic?.name??''}</text>
                    {story&&<text x={L+10} y={T+NODE_H+36} fontSize={11} fill="#8b949e" fontFamily="system-ui">{story.name}</text>}
                  </g>
                )
              })()}
            </svg>
          </div>
        )}
      </div>

      <TaskDrawer task={selectedTask} onClose={()=>setSelectedTask(null)}/>
    </div>
  )
}

function LegendLine({stroke,dash,label}:{stroke:string;dash?:string;label:string}) {
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
