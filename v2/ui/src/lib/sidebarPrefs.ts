// The sidebar project tree's per-person state: which projects are pinned,
// the order you put them in, and which orgs and projects are collapsed.
// Kept in localStorage for now (AMP #154): there is no user-preferences API,
// and a wrong or missing value only costs a tidy-up, so reads never throw.

export interface SidebarPrefs {
  /** project ids, pinned first-to-last in this order */
  pinned: number[]
  /** project ids in the order the person arranged them (all groups) */
  order: number[]
  /** org keys ('org:7', 'org:none') the person collapsed; absent = open */
  collapsedOrgs: string[]
  /** project ids the person expanded; absent = collapsed */
  openProjects: number[]
}

export const SIDEBAR_KEY = 'amp.sidebar.v1'
export const EMPTY_PREFS: SidebarPrefs = { pinned: [], order: [], collapsedOrgs: [], openProjects: [] }

const nums = (v: unknown): number[] => (Array.isArray(v) ? v.filter((x): x is number => Number.isInteger(x)) : [])
const strs = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [])

export function loadPrefs(): SidebarPrefs {
  try {
    const raw = localStorage.getItem(SIDEBAR_KEY)
    if (!raw) return EMPTY_PREFS
    const o = JSON.parse(raw) as Record<string, unknown>
    return { pinned: nums(o.pinned), order: nums(o.order), collapsedOrgs: strs(o.collapsedOrgs), openProjects: nums(o.openProjects) }
  } catch {
    return EMPTY_PREFS
  }
}

export function savePrefs(p: SidebarPrefs) {
  try { localStorage.setItem(SIDEBAR_KEY, JSON.stringify(p)) } catch { /* a per-viewer preference only */ }
}

/** Sort items by the saved order; ones it doesn't know keep their incoming order, after the known. */
export function sortByOrder<T extends { id: number }>(items: T[], order: number[]): T[] {
  const rank = new Map(order.map((id, i) => [id, i]))
  const big = order.length
  return items
    .map((it, i) => ({ it, i }))
    .sort((a, b) => (rank.get(a.it.id) ?? big) - (rank.get(b.it.id) ?? big) || a.i - b.i)
    .map(x => x.it)
}

/** Move `id` one step (-1 up, +1 down) within `ids` (the visible list) and return the new full order. */
export function moveInOrder(ids: number[], id: number, dir: -1 | 1): number[] {
  const i = ids.indexOf(id)
  const j = i + dir
  if (i < 0 || j < 0 || j >= ids.length) return ids
  const next = ids.slice()
  ;[next[i], next[j]] = [next[j], next[i]]
  return next
}
