import type { OwnerFilter } from './people'
import type { Task } from '../types'
import { displayState } from './ticketState'

/**
 * The Board's status filters. A blocked ticket is one of two different waits
 * (#129): 'question' waits on a person (an agent's question, red) and
 * 'waiting' on other tickets (dependencies or a merge, grey). They're
 * separate filters, so one question isn't lost among dependency waits.
 */
export type BoardStatus = 'backlog' | 'in_progress' | 'completed' | 'question' | 'waiting'

export interface FilterState {
  query: string
  state: '' | BoardStatus
  epicId: number | null
  /** whose tickets to show; defaults to 'mine' */
  owner: OwnerFilter
}

export const DEFAULT_FILTER: FilterState = { query: '', state: '', epicId: null, owner: 'mine' }

// ─── In the URL (v2/docs/url-state.md) ────────────────────────────────────────
// User-facing slugs, not the API's state names, so a link reads the way the
// pills do: status=ready|in_progress|done|waiting, q=, epic=<id>,
// owner=mine|unowned|all|<user id>. Defaults are left out, so a plain /board
// stays clean.

export const BOARD_FILTER_KEYS = ['status', 'q', 'epic', 'owner'] as const
export type BoardFilterKey = (typeof BOARD_FILTER_KEYS)[number]

const STATUS_SLUGS: Record<BoardStatus, string> = {
  backlog: 'ready', in_progress: 'in_progress', completed: 'done', question: 'question', waiting: 'waiting',
}
const STATUS_BY_SLUG = Object.fromEntries(
  Object.entries(STATUS_SLUGS).map(([state, slug]) => [slug, state]),
) as Record<string, FilterState['state']>

function parseOwnerSlug(v: string | null): OwnerFilter {
  if (v === 'all' || v === 'everyone') return 'everyone'
  if (v === 'mine' || v === 'unowned') return v
  const n = Number(v)
  return v && Number.isInteger(n) && n > 0 ? n : DEFAULT_FILTER.owner
}

/** The Board filter in `params`; anything missing or unusable is its default. */
export function parseBoardFilter(params: URLSearchParams): FilterState {
  const epic = Number(params.get('epic'))
  return {
    query: params.get('q') ?? '',
    state: STATUS_BY_SLUG[params.get('status') ?? ''] ?? '',
    epicId: Number.isInteger(epic) && epic > 0 ? epic : null,
    owner: parseOwnerSlug(params.get('owner')),
  }
}

/** The param updates that write `f`: null removes a key that holds its default. */
export function serializeBoardFilter(f: FilterState): Record<BoardFilterKey, string | null> {
  return {
    status: f.state ? STATUS_SLUGS[f.state] : null,
    q: f.query || null,
    epic: f.epicId != null ? String(f.epicId) : null,
    owner: f.owner === DEFAULT_FILTER.owner ? null : f.owner === 'everyone' ? 'all' : String(f.owner),
  }
}

/** Which status filter a ticket falls under; null for a scheduled one (it has its own column, no filter). */
export function boardStatus(t: Pick<Task, 'state'> & Partial<Pick<Task, 'block_reason' | 'blocked_by_ids'>>): BoardStatus | null {
  switch (displayState(t)) {
    case 'flag': return 'question'
    case 'wait': return 'waiting'
    case 'sched': return null
    case 'ready': return 'backlog'
    case 'prog': return 'in_progress'
    case 'done': return 'completed'
  }
}
