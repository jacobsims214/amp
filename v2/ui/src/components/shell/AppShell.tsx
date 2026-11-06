import { useEffect, useMemo, useState } from 'react'
import { NavLink, Outlet, useLocation, useMatch } from 'react-router-dom'
import { motion, useReducedMotion } from 'framer-motion'
import * as Tooltip from '@radix-ui/react-tooltip'
import {
  Activity, BookOpen, ChevronLeft, Home, Inbox, Menu, MonitorSmartphone, Radar, X, Rows3, Building2, CalendarDays, FolderKanban, GitBranch, GitCommitHorizontal, GraduationCap, KeyRound, LogOut, PieChart, LayoutGrid, Settings, Shapes, ShieldCheck, ScrollText,
} from 'lucide-react'
import { api } from '../../api/client'
import { useAuth } from '../../hooks/useAuth'
import { remoteAvailable, useRemote } from '../../remote/remote-context'
import type { Project } from '../../types'
import { refreshWaitCounts, useWaitCounts } from '../../lib/waitCounts'
import { Avatar } from '../ui/Avatar'
import { viewLink, type ProjectView } from '../../lib/projectLink'

// The frame around every page: one sidebar to get anywhere, the page beside
// it. The current project's views sit under it in the sidebar, so nothing
// needs a back button.

const SIDEBAR_WIDE = 232
const SIDEBAR_NARROW = 72

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

/** Icon-only nav items get the same dimensions so the tooltip anchor is square-ish. */
function iconNavCls({ isActive }: { isActive: boolean }) {
  return [
    'group flex items-center justify-center h-8 w-8 rounded-sm text-[13.5px] transition-colors mx-auto',
    isActive
      ? 'bg-surface text-ink font-semibold shadow-[inset_3px_0_0_var(--accent),0_0_0_1px_var(--line)]'
      : 'text-ink-2 hover:bg-surface-3 hover:text-ink',
  ].join(' ')
}

/** A "waiting on you" count: white on the accent, read as "N waiting on you". */
function Badge({ n }: { n?: number }) {
  if (!n) return null
  return <span className="ml-auto amp-num text-[11px] leading-none text-white bg-accent px-[5px] py-[2px] shrink-0" aria-hidden="true">{n}</span>
}


function Group({ children, collapsed }: { children: React.ReactNode; collapsed?: boolean }) {
  if (collapsed) return <div className="amp-label px-2.5 pt-5 pb-2 text-center">·</div>
  return <div className="amp-label px-2.5 pt-5 pb-2">{children}</div>
}

// A motion-enabled nav item. In collapsed mode it renders just the icon
// and wraps it in an accessible tooltip. It is declared outside AppShell so
// its React state is stable across renders.
function NavItem({ to, label, icon, badge, activeOnly = 'exact', title, className, onClick, children, collapsed, navOpen, shouldReduceMotion }: {
  to: string
  label: string
  icon: React.ReactNode
  badge?: number
  activeOnly?: 'exact' | 'partial'
  title?: string
  className?: string
  onClick?: () => void
  children?: React.ReactNode
  collapsed: boolean
  navOpen: boolean
  shouldReduceMotion: boolean
}) {
  const content = ({ isActive }: { isActive: boolean }) => (
    <motion.span
      className={`${collapsed && !navOpen ? iconNavCls({ isActive }) : navCls({ isActive })} ${className ?? ''}`}
      whileHover={shouldReduceMotion ? undefined : { scale: 1.02 }}
      whileTap={shouldReduceMotion ? undefined : { scale: 0.98 }}
      aria-label={badge ? `${label}, ${badge} waiting on you` : label}
    >
      <motion.span
        className="inline-flex"
        whileHover={shouldReduceMotion || !collapsed || navOpen ? undefined : { scale: 1.15 }}
        transition={{ type: 'spring', stiffness: 400, damping: 17 }}
      >
        {icon}
      </motion.span>
      {!(collapsed && !navOpen) && <span className="truncate">{label}</span>}
      {!(collapsed && !navOpen) && badge ? <Badge n={badge} /> : null}
      {!(collapsed && !navOpen) && children}
    </motion.span>
  )

  const link = activeOnly === 'exact' ? (
    <NavLink to={to} end onClick={onClick}>{content}</NavLink>
  ) : (
    <NavLink to={to} onClick={onClick}>{content}</NavLink>
  )

  if (!collapsed || navOpen) return link

  return (
    <Tooltip.Root>
      <Tooltip.Trigger asChild>{link}</Tooltip.Trigger>
      <Tooltip.Portal>
        <Tooltip.Content
          side="right"
          sideOffset={8}
          className="z-50 px-2 py-1 bg-ink text-on-ink text-[12px] rounded-sm shadow-[0_2px_8px_rgba(22,24,27,0.16)] select-none"
          aria-label={title || label}
        >
          {title || label}
          {badge ? <span className="ml-1.5 amp-num text-[11px] text-white/80">{badge}</span> : null}
          <Tooltip.Arrow className="fill-ink" width={10} height={5} />
        </Tooltip.Content>
      </Tooltip.Portal>
    </Tooltip.Root>
  )
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
  // Collapse to icon-only on desktop; mobile drawer stays full-width.
  const [collapsed, setCollapsed] = useState(() => {
    try { return localStorage.getItem('amp-sidebar-collapsed') === 'true' } catch { return false }
  })
  const shouldReduceMotion = useReducedMotion() ?? false
  const sidebarWidth = collapsed ? SIDEBAR_NARROW : SIDEBAR_WIDE

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

  const mainTransition = useMemo(() => ({
    width: { duration: shouldReduceMotion ? 0 : 0.25, ease: 'easeInOut' as const },
  }), [shouldReduceMotion])

  function toggleCollapsed() {
    const next = !collapsed
    setCollapsed(next)
    try { localStorage.setItem('amp-sidebar-collapsed', String(next)) } catch { /* storage unavailable */ }
  }

  const navItemProps = { collapsed, navOpen, shouldReduceMotion }
  return (
    <Tooltip.Provider delayDuration={300}>
      <div className="relative flex h-screen overflow-hidden bg-bg">
        {navOpen && <div className="fixed inset-0 z-30 bg-[var(--scrim)] md:hidden" onClick={() => setNavOpen(false)} aria-hidden="true" />}
        <motion.aside
          id="main-nav"
          initial={false}
          animate={{ width: navOpen ? SIDEBAR_WIDE : sidebarWidth }}
          transition={mainTransition}
          className={`flex-shrink-0 flex-col bg-surface-2 border-r border-line overflow-y-auto overflow-x-hidden fixed inset-y-0 left-0 z-40 md:static md:z-auto md:flex ${navOpen ? 'flex' : 'hidden'}`}
          aria-label="Main"
          onClick={e => { if ((e.target as HTMLElement).closest('a')) setNavOpen(false) }}
        >
          <button type="button" className="md:hidden absolute top-3 right-2 p-1.5 text-ink-3 hover:text-ink" onClick={() => setNavOpen(false)} aria-label="Close menu"><X size={16} /></button>
          <NavLink to="/" className={`flex items-center gap-2.5 px-4 pt-4 pb-3 ${collapsed && !navOpen ? 'justify-center' : ''}`} aria-label="AMP home">
            {/* Relights on every page change (keyed by path). */}
            <span key={location.pathname} className="amp-mark amp-ignite" aria-hidden="true">
              <i className="bg-band-1" /><i className="bg-band-2" /><i className="bg-band-3" /><i className="bg-band-4" />
            </span>
            <span className={`text-[17px] font-bold tracking-[0.16em] text-ink ${collapsed && !navOpen ? 'hidden' : ''}`}>AMP</span>
          </NavLink>

          <nav className="flex flex-col px-2 pb-4">
            <Group collapsed={collapsed && !navOpen}>Overview</Group>
            <NavItem {...navItemProps} to="/" label="My work" icon={<Home size={14} />} badge={wait?.counts.waiting} activeOnly="exact" onClick={() => setNavOpen(false)} />
            {remoteAvailable(me, remote) && <NavItem {...navItemProps} to="/sessions" label="Sessions" icon={<MonitorSmartphone size={14} />} onClick={() => setNavOpen(false)} />}
            <NavItem {...navItemProps} to="/projects" label="Projects" icon={<FolderKanban size={14} />} activeOnly="partial" onClick={() => setNavOpen(false)}>
              <span className="ml-auto amp-num text-[11px] text-ink-3">{projects.length || ''}</span>
            </NavItem>
            <NavItem {...navItemProps} to="/reports" label="Day reports" icon={<CalendarDays size={14} />} onClick={() => setNavOpen(false)} />
            <NavItem {...navItemProps} to="/trace" label="Trace" icon={<GitCommitHorizontal size={14} />} onClick={() => setNavOpen(false)} />
            <NavItem {...navItemProps} to="/guide" label="AMP guide" icon={<GraduationCap size={14} />} onClick={() => setNavOpen(false)} />
            {(me?.orgs?.length ?? 0) > 0 && <NavItem {...navItemProps} to="/portfolio" label="Portfolio" icon={<PieChart size={14} />} onClick={() => setNavOpen(false)} />}
            {me?.user_id != null && <NavItem {...navItemProps} to="/access" label="Agents & tokens" icon={<KeyRound size={14} />} onClick={() => setNavOpen(false)} />}

            {current && (
              <>
                <Group collapsed={collapsed && !navOpen}>Project</Group>
                {!(collapsed && !navOpen) && (
                  <div className="flex items-center gap-2 px-2.5 pb-1.5">
                    <span className="text-[13.5px] font-semibold text-ink truncate" title={current.name}>{current.name}</span>
                    {current.code.length <= 8 && <span className="amp-num text-[10.5px] text-ink-3 border border-line px-1 py-px ml-auto shrink-0">{current.code}</span>}
                  </div>
                )}
                {PROJECT_VIEWS.map(v => (
                  <NavItem
                    {...navItemProps}
                    key={v.label}
                    to={viewLink(currentId!, v.view, location.search)}
                    label={v.label}
                    icon={v.icon}
                    badge={v.badge ? waitIn(currentId!) : undefined}
                    activeOnly="partial"
                    className="pl-5"
                    onClick={() => setNavOpen(false)}
                  />
                ))}
              </>
            )}

            {others.length > 0 && (
              <>
                <Group collapsed={collapsed && !navOpen}>{current ? 'Other projects' : 'Projects'}</Group>
                {others.map(p => (
                  <NavItem
                    {...navItemProps}
                    key={p.id}
                    to={`/project/${p.id}`}
                    label={p.name}
                    title={p.name}
                    icon={<span className="w-3.5 h-3.5 rounded-full bg-surface-3 border border-line flex items-center justify-center amp-num text-[9px]">{p.code.slice(0, 2).toUpperCase()}</span>}
                    badge={waitIn(p.id) || undefined}
                    activeOnly="partial"
                    onClick={() => setNavOpen(false)}
                  />
                ))}
              </>
            )}

            {(isAdmin || orgAdmin) && (
              <>
                <Group collapsed={collapsed && !navOpen}>Admin</Group>
                {isAdmin && <NavItem {...navItemProps} to="/admin/users" label="People & agents" icon={<ShieldCheck size={14} />} onClick={() => setNavOpen(false)} />}
                <NavItem {...navItemProps} to="/admin/orgs" label="Teams & orgs" icon={<Building2 size={14} />} onClick={() => setNavOpen(false)} />
                {isAdmin && <NavItem {...navItemProps} to="/admin/audit" label="Audit log" icon={<ScrollText size={14} />} onClick={() => setNavOpen(false)} />}
              </>
            )}
            {!isAdmin && !orgAdmin && me && (
              <>
                <Group collapsed={collapsed && !navOpen}>Organization</Group>
                <NavItem {...navItemProps} to="/admin/orgs" label="Teams & orgs" icon={<Building2 size={14} />} onClick={() => setNavOpen(false)} />
                {isAdmin && <NavItem {...navItemProps} to="/admin/audit" label="Audit log" icon={<ScrollText size={14} />} onClick={() => setNavOpen(false)} />}
              </>
            )}
          </nav>

          {/* Collapse/expand toggle — only on desktop where the persistent sidebar lives. */}
          <div className="hidden md:flex mt-auto items-center justify-end px-2 py-2 border-t border-line">
            <button
              type="button"
              onClick={toggleCollapsed}
              aria-expanded={!collapsed}
              aria-controls="main-nav"
              aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
              className="group flex items-center justify-center h-7 w-7 text-ink-3 hover:text-ink hover:bg-surface-3 transition-colors rounded-sm"
            >
              <motion.span
                animate={{ rotate: collapsed ? 180 : 0 }}
                transition={{ duration: shouldReduceMotion ? 0 : 0.25, ease: [0.4, 0, 0.2, 1] }}
              >
                <ChevronLeft size={16} />
              </motion.span>
            </button>
          </div>

          {me && (
            <div className={`mt-auto flex items-center gap-2.5 px-4 py-3 border-t border-line ${collapsed && !navOpen ? 'flex-col px-2' : ''}`}>
              <Avatar name={me.name || me.email} kind={me.kind} />
              <div className={`min-w-0 flex-1 ${collapsed && !navOpen ? 'hidden' : ''}`}>
                <div className="text-[13px] text-ink truncate">{me.name || me.email}</div>
                <div className="amp-num text-[10.5px] text-ink-3 truncate">{isAdmin ? 'admin' : me.kind === 'agent' ? 'agent' : orgRoleLabel} · {me.email}</div>
              </div>
              {/* Signed in through the browser: oauth2-proxy clears the session and sends you to sign in again. */}
              {me.user_id != null && !me.via_token && (
                <Tooltip.Root>
                  <Tooltip.Trigger asChild>
                    <a href="/oauth2/sign_out?rd=%2F" title="Sign out" aria-label="Sign out"
                      className="shrink-0 p-1.5 text-ink-3 hover:text-ink hover:bg-surface-3 transition-colors rounded-sm">
                      <motion.span whileHover={shouldReduceMotion ? undefined : { scale: 1.1 }} whileTap={shouldReduceMotion ? undefined : { scale: 0.95 }}>
                        <LogOut size={14} />
                      </motion.span>
                    </a>
                  </Tooltip.Trigger>
                  <Tooltip.Portal>
                    <Tooltip.Content side="top" sideOffset={6} className="z-50 px-2 py-1 bg-ink text-on-ink text-[12px] rounded-sm shadow-[0_2px_8px_rgba(22,24,27,0.16)] select-none">
                      Sign out
                      <Tooltip.Arrow className="fill-ink" width={10} height={5} />
                    </Tooltip.Content>
                  </Tooltip.Portal>
                </Tooltip.Root>
              )}
            </div>
          )}
        </motion.aside>

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
    </Tooltip.Provider>
  )
}
