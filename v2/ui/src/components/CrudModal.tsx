import { Loader2, X } from 'lucide-react'

interface CrudModalProps {
  title: string
  onClose: () => void
  /** omit for a read-only / immediate-action modal: the footer then shows only "Close" */
  onSave?: () => Promise<void> | void
  saving?: boolean
  saveLabel?: string
  children: React.ReactNode
}

export function CrudModal({ title, onClose, onSave, saving = false, saveLabel = 'Save', children }: CrudModalProps) {
  return (
    <>
      {/* Backdrop */}
      <div
        className="fixed inset-0 z-40"
        style={{ background: 'var(--scrim)' }}
        onClick={onClose}
      />
      {/* Panel */}
      <div className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none">
        <div
          className="w-full max-w-lg pointer-events-auto flex flex-col rounded-2xl shadow-2xl"
          style={{
            background: 'var(--surface)',
            border: '1px solid var(--line)',
            boxShadow: '0 12px 32px color-mix(in srgb, var(--ink) 14%, transparent), 0 0 0 1px color-mix(in srgb, var(--link) 8%, transparent)',
            maxHeight: '90vh',
          }}
        >
          {/* Header */}
          <div className="flex items-center justify-between px-6 py-4 border-b border-line">
            <h2 className="text-sm font-semibold text-ink tracking-tight">{title}</h2>
            <button
              onClick={onClose}
              className="p-1.5 rounded-lg hover:bg-surface-3 transition-colors cursor-pointer"
            >
              <X size={14} className="text-ink-2" />
            </button>
          </div>
          {/* Body */}
          <div className="flex-1 overflow-y-auto px-6 py-5 space-y-4">
            {children}
          </div>
          {/* Footer */}
          <div className="flex items-center justify-end gap-2 px-6 py-4 border-t border-line">
            <button
              onClick={onClose}
              className="px-4 py-2 text-xs font-medium text-ink-2 hover:text-ink hover:bg-surface-3 rounded-lg transition-colors cursor-pointer"
            >
              {onSave ? 'Cancel' : 'Close'}
            </button>
            {onSave && (
              <button
                onClick={onSave}
                disabled={saving}
                className="amp-btn amp-btn-primary"
              >
                {saving && <Loader2 size={11} className="animate-spin" />}
                {saveLabel}
              </button>
            )}
          </div>
        </div>
      </div>
    </>
  )
}

/* ─── Shared form field styles ────────────────────────────────────────────── */
export const inputCls = 'amp-input'

export const labelCls = 'amp-label block mb-2'
