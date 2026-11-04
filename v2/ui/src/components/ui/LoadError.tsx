import { AlertTriangle, RotateCw } from 'lucide-react'

/**
 * A load that failed or timed out: what went wrong and a Retry. Used where a
 * page would otherwise sit on its loader or show a bare error string.
 */
export function LoadError({ message, onRetry, className = '' }: { message: string; onRetry: () => void; className?: string }) {
  return (
    <div role="alert" className={`amp-panel flex items-center gap-3 flex-wrap px-4 py-3 text-[13px] ${className}`}>
      <AlertTriangle size={14} className="text-danger-ink flex-shrink-0" aria-hidden="true" />
      <span className="text-danger-ink min-w-0 flex-1">{message}</span>
      <button type="button" onClick={onRetry} className="amp-btn">
        <RotateCw size={12} aria-hidden="true" />Retry
      </button>
    </div>
  )
}
