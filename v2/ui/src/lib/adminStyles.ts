// Class names and helpers shared by the admin screens (see components/admin.tsx).

export const fieldCls = 'amp-input'
export const smallSelectCls = 'amp-input !w-auto !py-1 !text-xs cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed'
export const primaryBtnCls = 'amp-btn amp-btn-primary'
export const quietBtnCls = 'amp-btn amp-btn-ghost amp-btn-sm'
export const dangerBtnCls = 'amp-btn amp-btn-ghost amp-btn-sm hover:!text-danger-ink'

export const errMsg = (e: unknown, fallback: string) => e instanceof Error ? e.message : fallback
