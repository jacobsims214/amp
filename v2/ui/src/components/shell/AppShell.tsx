import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useMatch } from 'react-router-dom'
import {
  Activity, BookOpen, ChevronLeft, ChevronRight, Home, Inbox, Menu, MonitorSmartphone, Radar, X, Rows3, Building2, CalendarDays, FolderKanban, GitBranch, GitCommitHorizontal, GraduationCap, KeyRound, LogOut, PieChart, LayoutGrid, Settings, Shapes, ShieldCheck, ScrollText,
} from 'lucide-react'
import { api } from '../../api/client'
import { useAuth } from '../../hooks/useAuth'
import { remoteAvailable, useRemote } from '../../remote/remote-context'
import type { Project } from '../../types'
import { refreshWaitCounts, useWaitCounts } from '../../lib/waitCounts'
import { Avatar } from '../ui/Avatar'
import { viewLink, type ProjectView } from '../../lib/projectLink'
import { projectAbbreviation } from '../../lib/projectAbbreviation'

// The frame around every page: one sidebar to get anywhere, the page beside
// it. The current project's views sit under it in the sidebar, so nothing
// needs a back button.

const PROJECT_VIEWS: { view: ProjectView; label: string; icon: React.ReactNode; badge?: boolean }[] = [
  { view: 'mission', label: 'Mission', icon: <Radar size={14} /> },
  { view: 'my-work', label: 'My work', icon: <Inbox size={14} />, badge: true },
  { view: 'board', label: 'Board', icon: <LayoutGrid size={14} /> },
  { view: 'plan', label: 'Flight plan', icon: <Rows3 size={14} /> },
  { view: 'dag', label: 'Graph', icon: <GitBranch size={14} /> },
  { view: 'report', label: 'Reports', icon: <Activity size={14} /> },
  { view: 'artifacts', label: 'Artifacts', icon: <Shapes size={14} /> },
  { view: 'kb', label: 'Library', icon: <BookOpen size={14} /> },
  { view: 'settings', label: 'Settings', icon: <Settings size={14} /> },
]

function navCls({ isActive }: { isActive: boolean }) {
  return [
    'group flex items-center gap-2.5 h-8 px-2.5 text-[13.5px] transition-colors',
    isActive
      ? 'bg-surface text-ink font-semibold shadow-[inset_3px_0_0_var(--accent),0_0_0_1px_var(--line)]'
      : 'text-ink-2 hover:bg-surface-3 hover:text-ink',
  ].join(' ')
}

/** A small project "avatar": a 3-letter abbreviation derived from the name. */
function ProjectAvatar({ name, title, collapsed }: { name: string; title: string; collapsed?: boolean }) {
  const abbr = projectAbbreviation(name)
  return (
    <span
      className={`inline-flex items-center justify-center bg-surface-3 text-ink-2 font-data font-semibold leading-none select-none ${collapsed ? 'w-[22px] h-[22px] text-[10px]' : 'w-5 h-5 text-[9px]'}`}
      style={{ borderRadius: 'var(--radius)' }}
      title={title}
    >
      {abbr}
    </span>
  )
}

/** A "waiting on you" count: white on the accent, read as "N waiting on you". */
function Badge({ n }: { n?: number }) {
  if (!n) return null
  return <span className="ml-auto amp-num text-[11px] leading-none text-white bg-accent px-[5px] py-[2px] shrink-0" aria-hidden="true">{n}</span>
}

/** The accessible name of a link with a Badge: "My work, 6 waiting on you". */
function badged(label: string, n?: number) {
  return n ? `${label}, ${n} waiting on you` : undefined
}

function Group({ children, collapsed }: { children: React.ReactNode; collapsed?: boolean }) {
  return <div className={`amp-label px-2.5 pt-5 pb-2 ${collapsed ? 'md:px-0 md:flex md:justify-center' : ''}`}>{<span className={collapsed ? 'md:hidden' : ''}>{children}</span>}</div>
}

export function AppShell() {
  const { me, isAdmin } = useAuth()
  const location = useLocation()
  const projectMatch = useMatch('/project/:projectId/*')
  const currentId = projectMatch?.params.projectId
  const [projects, setProjects] = useState<Project[]>([])
  const remote = useRemote()
  const [navOpen, setNavOpen] = useState(false)
  // On desktop the sidebar can be collapsed to a narrow rail.
  const [collapsed, setCollapsed] = useState(false)
  const collapsedCls = collapsed ? 'md:w-[64px] md:items-center' : 'md:w-[232px]'

  // Refresh the list when moving around (a project may have been created,
  // renamed, or archived on the page just left).
  useEffect(() => {
    api.listProjects().then(setProjects).catch(() => setProjects([]))
    refreshWaitCounts()
  }, [location.pathname])
  const wait = useWaitCounts()
  const waitIn = (id: number | string) => wait?.projects[String(id)]?.waiting
  // The phone bar: this project's count inside a project, the total elsewhere.
  const phoneWaiting = (currentId ? waitIn(currentId) : wait?.counts.waiting) ?? 0

  const orgAdmin = (me?.orgs ?? []).some(o => o.my_role === 'owner' || o.my_role === 'admin')
  // The role people know themselves by: org owner or admin, else member.
  const orgRole = (me?.orgs ?? []).find(o => o.my_role === 'owner') ?? (me?.orgs ?? []).find(o => o.my_role === 'admin')
  const orgRoleLabel = orgRole ? `${orgRole.my_role}, ${orgRole.name}` : 'member'
  const current = projects.find(p => String(p.id) === currentId)
  const others = projects.filter(p => String(p.id) !== currentId).slice(0, 8)

  return (
    <div className="relative flex h-screen overflow-hidden bg-bg">
      {navOpen && <div className="fixed inset-0 z-30 bg-[var(--scrim)] md:hidden" onClick={() => setNavOpen(false)} aria-hidden="true" />}
      <aside id="main-nav" className={`${collapsedCls} flex-shrink-0 flex-col bg-surface-2 border-r border-line overflow-y-auto fixed inset-y-0 left-0 z-40 transition-[width] duration-220 ease-[cubic-bezier(0.2,0.8,0.2,1)] md:static md:z-auto md:flex ${navOpen ? 'flex' : 'hidden'}`} aria-label="Main"
        onClick={e => { if ((e.target as HTMLElement).closest('a')) setNavOpen(false) }}>
        <button type="button" className="hidden md:flex absolute top-3 -right-3 z-50 items-center justify-center w-6 h-6 bg-surface border border-line rounded-sm text-ink-3 hover:text-ink hover:bg-surface-3 shadow-sm" onClick={() => setCollapsed(c => !c)} aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'} aria-expanded={!collapsed}>
          {collapsed ? <ChevronRight size={14} /> : <ChevronLeft size={14} />}
        </button>
        <button type="button" className="md:hidden absolute top-3 right-2 p-1.5 text-ink-3 hover:text-ink" onClick={() => setNavOpen(false)} aria-label="Close menu"><X size={16} /></button>
        <NavLink to="/" className={`flex items-center gap-2.5 px-4 pt-4 pb-3 ${collapsed ? 'md:justify-center md:px-2' : ''}`} aria-label="AMP home">
          {/* Relights on every page change (keyed by path). */}
          <span key={location.pathname} className="amp-mark amp-ignite" aria-hidden="true">
            <i className="bg-band-1" /><i className="bg-band-2" /><i className="bg-band-3" /><i className="bg-band-4" />
          </span>
          <span className={`text-[17px] font-bold tracking-[0.16em] text-ink ${collapsed ? 'md:hidden' : ''}`}>AMP</span>
        </NavLink>

        <nav className={`flex flex-col px-2 pb-4 ${collapsed ? 'md:px-1 md:items-center' : ''}`}>
          <Group collapsed={collapsed}>Overview</Group>
          <NavLink to="/" end className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`} aria-label={badged('My work', wait?.counts.waiting)}><span className="inline-flex" title={collapsed ? 'My work' : undefined}><Home size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>My work</span><span className={collapsed ? 'md:hidden' : ''}><Badge n={wait?.counts.waiting} /></span></NavLink>
          {remoteAvailable(me, remote) && <NavLink to="/sessions" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Sessions' : undefined}><MonitorSmartphone size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Sessions</span></NavLink>}
          <NavLink to="/projects" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Projects' : undefined}><FolderKanban size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Projects</span>
            <span className={`amp-num text-[11px] text-ink-3 ${collapsed ? 'md:hidden' : 'ml-auto'}`}>{projects.length || ''}</span></NavLink>
          <NavLink to="/reports" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Day reports' : undefined}><CalendarDays size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Day reports</span></NavLink>
          <NavLink to="/trace" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Trace' : undefined}><GitCommitHorizontal size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Trace</span></NavLink>
          <NavLink to="/guide" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'AMP guide' : undefined}><GraduationCap size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>AMP guide</span></NavLink>
          {(me?.orgs?.length ?? 0) > 0 && <NavLink to="/portfolio" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Portfolio' : undefined}><PieChart size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Portfolio</span></NavLink>}
          {me?.user_id != null && <NavLink to="/access" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Agents & tokens' : undefined}><KeyRound size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Agents & tokens</span></NavLink>}

          {current && (
            <>
              <Group collapsed={collapsed}>Project</Group>
              <div className={`flex items-center gap-2 px-2.5 pb-1.5 ${collapsed ? 'md:justify-center md:px-0' : ''}`}>
                <ProjectAvatar name={current.name} title={current.name} collapsed={collapsed} />
                <span className={`text-[13.5px] font-semibold text-ink truncate ${collapsed ? 'md:hidden' : ''}`} title={current.name}>{current.name}</span>
                {!collapsed && current.code.length <= 8 && <span className="amp-num text-[10.5px] text-ink-3 border border-line px-1 py-px ml-auto shrink-0">{current.code}</span>}
              </div>
              {PROJECT_VIEWS.map(v => (
                <NavLink key={v.label} to={viewLink(currentId!, v.view, location.search)} className={p => `${navCls(p)} pl-5 ${collapsed ? 'md:justify-center md:px-0 md:pl-0 md:w-10 md:h-10 md:mx-auto' : ''}`} aria-label={v.badge ? badged(v.label, waitIn(currentId!)) : undefined}>
                  <span className="inline-flex" title={collapsed ? v.label : undefined}>{v.icon}</span>
                  <span className={collapsed ? 'md:hidden' : ''}>{v.label}</span>
                  <span className={collapsed ? 'md:hidden' : ''}>{v.badge && <Badge n={waitIn(currentId!)} />}</span>
                </NavLink>
              ))}
            </>
          )}

          {others.length > 0 && (
            <>
              <Group collapsed={collapsed}>{current ? 'Other projects' : 'Projects'}</Group>
              {others.map(p => (
                <NavLink key={p.id} to={`/project/${p.id}`} className={p2 => `${navCls(p2)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`} title={p.name} aria-label={badged(p.name, waitIn(p.id))}>
                  <span className="inline-flex" title={collapsed ? p.name : undefined}>
                    {collapsed ? <ProjectAvatar name={p.name} title={p.name} collapsed={collapsed} /> : null}
                  </span>
                  <span className={`truncate ${collapsed ? 'md:hidden' : ''}`}>{p.name}</span>
                  {/* Something waiting beats the code; long slugs crowd the name out of a narrow sidebar. */}
                  {!collapsed && (waitIn(p.id) ? <Badge n={waitIn(p.id)} />
                    : p.code.length <= 8 && <span className="amp-num text-[10.5px] text-ink-3 ml-auto shrink-0">{p.code}</span>)}
                </NavLink>
              ))}
            </>
          )}

          {(isAdmin || orgAdmin) && (
            <>
              <Group collapsed={collapsed}>Admin</Group>
              {isAdmin && <NavLink to="/admin/users" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'People & agents' : undefined}><ShieldCheck size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>People & agents</span></NavLink>}
              <NavLink to="/admin/orgs" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Teams & orgs' : undefined}><Building2 size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Teams & orgs</span></NavLink>
              {isAdmin && <NavLink to="/admin/audit" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Audit log' : undefined}><ScrollText size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Audit log</span></NavLink>}
            </>
          )}
          {!isAdmin && !orgAdmin && me && (
            <>
              <Group collapsed={collapsed}>Organization</Group>
              <NavLink to="/admin/orgs" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Teams & orgs' : undefined}><Building2 size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Teams & orgs</span></NavLink>
              {isAdmin && <NavLink to="/admin/audit" className={p => `${navCls(p)} ${collapsed ? 'md:justify-center md:px-0 md:w-10 md:h-10 md:mx-auto' : ''}`}><span className="inline-flex" title={collapsed ? 'Audit log' : undefined}><ScrollText size={14} /></span><span className={collapsed ? 'md:hidden' : ''}>Audit log</span></NavLink>}
            </>
          )}
        </nav>

        {me && (
          <div className={`mt-auto flex items-center gap-2.5 px-4 py-3 border-t border-line ${collapsed ? 'md:justify-center md:px-2' : ''}`}>
            <span className="inline-flex" title={collapsed ? (me.name || me.email) : undefined}><Avatar name={me.name || me.email} kind={me.kind} /></span>
            <div className={`min-w-0 flex-1 ${collapsed ? 'md:hidden' : ''}`}>
              <div className="text-[13px] text-ink truncate">{me.name || me.email}</div>
              <div className="amp-num text-[10.5px] text-ink-3 truncate">{isAdmin ? 'admin' : me.kind === 'agent' ? 'agent' : orgRoleLabel} · {me.email}</div>
            </div>
            {/* Signed in through the browser: oauth2-proxy clears the session and sends you to sign in again. */}
            {me.user_id != null && !me.via_token && (
              <a href="/oauth2/sign_out?rd=%2F" title="Sign out" aria-label="Sign out"
                className={`shrink-0 p-1.5 text-ink-3 hover:text-ink hover:bg-surface-3 transition-colors ${collapsed ? 'md:hidden' : ''}`}>
                <LogOut size={14} />
              </a>
            )}
          </div>
        )}
      </aside>

      <main className="relative flex-1 min-w-0 h-full overflow-auto">
        <div className="md:hidden sticky top-0 z-20 flex items-center gap-2 h-11 px-2 bg-surface border-b border-line">
          <button type="button" className="p-2 text-ink" onClick={() => setNavOpen(true)} aria-label="Open menu" aria-expanded={navOpen} aria-controls="main-nav"><Menu size={18} /></button>
          <span className="text-[15px] font-bold tracking-[0.16em] text-ink">AMP</span>
          {phoneWaiting > 0 && (
            <NavLink to={currentId ? `/project/${currentId}/my-work` : '/'} className="ml-auto mr-1 amp-num text-[11px] text-white bg-accent px-1.5 py-0.5">
              {phoneWaiting} waiting on you</NavLink>
          )}
        </div>
        <Outlet />
      </main>
    </div>
  )
}
