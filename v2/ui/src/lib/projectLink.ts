/**
 * One link format for every project route (KB decisions/human-work-ux.md §3):
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
