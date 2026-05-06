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
      <div
        className="fixed inset-0 z-40"
        style={{ background: 'rgba(4, 8, 18, 0.75)', backdropFilter: 'blur(4px)' }}
        onClick={onClose}
      />
      <div className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none">
        <div
          className="w-full max-w-md pointer-events-auto rounded-2xl"
          style={{
            background: '#0D1726',
            border: '1px solid #1E2C45',
            boxShadow: '0 24px 64px rgba(0,0,0,0.6), 0 0 0 1px rgba(239,68,68,0.08)',
          }}
        >
          {/* Header */}
          <div className="flex items-center justify-between px-6 py-4 border-b border-[#1E2C45]">
            <h2 className="text-sm font-semibold text-[#DDE6F0]">{title}</h2>
            <button onClick={onClose} className="p-1.5 hover:bg-[#172540] rounded-lg transition-colors cursor-pointer">
              <X size={14} className="text-[#7E91A8]" />
            </button>
          </div>
          {/* Warning */}
          <div className="px-6 py-5">
            <div className="flex items-start gap-3 p-4 bg-[#EF4444]/8 border border-[#EF4444]/20 rounded-xl mb-4">
              <AlertTriangle size={15} className="text-[#F87171] flex-shrink-0 mt-0.5" />
              <p className="text-xs text-[#F87171] leading-relaxed">{description}</p>
            </div>
            <p className="text-xs text-[#3D5068]">This action cannot be undone.</p>
          </div>
          {/* Footer */}
          <div className="flex items-center justify-end gap-2 px-6 py-4 border-t border-[#1E2C45]">
            <button
              onClick={onClose}
              className="px-4 py-2 text-xs font-medium text-[#7E91A8] hover:text-[#DDE6F0] hover:bg-[#172540] rounded-lg transition-colors cursor-pointer"
            >
              Cancel
            </button>
            <button
              onClick={onConfirm}
              disabled={deleting}
              className="flex items-center gap-1.5 px-4 py-2 bg-[#EF4444] hover:bg-[#DC2626] disabled:opacity-50 disabled:cursor-not-allowed text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer"
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
