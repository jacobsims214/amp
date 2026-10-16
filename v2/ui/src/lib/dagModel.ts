import * as dagre from '@dagrejs/dagre'
import type { Epic, Story, Task } from '../types'

// The DAG view's graph model, kept free of React so it's easy to follow:
// which tickets are shown, which stories are collapsed into one node, and
// where everything goes (dagre, left to right, one cluster per expanded
// story).

export const TICKET_W = 220
export const TICKET_H = 78
export const STORY_W = 250
export const STORY_H = 92

export type StateCounts = Record<'backlog' | 'in_progress' | 'blocked' | 'completed', number>

/** Ancestors (what a ticket waits on) and descendants (what waits on it), transitively. */
export function chainOf(id: number, tasks: Task[]): { up: Set<number>; down: Set<number> } {
  const deps = new Map(tasks.map(t => [t.id, t.dependency_ids ?? []]))
  const dependents = new Map<number, number[]>()
  for (const t of tasks) for (const d of t.dependency_ids ?? []) dependents.set(d, [...(dependents.get(d) ?? []), t.id])
  const walk = (start: number, next: Map<number, number[]>) => {
    const seen = new Set<number>()
    const stack = [...(next.get(start) ?? [])]
    while (stack.length) {
      const n = stack.pop()!
      if (seen.has(n) || n === start) continue
      seen.add(n)
      stack.push(...(next.get(n) ?? []))
    }
    return seen
  }
  return { up: walk(id, deps), down: walk(id, dependents) }
}

export interface ModelInput {
  epics: Epic[]
  stories: Story[]
  tasks: Task[]
  /** tickets to show (already filtered) */
  visible: Set<number>
  /** stories shown ticket by ticket; the rest collapse into one node each */
  expanded: Set<number>
}

export interface TicketNodeModel { kind: 'ticket'; id: string; task: Task; x: number; y: number }
export interface StoryNodeModel {
  kind: 'story'; id: string; story: Story; epic?: Epic
  tasks: Task[]; counts: StateCounts; x: number; y: number
}
export interface ClusterModel { kind: 'cluster'; id: string; story: Story; epic?: Epic; x: number; y: number; width: number; height: number; tasks: Task[] }
export interface EdgeModel {
  id: string; source: string; target: string
  /** ticket→ticket edges carry the ticket ids; story edges carry how many dependencies they stand for */
  from?: number; to?: number; count: number
  blocked: boolean; crossEpic: boolean
}

export interface GraphModel {
  tickets: TicketNodeModel[]
  stories: StoryNodeModel[]
  clusters: ClusterModel[]
  edges: EdgeModel[]
}

export const ticketNodeId = (id: number) => `t${id}`
export const storyNodeId = (id: number) => `s${id}`
const clusterId = (id: number) => `c${id}`

export function countStates(tasks: Task[]): StateCounts {
  const c: StateCounts = { backlog: 0, in_progress: 0, blocked: 0, completed: 0 }
  for (const t of tasks) if (t.state in c) c[t.state as keyof StateCounts]++
  return c
}

export function buildGraph({ epics, stories, tasks, visible, expanded }: ModelInput): GraphModel {
  const storyById = new Map(stories.map(s => [s.id, s]))
  const epicById = new Map(epics.map(e => [e.id, e]))
  const shown = tasks.filter(t => visible.has(t.id))
  const byStory = new Map<number, Task[]>()
  for (const t of shown) byStory.set(t.story_id, [...(byStory.get(t.story_id) ?? []), t])

  // Where each shown ticket is drawn: as itself, or inside its story's node.
  const nodeOf = new Map<number, string>()
  for (const t of shown) nodeOf.set(t.id, expanded.has(t.story_id) ? ticketNodeId(t.id) : storyNodeId(t.story_id))

  const g = new dagre.graphlib.Graph({ compound: true })
  g.setGraph({ rankdir: 'LR', ranksep: 90, nodesep: 24, marginx: 40, marginy: 40, acyclicer: 'greedy', ranker: 'network-simplex' })
  g.setDefaultEdgeLabel(() => ({}))

  for (const [storyId, list] of byStory) {
    if (expanded.has(storyId)) {
      // dagre leaves ~22px inside a cluster's border: room for the story's title strip.
      g.setNode(clusterId(storyId), {})
      for (const t of list) {
        g.setNode(ticketNodeId(t.id), { width: TICKET_W, height: TICKET_H })
        g.setParent(ticketNodeId(t.id), clusterId(storyId))
      }
    } else {
      g.setNode(storyNodeId(storyId), { width: STORY_W, height: STORY_H })
    }
  }

  // Edges between drawn nodes; a story node's edges stand for all of its
  // tickets' dependencies (counted), and edges inside one story node vanish.
  const edgeMap = new Map<string, EdgeModel>()
  const taskById = new Map(tasks.map(t => [t.id, t]))
  for (const t of shown) {
    for (const d of t.dependency_ids ?? []) {
      const dep = taskById.get(d)
      const source = nodeOf.get(d), target = nodeOf.get(t.id)
      if (!dep || !source || !target || source === target) continue
      const key = `${source}->${target}`
      const e = edgeMap.get(key) ?? {
        id: key, source, target, count: 0, blocked: false,
        crossEpic: dep.epic_id !== t.epic_id,
        ...(source.startsWith('t') && target.startsWith('t') ? { from: d, to: t.id } : {}),
      }
      e.count++
      e.blocked ||= t.state === 'blocked' && (t.blocked_by_ids ?? []).includes(d)
      edgeMap.set(key, e)
    }
  }
  for (const e of edgeMap.values()) g.setEdge(e.source, e.target)

  dagre.layout(g)

  const model: GraphModel = { tickets: [], stories: [], clusters: [], edges: [...edgeMap.values()] }
  for (const [storyId, list] of byStory) {
    const story = storyById.get(storyId)
    if (!story) continue
    const epic = epicById.get(story.epic_id)
    if (expanded.has(storyId)) {
      const c = g.node(clusterId(storyId))
      if (c) model.clusters.push({ kind: 'cluster', id: clusterId(storyId), story, epic, x: c.x - c.width / 2, y: c.y - c.height / 2, width: c.width, height: c.height, tasks: list })
      for (const t of list) {
        const n = g.node(ticketNodeId(t.id))
        if (n) model.tickets.push({ kind: 'ticket', id: ticketNodeId(t.id), task: t, x: n.x - TICKET_W / 2, y: n.y - TICKET_H / 2 })
      }
    } else {
      const n = g.node(storyNodeId(storyId))
      if (n) model.stories.push({ kind: 'story', id: storyNodeId(storyId), story, epic, tasks: list, counts: countStates(list), x: n.x - STORY_W / 2, y: n.y - STORY_H / 2 })
    }
  }
  return model
}
