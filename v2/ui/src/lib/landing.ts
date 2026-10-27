/**
 * Which tab a project opens on (KB decisions/human-work-ux.md, review outcome 3):
 * `/project/:id` with no tab opens My work when the viewer has anything waiting
 * on them in the project, and Mission otherwise. A tab in the URL always wins,
 * so `/project/:id/mission` and `/project/:id/my-work` never redirect.
 *
 * Links from before the My work tab (`/project/:id?focus=`, `?ticket=`,
 * `?task=`) opened a ticket on Mission; they still do.
 */
export type LandingTab = 'my-work' | 'mission'

const LEGACY_TICKET_PARAMS = ['focus', 'ticket', 'task']

/** The tab the URL decides on its own (old ticket links, ?approve=), or null when it depends on the counts. */
export function fixedTab(search: string | URLSearchParams): LandingTab | null {
  const params = typeof search === 'string' ? new URLSearchParams(search) : search
  if (params.has('approve')) return 'my-work'
  if (LEGACY_TICKET_PARAMS.some(p => params.has(p))) return 'mission'
  return null
}

export function landingTab(search: string | URLSearchParams, waiting: number | null | undefined): LandingTab {
  return fixedTab(search) ?? ((waiting ?? 0) > 0 ? 'my-work' : 'mission')
}

/** The URL to replace `/project/:id[?…]` with: the tab, query kept. */
export function landingPath(projectId: string | number, search: string, tab: LandingTab): string {
  const q = search.replace(/^\?/, '')
  return `/project/${projectId}/${tab}${q ? `?${q}` : ''}`
}
