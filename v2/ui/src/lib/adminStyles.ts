// Class names and helpers shared by the admin screens (see components/admin.tsx).

export const fieldCls =
  'px-3 py-2 rounded-lg text-sm bg-[#08101F] border border-[#1E2C45] text-[#DDE6F0] placeholder-[#3D5068] outline-none focus:border-[#6366F1]'
export const smallSelectCls =
  'bg-[#08101F] border border-[#1E2C45] rounded-lg px-2 py-1 text-xs text-[#DDE6F0] focus:outline-none focus:border-[#6366F1] cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed'
export const primaryBtnCls =
  'flex items-center gap-1.5 px-3 py-2 rounded-lg text-xs font-semibold text-white bg-[#6366F1] hover:bg-[#4F46E5] disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer transition-all active:scale-[0.97]'
export const quietBtnCls =
  'flex items-center gap-1 px-2 py-1 rounded-lg text-xs font-medium text-[#7E91A8] hover:text-[#DDE6F0] hover:bg-[#172540] disabled:opacity-50 cursor-pointer transition-colors'
export const dangerBtnCls =
  'flex items-center gap-1 px-2 py-1 rounded-lg text-xs font-medium text-[#7E91A8] hover:text-[#F87171] hover:bg-[#EF4444]/10 disabled:opacity-50 cursor-pointer transition-colors'

export const errMsg = (e: unknown, fallback: string) => e instanceof Error ? e.message : fallback
