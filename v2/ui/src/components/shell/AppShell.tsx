import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useMatch } from 'react-router-dom'
import {
  Activity, BookOpen, Home, Menu, MonitorSmartphone, Radar, X, Rows3, Building2, CalendarDays, FolderKanban, GitBranch, GitCommitHorizontal, GraduationCap, KeyRound, LogOut, PieChart, LayoutGrid, Settings, Shapes, ShieldCheck, ScrollText,
} from 'lucide-react'
import { api } from '../../api/client'
import { useAuth } from '../../hooks/useAuth'
import { remoteAvailable, useRemote } from '../../remote/remote-context'
import type { Project } from '../../types'
import { Avatar } from '../ui/Avatar'

// The frame around every page: one sidebar to get anywhere, the page beside
// it. The current project's views sit under it in the sidebar, so nothing
// needs a back button.

const PROJECT_VIEWS: { to: (id: string) => string; label: string; icon: React.ReactNode; end?: boolean }[] = [
  { to: id => `/project/${id}`, label: 'Mission', icon: <Radar size={14} />, end: true },
  { to: id => `/project/${id}/board`, label: 'Board', icon: <LayoutGrid size={14} /> },
  { to: id => `/project/${id}/plan`, label: 'Flight plan', icon: <Rows3 size={14} /> },
  { to: id => `/project/${id}/dag`, label: 'Graph', icon: <GitBranch size={14} /> },
  { to: id => `/project/${id}/report`, label: 'Reports', icon: <Activity size={14} /> },
  { to: id => `/project/${id}/artifacts`, label: 'Artifacts', icon: <Shapes size={14} /> },
  { to: id => `/project/${id}/kb`, label: 'Library', icon: <BookOpen size={14} /> },
  { to: id => `/project/${id}/settings`, label: 'Settings', icon: <Settings size={14} /> },
]

function navCls({ isActive }: { isActive: boolean }) {
  return [
    'group flex items-center gap-2.5 h-8 px-2.5 text-[13.5px] transition-colors',
    isActive
      ? 'bg-surface text-ink font-semibold shadow-[inset_3px_0_0_var(--accent),0_0_0_1px_var(--line)]'
      : 'text-ink-2 hover:bg-surface-3 hover:text-ink',
  ].join(' ')
}

function Group({ children }: { children: React.ReactNode }) {
  return <div className="amp-label px-2.5 pt-5 pb-2">{children}</div>
}

export function AppShell() {
  const { me, isAdmin } = useAuth()
  const location = useLocation()
  const projectMatch = useMatch('/project/:projectId/*')
  const currentId = projectMatch?.params.projectId
  const [projects, setProjects] = useState<Project[]>([])
  const remote = useRemote()
  // Below md the sidebar is a drawer behind a menu button, so pages fit a phone.
  const [navOpen, setNavOpen] = useState(false)

  // Refresh the list when moving around (a project may have been created,
  // renamed, or archived on the page just left).
  useEffect(() => {
    api.listProjects().then(setProjects).catch(() => setProjects([]))
  }, [location.pathname])

  const orgAdmin = (me?.orgs ?? []).some(o => o.my_role === 'owner' || o.my_role === 'admin')
  // The role people know themselves by: org owner or admin, else member.
  const orgRole = (me?.orgs ?? []).find(o => o.my_role === 'owner') ?? (me?.orgs ?? []).find(o => o.my_role === 'admin')
  const orgRoleLabel = orgRole ? `${orgRole.my_role}, ${orgRole.name}` : 'member'
  const current = projects.find(p => String(p.id) === currentId)
  const others = projects.filter(p => String(p.id) !== currentId).slice(0, 8)

  return (
    <div className="relative flex h-screen overflow-hidden bg-bg">
      {navOpen && <div className="fixed inset-0 z-30 bg-[var(--scrim)] md:hidden" onClick={() => setNavOpen(false)} aria-hidden="true" />}
      <aside id="main-nav" className={`w-[232px] flex-shrink-0 flex-col bg-surface-2 border-r border-line overflow-y-auto fixed inset-y-0 left-0 z-40 md:static md:z-auto md:flex ${navOpen ? 'flex' : 'hidden'}`} aria-label="Main"
        onClick={e => { if ((e.target as HTMLElement).closest('a')) setNavOpen(false) }}>
        <button type="button" className="md:hidden absolute top-3 right-2 p-1.5 text-ink-3 hover:text-ink" onClick={() => setNavOpen(false)} aria-label="Close menu"><X size={16} /></button>
        <NavLink to="/" className="flex items-center gap-2.5 px-4 pt-4 pb-3" aria-label="AMP home">
          {/* Relights on every page change (keyed by path). */}
          <span key={location.pathname} className="amp-mark amp-ignite" aria-hidden="true">
            <i className="bg-band-1" /><i className="bg-band-2" /><i className="bg-band-3" /><i className="bg-band-4" />
          </span>
          <span className="text-[17px] font-bold tracking-[0.16em] text-ink">AMP</span>
        </NavLink>

        <nav className="flex flex-col px-2 pb-4">
          <Group>Overview</Group>
          <NavLink to="/" end className={navCls}><Home size={14} />My work</NavLink>
          {remoteAvailable(me, remote) && <NavLink to="/sessions" className={navCls}><MonitorSmartphone size={14} />Sessions</NavLink>}
          <NavLink to="/projects" className={navCls}><FolderKanban size={14} />Projects
            <span className="ml-auto amp-num text-[11px] text-ink-3">{projects.length || ''}</span></NavLink>
          <NavLink to="/reports" className={navCls}><CalendarDays size={14} />Day reports</NavLink>
          <NavLink to="/trace" className={navCls}><GitCommitHorizontal size={14} />Trace</NavLink>
          <NavLink to="/guide" className={navCls}><GraduationCap size={14} />AMP guide</NavLink>
          {(me?.orgs?.length ?? 0) > 0 && <NavLink to="/portfolio" className={navCls}><PieChart size={14} />Portfolio</NavLink>}
          {me?.user_id != null && <NavLink to="/access" className={navCls}><KeyRound size={14} />Agents & tokens</NavLink>}

          {current && (
            <>
              <Group>Project</Group>
              <div className="flex items-center gap-2 px-2.5 pb-1.5">
                <span className="text-[13.5px] font-semibold text-ink truncate" title={current.name}>{current.name}</span>
                {current.code.length <= 8 && <span className="amp-num text-[10.5px] text-ink-3 border border-line px-1 py-px ml-auto shrink-0">{current.code}</span>}
              </div>
              {PROJECT_VIEWS.map(v => (
                <NavLink key={v.label} to={v.to(currentId!)} end={v.end} className={p => `${navCls(p)} pl-5`}>
                  {v.icon}{v.label}
                </NavLink>
              ))}
            </>
          )}

          {others.length > 0 && (
            <>
              <Group>{current ? 'Other projects' : 'Projects'}</Group>
              {others.map(p => (
                <NavLink key={p.id} to={`/project/${p.id}`} className={navCls} title={p.name}>
                  <span className="truncate">{p.name}</span>
                  {/* Long slugs crowd the name out of a narrow sidebar; only short codes earn the space. */}
                  {p.code.length <= 8 && <span className="amp-num text-[10.5px] text-ink-3 ml-auto shrink-0">{p.code}</span>}
                </NavLink>
              ))}
            </>
          )}

          {(isAdmin || orgAdmin) && (
            <>
              <Group>Admin</Group>
              {isAdmin && <NavLink to="/admin/users" className={navCls}><ShieldCheck size={14} />People & agents</NavLink>}
              <NavLink to="/admin/orgs" className={navCls}><Building2 size={14} />Teams & orgs</NavLink>
              {isAdmin && <NavLink to="/admin/audit" className={navCls}><ScrollText size={14} />Audit log</NavLink>}
            </>
          )}
          {!isAdmin && !orgAdmin && me && (
            <>
              <Group>Organization</Group>
              <NavLink to="/admin/orgs" className={navCls}><Building2 size={14} />Teams & orgs</NavLink>
              {isAdmin && <NavLink to="/admin/audit" className={navCls}><ScrollText size={14} />Audit log</NavLink>}
            </>
          )}
        </nav>

        {me && (
          <div className="mt-auto flex items-center gap-2.5 px-4 py-3 border-t border-line">
            <Avatar name={me.name || me.email} kind={me.kind} />
            <div className="min-w-0 flex-1">
              <div className="text-[13px] text-ink truncate">{me.name || me.email}</div>
              <div className="amp-num text-[10.5px] text-ink-3 truncate">{isAdmin ? 'admin' : me.kind === 'agent' ? 'agent' : orgRoleLabel} · {me.email}</div>
            </div>
            {/* Signed in through the browser: oauth2-proxy clears the session and sends you to sign in again. */}
            {me.user_id != null && !me.via_token && (
              <a href="/oauth2/sign_out?rd=%2F" title="Sign out" aria-label="Sign out"
                className="shrink-0 p-1.5 text-ink-3 hover:text-ink hover:bg-surface-3 transition-colors">
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
        </div>
        <Outlet />
      </main>
    </div>
  )
}
