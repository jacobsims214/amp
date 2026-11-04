import { SearchX, X } from 'lucide-react'

/** `?focus=` named a ticket that doesn't exist or that the viewer can't see. */
export function FocusNotFound({ id, onDismiss }: { id: number; onDismiss: () => void }) {
  return (
    <div role="status" className="amp-panel flex items-center gap-3 px-4 py-3 text-[13px] text-ink-2">
      <SearchX size={14} className="text-ink-3 shrink-0" aria-hidden="true" />
      <span>Ticket <span className="amp-num text-ink">#{id}</span> was not found, or you can't see it.</span>
      <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost ml-auto" onClick={onDismiss}>
        <X size={11} />Dismiss
      </button>
    </div>
  )
}
