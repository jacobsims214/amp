import { Loader2, X } from 'lucide-react'

interface CrudModalProps {
  title: string
  onClose: () => void
  onSave: () => Promise<void>
  saving?: boolean
  children: React.ReactNode
}

export function CrudModal({ title, onClose, onSave, saving = false, children }: CrudModalProps) {
  return (
    <>
      {/* Backdrop */}
      <div className="fixed inset-0 bg-black/60 z-40" onClick={onClose} />
      {/* Panel */}
      <div className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none">
        <div className="bg-[#161b22] border border-[#30363d] rounded-lg shadow-2xl w-full max-w-lg pointer-events-auto flex flex-col max-h-[90vh]">
          {/* Header */}
          <div className="flex items-center justify-between px-5 py-4 border-b border-[#30363d]">
            <h2 className="text-sm font-semibold text-[#e6edf3]">{title}</h2>
            <button onClick={onClose} className="p-1 hover:bg-[#21262d] rounded transition-colors">
              <X size={14} className="text-[#8b949e]" />
            </button>
          </div>
          {/* Body */}
          <div className="flex-1 overflow-y-auto px-5 py-4 space-y-4">
            {children}
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
              onClick={onSave}
              disabled={saving}
              className="flex items-center gap-1.5 px-3 py-1.5 bg-[#238636] hover:bg-[#2ea043] disabled:opacity-50 text-white text-xs font-medium rounded-md transition-colors"
            >
              {saving && <Loader2 size={11} className="animate-spin" />}
              Save
            </button>
          </div>
        </div>
      </div>
    </>
  )
}
