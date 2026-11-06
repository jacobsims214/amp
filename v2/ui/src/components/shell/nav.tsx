import {
  Activity, BookOpen, Inbox, Radar, Rows3, GitBranch, LayoutGrid, Settings, Shapes,
} from 'lucide-react'
import type { ProjectView } from '../../lib/projectLink'

// Pieces the sidebar's Overview links and its project tree share.

export const PROJECT_VIEWS: { view: ProjectView; label: string; icon: React.ReactNode; badge?: boolean }[] = [
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

export function navCls({ isActive }: { isActive: boolean }) {
  return [
    'group flex items-center gap-2.5 h-8 px-2.5 text-[13.5px] transition-colors',
    isActive
      ? 'bg-surface text-ink font-semibold shadow-[inset_3px_0_0_var(--accent),0_0_0_1px_var(--line)]'
      : 'text-ink-2 hover:bg-surface-3 hover:text-ink',
  ].join(' ')
}

/** The accessible name of a link with a Badge: "My work, 6 waiting on you". */
export function badged(label: string, n?: number) {
  return n ? `${label}, ${n} waiting on you` : undefined
}
