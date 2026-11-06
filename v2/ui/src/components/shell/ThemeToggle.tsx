import { Monitor, Moon, Sun } from 'lucide-react'
import { useTheme } from '../../hooks/useTheme'
import { THEME_PREFS, type ThemePref } from '../../lib/theme'

const OPTIONS: Record<ThemePref, { label: string; icon: React.ReactNode }> = {
  system: { label: 'System', icon: <Monitor size={13} /> },
  light: { label: 'Light', icon: <Sun size={13} /> },
  dark: { label: 'Dark', icon: <Moon size={13} /> },
}

/** System / Light / Dark, as a radio group. Lives in the sidebar footer. */
export function ThemeToggle() {
  const { pref, setPref } = useTheme()
  return (
    <div role="radiogroup" aria-label="Theme" className="flex border border-line">
      {THEME_PREFS.map(p => {
        const on = pref === p
        return (
          <button key={p} type="button" role="radio" aria-checked={on} aria-label={OPTIONS[p].label} title={OPTIONS[p].label}
            onClick={() => setPref(p)}
            className={`flex-1 flex items-center justify-center gap-1 py-1.5 text-[11px] cursor-pointer transition-colors ${on ? 'bg-surface-3 text-ink font-semibold' : 'text-ink-2 hover:text-ink hover:bg-surface-3'}`}>
            {OPTIONS[p].icon}<span className="amp-num">{OPTIONS[p].label}</span>
          </button>
        )
      })}
    </div>
  )
}
