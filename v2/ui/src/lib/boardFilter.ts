import type { OwnerFilter } from './people'

export interface FilterState {
  query: string
  state: '' | 'backlog' | 'in_progress' | 'completed' | 'blocked'
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

const STATUS_SLUGS: Record<Exclude<FilterState['state'], ''>, string> = {
  backlog: 'ready', in_progress: 'in_progress', completed: 'done', blocked: 'waiting',
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
