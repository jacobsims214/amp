import { Loader2, X, AlertTriangle } from 'lucide-react'

interface ConfirmDeleteModalProps {
  title: string
  description: string
  onClose: () => void
  onConfirm: () => Promise<void>
  deleting?: boolean
  /** API error from the last attempt, shown inline */
  error?: string | null
}

export function ConfirmDeleteModal({ title, description, onClose, onConfirm, deleting = false, error = null }: ConfirmDeleteModalProps) {
  return (
    <>
      <div
        className="fixed inset-0 z-40"
        style={{ background: 'color-mix(in srgb, var(--ink) 38%, transparent)', backdropFilter: 'blur(4px)' }}
        onClick={onClose}
      />
      <div className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none">
        <div
          className="w-full max-w-md pointer-events-auto rounded-2xl"
          style={{
            background: 'var(--surface)',
            border: '1px solid var(--line)',
            boxShadow: '0 24px 64px color-mix(in srgb, var(--ink) 30%, transparent), 0 0 0 1px color-mix(in srgb, var(--danger) 8%, transparent)',
          }}
        >
          {/* Header */}
          <div className="flex items-center justify-between px-6 py-4 border-b border-line">
            <h2 className="text-sm font-semibold text-ink">{title}</h2>
            <button onClick={onClose} className="p-1.5 hover:bg-surface-3 rounded-lg transition-colors cursor-pointer">
              <X size={14} className="text-ink-2" />
            </button>
          </div>
          {/* Warning */}
          <div className="px-6 py-5">
            <div className="flex items-start gap-3 p-4 bg-danger/8 border border-danger/20 rounded-xl mb-4">
              <AlertTriangle size={15} className="text-danger-ink flex-shrink-0 mt-0.5" />
              <p className="text-xs text-danger-ink leading-relaxed">{description}</p>
            </div>
            <p className="text-xs text-ink-3">This action cannot be undone.</p>
            {error && <p className="text-xs text-danger-ink mt-3">{error}</p>}
          </div>
          {/* Footer */}
          <div className="flex items-center justify-end gap-2 px-6 py-4 border-t border-line">
            <button
              onClick={onClose}
              className="px-4 py-2 text-xs font-medium text-ink-2 hover:text-ink hover:bg-surface-3 rounded-lg transition-colors cursor-pointer"
            >
              Cancel
            </button>
            <button
              onClick={onConfirm}
              disabled={deleting}
              className="flex items-center gap-1.5 px-4 py-2 bg-danger hover:bg-danger disabled:opacity-50 disabled:cursor-not-allowed text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer"
            >
              {deleting && <Loader2 size={11} className="animate-spin" />}
              Delete
            </button>
          </div>
        </div>
      </div>
    </>
  )
}
