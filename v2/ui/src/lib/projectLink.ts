/**
 * One link format for every project route (v2/docs/url-state.md):
 * `/project/:id[/<view>][?focus=<ticket id>]`. Views open the focused ticket
 * (Board and Flight plan open the drawer; Mission opens it on arrival; My work
 * outlines the item that contains it).
 *
 * With no view, the link is the project itself, `/project/:id`, which lands on
 * My work when something waits on the viewer there and on Mission otherwise
 * (lib/landing.ts). Naming a view, Mission included, always opens that view.
 */
export type ProjectView = 'mission' | 'my-work' | 'board' | 'plan' | 'dag' | 'report' | 'kb' | 'artifacts' | 'settings'

export function projectLink(projectId: number | string, view?: ProjectView, focusId?: number | null): string {
  const base = view ? `/project/${projectId}/${view}` : `/project/${projectId}`
  return focusId != null && focusId > 0 ? `${base}?focus=${focusId}` : base
}

/** The views that open a ticket from `?focus=`. */
export const FOCUS_VIEWS: readonly ProjectView[] = ['mission', 'my-work', 'board', 'plan', 'dag']

/**
 * The link to `view` from a page whose query string is `search`: a view that
 * honors `?focus=` keeps the ticket that's open, so switching views doesn't
 * lose it. Nothing else carries over (filters belong to the view they're on).
 */
export function viewLink(projectId: number | string, view: ProjectView, search: string): string {
  const focus = Number(new URLSearchParams(search).get('focus'))
  return projectLink(projectId, view, FOCUS_VIEWS.includes(view) && Number.isInteger(focus) ? focus : null)
}
