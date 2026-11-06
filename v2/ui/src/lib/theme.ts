// Theme preference: System (follow prefers-color-scheme), Light or Dark.
// The choice is kept in localStorage; `data-theme` on <html> is always the
// resolved value ('light' | 'dark'). public/theme-init.js applies the same
// rule before first paint, so keep the two in step.

export type ThemePref = 'system' | 'light' | 'dark'
export type ResolvedTheme = 'light' | 'dark'

export const THEME_KEY = 'amp.theme'
export const THEME_PREFS: ThemePref[] = ['system', 'light', 'dark']
export const THEME_COLOR: Record<ResolvedTheme, string> = { light: '#F2F2F0', dark: '#101215' }

export function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(THEME_KEY)
    if (v === 'light' || v === 'dark') return v
  } catch { /* storage blocked: follow the system */ }
  return 'system'
}

export function writePref(p: ThemePref): void {
  try {
    if (p === 'system') localStorage.removeItem(THEME_KEY)
    else localStorage.setItem(THEME_KEY, p)
  } catch { /* per-browser preference only */ }
}

export function systemPrefersDark(): boolean {
  return typeof window !== 'undefined' && !!window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches
}

export function resolve(p: ThemePref): ResolvedTheme {
  return p === 'dark' || (p === 'system' && systemPrefersDark()) ? 'dark' : 'light'
}

export function applyTheme(r: ResolvedTheme): void {
  document.documentElement.setAttribute('data-theme', r)
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', THEME_COLOR[r])
}
