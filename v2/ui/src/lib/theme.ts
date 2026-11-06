// Theme state: light / dark / system, persisted to localStorage.
// The resolved theme is applied as `dark` class on <html> for Tailwind v4,
// and as `data-theme` for CSS custom-property overrides in index.css.

export type ThemeMode = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'amp-theme'

export function readSavedTheme(): ThemeMode {
  const raw = typeof window !== 'undefined' ? window.localStorage.getItem(STORAGE_KEY) : null
  if (raw === 'light' || raw === 'dark' || raw === 'system') return raw
  return 'system'
}

export function saveTheme(theme: ThemeMode): void {
  if (typeof window === 'undefined') return
  window.localStorage.setItem(STORAGE_KEY, theme)
}

export function resolveTheme(theme: ThemeMode): 'light' | 'dark' {
  if (theme === 'light') return 'light'
  if (theme === 'dark') return 'dark'
  if (typeof window === 'undefined') return 'light'
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

export function applyTheme(theme: ThemeMode): void {
  const resolved = resolveTheme(theme)
  const html = document.documentElement
  html.classList.toggle('dark', resolved === 'dark')
  html.dataset.theme = resolved
}
