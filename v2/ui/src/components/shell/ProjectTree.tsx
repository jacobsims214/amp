import { useEffect, useMemo, useState } from 'react'
import { NavLink } from 'react-router-dom'
import { ChevronDown, ChevronRight, ChevronUp, Pin, PinOff } from 'lucide-react'
import type { Org, Project } from '../../types'
import { viewLink } from '../../lib/projectLink'
import {
  loadPrefs, moveInOrder, savePrefs, sortByOrder, type SidebarPrefs,
} from '../../lib/sidebarPrefs'
import { badged, navCls, PROJECT_VIEWS } from './nav'
import { Badge } from './Badge'

// The sidebar's projects: pinned ones first, then one collapsible group per
// org, each project expanding to its views. The current project (and its org)
// is always open so you never lose your place. Order, pins and what you
// collapsed persist in localStorage (lib/sidebarPrefs.ts).

const NO_ORG = 'org:none'
const FILTER_FROM = 6 // a type-to-filter box once the list is long enough to need one

interface Props {
  projects: Project[]
  orgs: Pick<Org, 'id' | 'name'>[]
  currentId?: string
  search: string
  waitIn: (id: number | string) => number | undefined
}

const iconBtn = 'shrink-0 p-1 text-ink-3 hover:text-ink hover:bg-surface-3 disabled:opacity-30 disabled:hover:bg-transparent focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent'

export function ProjectTree({ projects, orgs, currentId, search, waitIn }: Props) {
  const [prefs, setPrefs] = useState<SidebarPrefs>(loadPrefs)
  const [q, setQ] = useState('')
  useEffect(() => { savePrefs(prefs) }, [prefs])
  const update = (f: (p: SidebarPrefs) => SidebarPrefs) => setPrefs(f)

  const needle = q.trim().toLowerCase()
  const ordered = useMemo(() => sortByOrder(projects, prefs.order), [projects, prefs.order])
  const visible = needle
    ? ordered.filter(p => p.name.toLowerCase().includes(needle) || p.code.toLowerCase().includes(needle))
    : ordered
  const pinnedList = prefs.pinned
    .map(id => visible.find(p => p.id === id))
    .filter((p): p is Project => !!p)
  const pinnedIds = new Set(pinnedList.map(p => p.id))

  // Groups: one per org_id among the visible projects, named from the orgs we
  // know (me.orgs only lists memberships, so a shared or admin-visible project's
  // org is named by the project's org_name, else "Org #id"); then projects with no org at all.
  const groups = useMemo(() => {
    const names = new Map(orgs.map(o => [o.id, o.name]))
    for (const p of visible) if (p.org_id != null && p.org_name && !names.has(p.org_id)) names.set(p.org_id, p.org_name)
    const ids: number[] = orgs.map(o => o.id)
    for (const p of visible) if (p.org_id != null && !ids.includes(p.org_id)) ids.push(p.org_id)
    const gs = ids.map(id => ({ key: `org:${id}`, name: names.get(id) ?? `Org #${id}`, items: visible.filter(p => p.org_id === id && !pinnedIds.has(p.id)) }))
    const rest = visible.filter(p => p.org_id == null && !pinnedIds.has(p.id))
    gs.push({ key: NO_ORG, name: ids.length ? 'No org' : 'Projects', items: rest })
    return gs.filter(g => g.items.length > 0)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgs, visible, prefs.pinned])

  const togglePin = (id: number) => update(p => ({ ...p, pinned: p.pinned.includes(id) ? p.pinned.filter(x => x !== id) : [...p.pinned, id] }))
  const move = (list: Project[], id: number, dir: -1 | 1) => update(p => {
    const ids = moveInOrder(list.map(x => x.id), id, dir)
    if (list === pinnedList) return { ...p, pinned: ids }
    // Reorder within the group; keep every other project where it was.
    const rest = sortByOrder(projects, p.order).map(x => x.id).filter(x => !ids.includes(x))
    return { ...p, order: [...ids, ...rest] }
  })

  const isOrgOpen = (key: string, hasCurrent: boolean) => needle !== '' || hasCurrent || !prefs.collapsedOrgs.includes(key)
  const isProjOpen = (id: number) => String(id) === currentId || (needle === '' && prefs.openProjects.includes(id))
  const toggleOrg = (key: string) => update(p => ({ ...p, collapsedOrgs: p.collapsedOrgs.includes(key) ? p.collapsedOrgs.filter(k => k !== key) : [...p.collapsedOrgs, key] }))
  const toggleProj = (id: number) => update(p => ({ ...p, openProjects: p.openProjects.includes(id) ? p.openProjects.filter(k => k !== id) : [...p.openProjects, id] }))

  const row = (p: Project, list: Project[], pinned: boolean) => {
    const open = isProjOpen(p.id)
    const isCurrent = String(p.id) === currentId
    const n = waitIn(p.id)
    const i = list.indexOf(p)
    const panel = `proj-views-${p.id}`
    return (
      <li key={p.id}>
        <div className="group/row flex items-center">
          <button type="button" className={iconBtn} aria-expanded={open} aria-controls={panel}
            aria-label={`${open ? 'Collapse' : 'Expand'} ${p.name}`} disabled={isCurrent}
            onClick={() => toggleProj(p.id)}>
            {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
          </button>
          <NavLink to={`/project/${p.id}`} end className={a => `${navCls(a)} flex-1 min-w-0 pl-1`} title={p.name} aria-label={badged(p.name, n)}>
            <span className="truncate">{p.name}</span>
            {/* Something waiting beats the code; long slugs crowd the name out of a narrow sidebar. */}
            {n ? <Badge n={n} /> : p.code.length <= 8 && <span className="amp-num text-[10.5px] text-ink-3 ml-auto shrink-0">{p.code}</span>}
          </NavLink>
          <div className="flex items-center opacity-0 group-hover/row:opacity-100 focus-within:opacity-100 max-md:opacity-100">
            <button type="button" className={iconBtn} aria-label={`Move ${p.name} up`} disabled={i <= 0 || needle !== ''} onClick={() => move(list, p.id, -1)}><ChevronUp size={12} /></button>
            <button type="button" className={iconBtn} aria-label={`Move ${p.name} down`} disabled={i >= list.length - 1 || needle !== ''} onClick={() => move(list, p.id, 1)}><ChevronDown size={12} /></button>
            <button type="button" className={iconBtn} aria-pressed={pinned} aria-label={`${pinned ? 'Unpin' : 'Pin'} ${p.name}`} onClick={() => togglePin(p.id)}>
              {pinned ? <PinOff size={12} /> : <Pin size={12} />}
            </button>
          </div>
        </div>
        {open && (
          <ul id={panel} className="flex flex-col">
            {PROJECT_VIEWS.map(v => (
              <li key={v.label}>
                <NavLink to={viewLink(p.id, v.view, isCurrent ? search : '')} className={a => `${navCls(a)} pl-7`} aria-label={v.badge ? badged(v.label, n) : undefined}>
                  {v.icon}{v.label}{v.badge && <Badge n={n} />}
                </NavLink>
              </li>
            ))}
          </ul>
        )}
      </li>
    )
  }

  if (projects.length === 0) return null
  return (
    <>
      <div className="amp-label px-2.5 pt-5 pb-2">Projects</div>
      {projects.length >= FILTER_FROM && (
        <input type="search" value={q} onChange={e => setQ(e.target.value)} placeholder="Filter projects"
          aria-label="Filter projects" className="mx-2.5 mb-1.5 h-7 px-2 text-[12.5px] bg-surface border border-line text-ink placeholder:text-ink-3" />
      )}
      {needle !== '' && visible.length === 0 && <div className="px-2.5 py-1 text-[12px] text-ink-3">No project matches.</div>}
      <div className="max-h-[60vh] overflow-y-auto">
        {pinnedList.length > 0 && (
          <section aria-label="Pinned projects">
            <div className="amp-num text-[10.5px] uppercase tracking-wider text-ink-3 px-2.5 pt-1 pb-1">Pinned</div>
            <ul>{pinnedList.map(p => row(p, pinnedList, true))}</ul>
          </section>
        )}
        {groups.map(g => {
          const hasCurrent = g.items.some(p => String(p.id) === currentId)
          const open = isOrgOpen(g.key, hasCurrent)
          const total = g.items.reduce((s, p) => s + (waitIn(p.id) ?? 0), 0)
          const id = `grp-${g.key.replace(':', '-')}`
          return (
            <section key={g.key} aria-label={g.name}>
              <button type="button" aria-expanded={open} aria-controls={id} disabled={hasCurrent && needle === ''}
                aria-label={`${open ? 'Collapse' : 'Expand'} ${g.name}${total ? `, ${total} waiting on you` : ''}`}
                onClick={() => toggleOrg(g.key)}
                className="flex w-full items-center gap-1.5 h-7 px-2.5 text-left amp-num text-[10.5px] uppercase tracking-wider text-ink-3 hover:text-ink">
                {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
                <span className="truncate">{g.name}</span>
                {!open && <Badge n={total} />}
              </button>
              {open && <ul id={id}>{g.items.map(p => row(p, g.items, false))}</ul>}
            </section>
          )
        })}
      </div>
    </>
  )
}
