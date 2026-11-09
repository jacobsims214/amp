import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useMatch } from 'react-router-dom'
import {
  Home, Menu, MonitorSmartphone, X, Building2, CalendarDays, FolderKanban, GitCommitHorizontal, GraduationCap, KeyRound, PieChart, ShieldCheck, ScrollText,
} from 'lucide-react'
import { api } from '../../api/client'
import { useAuth } from '../../hooks/useAuth'
import { remoteAvailable, useRemote } from '../../remote/remote-context'
import type { Project } from '../../types'
import { refreshWaitCounts, useWaitCounts } from '../../lib/waitCounts'
import { primeProjects } from '../../lib/projectCache'
import { badged, navCls } from './nav'
import { Badge } from './Badge'
import { ProjectTree } from './ProjectTree'
import { IdentityMenu } from './IdentityMenu'
import { ThemeToggle } from './ThemeToggle'
import { useThemeStore } from '../../lib/themeStore'

// The frame around every page: one sidebar to get anywhere, the page beside
// it. Projects are a tree (org > project > views) in the sidebar, so nothing
// needs a back button.

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
    api.listProjects()
      .then(list => { setProjects(list); primeProjects(list) })
      .catch(() => setProjects([]))
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

  // Initialize theme on first render (applies persisted/system preference).
  useThemeStore(state => state.init)

  return (
    <div className="relative flex h-screen overflow-hidden bg-bg">
      <a href="#main-content" className="amp-skip-link">Skip to main content</a>
      {navOpen && <div className="fixed inset-0 z-30 bg-[var(--scrim)] md:hidden" onClick={() => setNavOpen(false)} aria-hidden="true" />}
      <aside id="main-nav" className={`w-[232px] flex-shrink-0 flex-col bg-surface-2 border-r border-line overflow-y-auto fixed inset-y-0 left-0 z-40 md:static md:z-auto md:flex ${navOpen ? 'flex' : 'hidden'}`} aria-label="Main"
        onClick={e => { if ((e.target as HTMLElement).closest('a')) setNavOpen(false) }}>
        <button type="button" className="md:hidden absolute top-3 right-2 p-1.5 text-ink-3 hover:text-ink" onClick={() => setNavOpen(false)} aria-label="Close menu"><X size={16} /></button>
        <div className="flex items-center justify-between px-4 pt-4 pb-3">
          <NavLink to="/" className="flex items-center gap-2.5" aria-label="AMP home">
            {/* Relights on every page change (keyed by path). */}
            <span key={location.pathname} className="amp-mark amp-ignite" aria-hidden="true">
              <i className="bg-band-1" /><i className="bg-band-2" /><i className="bg-band-3" /><i className="bg-band-4" />
            </span>
            <span className="text-[17px] font-bold tracking-[0.16em] text-ink">AMP</span>
          </NavLink>
          <ThemeToggle />
        </div>

        <nav className="flex flex-col px-2 pb-4">
          <Group>Overview</Group>
          <NavLink to="/" end className={navCls} aria-label={badged('My work', wait?.counts.waiting)}><Home size={14} />My work<Badge n={wait?.counts.waiting} /></NavLink>
          {remoteAvailable(me, remote) && <NavLink to="/sessions" className={navCls}><MonitorSmartphone size={14} />Sessions</NavLink>}
          <NavLink to="/projects" className={navCls}><FolderKanban size={14} />Projects
            <span className="ml-auto amp-num text-[11px] text-ink-3">{projects.length || ''}</span></NavLink>
          <NavLink to="/reports" className={navCls}><CalendarDays size={14} />Day reports</NavLink>
          <NavLink to="/trace" className={navCls}><GitCommitHorizontal size={14} />Trace</NavLink>
          <NavLink to="/guide" className={navCls}><GraduationCap size={14} />AMP guide</NavLink>
          {(me?.orgs?.length ?? 0) > 0 && <NavLink to="/portfolio" className={navCls}><PieChart size={14} />Portfolio</NavLink>}
          {me?.user_id != null && <NavLink to="/access" className={navCls}><KeyRound size={14} />Agents & tokens</NavLink>}

          <ProjectTree projects={projects} orgs={me?.orgs ?? []} currentId={currentId} search={location.search} waitIn={waitIn} />

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

        {me && <IdentityMenu me={me} roleLabel={isAdmin ? 'admin' : me.kind === 'agent' ? 'agent' : orgRoleLabel} />}
      </aside>

      <main id="main-content" tabIndex={-1} className="relative flex-1 min-w-0 h-full overflow-auto">
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
