import { Loader2, X, AlertTriangle } from 'lucide-react'

interface ConfirmDeleteModalProps {
  title: string
  description: string
  onClose: () => void
  onConfirm: () => Promise<void>
  deleting?: boolean
}

export function ConfirmDeleteModal({ title, description, onClose, onConfirm, deleting = false }: ConfirmDeleteModalProps) {
  return (
    <>
      <div className="fixed inset-0 bg-black/60 z-40" onClick={onClose} />
      <div className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none">
        <div className="bg-[#161b22] border border-[#30363d] rounded-lg shadow-2xl w-full max-w-md pointer-events-auto">
          {/* Header */}
          <div className="flex items-center justify-between px-5 py-4 border-b border-[#30363d]">
            <h2 className="text-sm font-semibold text-[#e6edf3]">{title}</h2>
            <button onClick={onClose} className="p-1 hover:bg-[#21262d] rounded transition-colors">
              <X size={14} className="text-[#8b949e]" />
            </button>
          </div>
          {/* Warning */}
          <div className="px-5 py-4">
            <div className="flex items-start gap-3 p-3 bg-[#f851491a] border border-[#f85149]/30 rounded-md mb-4">
              <AlertTriangle size={14} className="text-[#f85149] flex-shrink-0 mt-0.5" />
              <p className="text-xs text-[#f85149]">{description}</p>
            </div>
            <p className="text-xs text-[#8b949e]">This action cannot be undone.</p>
          </div>
          {/* Footer */}
          <div className="flex items-center justify-end gap-2 px-5 py-4 border-t border-[#30363d]">
            <button
              onClick={onClose}
              className="px-3 py-1.5 text-xs text-[#8b949e] hover:text-[#e6edf3] transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={onConfirm}
              disabled={deleting}
              className="flex items-center gap-1.5 px-3 py-1.5 bg-[#da3633] hover:bg-[#f85149] disabled:opacity-50 text-white text-xs font-medium rounded-md transition-colors"
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
