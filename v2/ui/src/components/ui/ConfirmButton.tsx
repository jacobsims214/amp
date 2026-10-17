import { useState } from 'react'

/** A button that asks once more, in place, before doing something hard to
 *  undo. (The app never uses browser confirm dialogs.) */
export function ConfirmButton({ label, confirmLabel, onConfirm, busy, danger, icon }: {
  label: string; confirmLabel: string; onConfirm: () => void; busy?: boolean; danger?: boolean; icon?: React.ReactNode
}) {
  const [armed, setArmed] = useState(false)
  if (!armed) {
    return <button type="button" className={`amp-btn amp-btn-sm ${danger ? 'amp-btn-danger' : ''}`} disabled={busy} onClick={() => setArmed(true)}>{icon}{label}</button>
  }
  return (
    <span className="inline-flex gap-1.5">
      <button type="button" className="amp-btn amp-btn-sm amp-btn-accent" disabled={busy} onClick={() => { setArmed(false); onConfirm() }}>{confirmLabel}</button>
      <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => setArmed(false)}>Cancel</button>
    </span>
  )
}
