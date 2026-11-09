import { Sun, Moon, Monitor } from 'lucide-react'
import { useThemeStore } from '../../lib/themeStore'
import type { ThemeMode } from '../../lib/theme'

const MODES: { mode: ThemeMode; icon: React.ReactNode; label: string }[] = [
  { mode: 'light', icon: <Sun size={14} />, label: 'Light theme' },
  { mode: 'dark', icon: <Moon size={14} />, label: 'Dark theme' },
  { mode: 'system', icon: <Monitor size={14} />, label: 'System theme' },
]

export function ThemeToggle() {
  const { mode, setMode } = useThemeStore()
  return (
    <div
      className="inline-flex gap-0.5 p-0.5 border border-line rounded-sm bg-surface"
      role="group"
      aria-label="Theme"
    >
      {MODES.map(({ mode: m, icon, label }) => (
        <button
          key={m}
          type="button"
          aria-label={label}
          aria-pressed={mode === m}
          onClick={() => setMode(m)}
          className={[
            'inline-flex items-center justify-center w-7 h-6 rounded-sm transition-all',
            'text-ink-3 hover:text-ink hover:bg-surface-3 hover:scale-110',
            mode === m ? 'bg-ink text-on-ink hover:text-on-ink' : '',
          ].join(' ')}
        >
          {icon}
        </button>
      ))}
    </div>
  )
}
