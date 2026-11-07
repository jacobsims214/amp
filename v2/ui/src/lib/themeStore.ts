import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import { applyTheme, resolveTheme, type ThemeMode } from './theme'

interface ThemeState {
  mode: ThemeMode
  resolved: 'light' | 'dark'
  setMode: (mode: ThemeMode) => void
  init: () => void
}

export const useThemeStore = create<ThemeState>()(
  persist(
    (set, get) => ({
      mode: 'system',
      resolved: 'light',
      setMode(mode) {
        set({ mode, resolved: resolveTheme(mode) })
        applyTheme(mode)
      },
      init() {
        const { mode } = get()
        set({ resolved: resolveTheme(mode) })
        applyTheme(mode)
      },
    }),
    {
      name: 'amp-theme',
      // Only persist the mode; resolved is computed on hydration.
      partialize: state => ({ mode: state.mode }),
      onRehydrateStorage: () => state => {
        if (state) state.init()
      },
    }
  )
)
