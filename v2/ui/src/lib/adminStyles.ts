// Class names and helpers shared by the admin screens (see components/admin.tsx).

export const fieldCls =
  'px-3 py-2 rounded-lg text-sm bg-bg border border-line text-ink placeholder-ink-3 outline-none focus:border-primary'
export const smallSelectCls =
  'bg-bg border border-line rounded-lg px-2 py-1 text-xs text-ink focus:outline-none focus:border-primary cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed'
export const primaryBtnCls =
  'flex items-center gap-1.5 px-3 py-2 rounded-lg text-xs font-semibold text-white bg-primary hover:bg-primary disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer transition-all active:scale-[0.97]'
export const quietBtnCls =
  'flex items-center gap-1 px-2 py-1 rounded-lg text-xs font-medium text-ink-2 hover:text-ink hover:bg-surface-3 disabled:opacity-50 cursor-pointer transition-colors'
export const dangerBtnCls =
  'flex items-center gap-1 px-2 py-1 rounded-lg text-xs font-medium text-ink-2 hover:text-danger-ink hover:bg-danger/10 disabled:opacity-50 cursor-pointer transition-colors'

export const errMsg = (e: unknown, fallback: string) => e instanceof Error ? e.message : fallback
