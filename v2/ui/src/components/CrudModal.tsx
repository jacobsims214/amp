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
      <div
        className="fixed inset-0 z-40"
        style={{ background: 'rgba(4, 8, 18, 0.75)', backdropFilter: 'blur(4px)' }}
        onClick={onClose}
      />
      {/* Panel */}
      <div className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none">
        <div
          className="w-full max-w-lg pointer-events-auto flex flex-col rounded-2xl shadow-2xl"
          style={{
            background: '#0D1726',
            border: '1px solid #1E2C45',
            boxShadow: '0 24px 64px rgba(0,0,0,0.6), 0 0 0 1px rgba(99,102,241,0.08)',
            maxHeight: '90vh',
          }}
        >
          {/* Header */}
          <div className="flex items-center justify-between px-6 py-4 border-b border-[#1E2C45]">
            <h2 className="text-sm font-semibold text-[#DDE6F0] tracking-tight">{title}</h2>
            <button
              onClick={onClose}
              className="p-1.5 rounded-lg hover:bg-[#172540] transition-colors cursor-pointer"
            >
              <X size={14} className="text-[#7E91A8]" />
            </button>
          </div>
          {/* Body */}
          <div className="flex-1 overflow-y-auto px-6 py-5 space-y-4">
            {children}
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
              onClick={onSave}
              disabled={saving}
              className="flex items-center gap-1.5 px-4 py-2 bg-[#6366F1] hover:bg-[#4F46E5] disabled:opacity-50 disabled:cursor-not-allowed text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer"
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

/* ─── Shared form field styles ────────────────────────────────────────────── */
export const inputCls =
  'w-full bg-[#08101F] border border-[#1E2C45] rounded-lg px-3 py-2 text-sm text-[#DDE6F0] placeholder-[#3D5068] ' +
  'focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all'

export const labelCls = 'text-xs font-medium text-[#7E91A8] block mb-1.5'
