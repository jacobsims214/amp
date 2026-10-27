/**
 * One link format for every project route (KB decisions/human-work-ux.md §3):
 * `/project/:id[/<view>][?focus=<ticket id>]`. Views open the focused ticket
 * (Board and Flight plan open the drawer; Mission opens it on arrival).
 */
export type ProjectView = 'mission' | 'my-work' | 'board' | 'plan' | 'dag' | 'report' | 'kb' | 'artifacts' | 'settings'

export function projectLink(projectId: number | string, view: ProjectView = 'mission', focusId?: number | null): string {
  const base = view === 'mission' ? `/project/${projectId}` : `/project/${projectId}/${view}`
  return focusId != null && focusId > 0 ? `${base}?focus=${focusId}` : base
}
