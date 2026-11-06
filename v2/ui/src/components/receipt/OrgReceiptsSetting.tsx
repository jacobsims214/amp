import { useState } from 'react'
import { Loader2 } from 'lucide-react'
import { api } from '../../api/client'
import type { Org, ReceiptsMode } from '../../types'

const MODES: { value: ReceiptsMode; label: string; hint: string }[] = [
  { value: 'off', label: 'Off', hint: 'nothing is recorded or stored' },
  { value: 'inform', label: 'Inform', hint: 'receipts are shown, nothing waits' },
  { value: 'require', label: 'Require', hint: 'tickets wait for a receipt or a person\'s Accept' },
]

/**
 * Org settings: the verification receipts default for the org's projects
 * (#126). Off unless an org admin changes it; a project can override it in
 * its own settings. Shown to org admins; the API refuses anyone else and
 * anything but the web UI.
 */
export function OrgReceiptsSetting({ org, onSaved }: { org: Org; onSaved?: (o: Org) => void }) {
  const [mode, setMode] = useState<ReceiptsMode>(org.receipts_mode ?? 'off')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const change = async (next: ReceiptsMode) => {
    const prev = mode
    setMode(next); setSaving(true); setError(null)
    try {
      const o = await api.setOrgReceiptsMode(org.id, next)
      setMode(o.receipts_mode ?? next)
      onSaved?.(o)
    } catch (e) {
      setMode(prev)
      setError(e instanceof Error ? e.message : 'Could not change the setting')
    } finally {
      setSaving(false)
    }
  }
  return (
    <section className="space-y-2" data-testid="org-receipts-setting" aria-label="Verification receipts">
      <h3 className="m-0 text-[13px] font-semibold text-ink flex items-center gap-2">
        Verification receipts: the default for {org.name}'s projects
        {saving && <Loader2 size={12} className="animate-spin" aria-hidden="true" />}
      </h3>
      <p className="m-0 text-xs text-ink-2 leading-relaxed">A project's owners can choose differently in its settings.</p>
      <div className="grid gap-1.5" role="radiogroup" aria-label="Receipts default">
        {MODES.map(m => (
          <label key={m.value} className="flex items-center gap-2.5 text-[13px] text-ink cursor-pointer select-none">
            <input type="radio" name={`org-receipts-${org.id}`} className="accent-[var(--ink)]" checked={mode === m.value}
              disabled={saving} onChange={() => change(m.value)} />
            {m.label}<span className="text-ink-3">· {m.hint}</span>
          </label>
        ))}
      </div>
      {error && <p role="alert" className="m-0 text-[12px] text-danger-ink">{error}</p>}
    </section>
  )
}
