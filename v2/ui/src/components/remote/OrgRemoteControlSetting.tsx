/**
 * Org settings: "Allow remote control of harness sessions" (protocol 8.3, open question 4:
 * off by default). Shown to org admins only; the API decides who may change it.
 */
import { useEffect, useState } from 'react'
import { Loader2 } from 'lucide-react'
import { getOrgRemotePolicy, setOrgRemotePolicy, type OrgRemotePolicy } from '../../remote/orgPolicy'
import { RelayError } from '../../remote/transport'

type Load = { kind: 'loading' } | { kind: 'ready'; policy: OrgRemotePolicy } | { kind: 'unavailable'; message: string }

function loadProblem(e: unknown): string {
  if (e instanceof RelayError && e.status === 404) return 'This AMP server doesn\'t offer remote control yet.'
  if (e instanceof RelayError && e.status === 403) return 'Only org admins can see or change this setting.'
  return e instanceof Error ? e.message : 'Couldn\'t load the setting.'
}

export function OrgRemoteControlSetting({ orgId }: { orgId: number }) {
  const [load, setLoad] = useState<Load>({ kind: 'loading' })
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setLoad({ kind: 'loading' })
    getOrgRemotePolicy(orgId).then(
      policy => { if (!cancelled) setLoad({ kind: 'ready', policy }) },
      e => { if (!cancelled) setLoad({ kind: 'unavailable', message: loadProblem(e) }) },
    )
    return () => { cancelled = true }
  }, [orgId])

  const toggle = async (enabled: boolean) => {
    setSaving(true); setError(null)
    try { setLoad({ kind: 'ready', policy: await setOrgRemotePolicy(orgId, { enabled }) }) }
    catch (e) { setError(e instanceof RelayError && e.status === 403 ? 'Only org admins can change this setting.' : e instanceof Error ? e.message : 'Couldn\'t change the setting.') }
    finally { setSaving(false) }
  }

  const policy = load.kind === 'ready' ? load.policy : null
  return (
    <section className="space-y-2" aria-labelledby={`remote-control-${orgId}`}>
      <h3 id={`remote-control-${orgId}`} className="amp-label m-0">Remote control</h3>
      <p className="m-0 text-xs text-ink-2 leading-relaxed max-w-[640px]">
        Lets members drive a harness session running on their own laptop from the AMP web app or a phone: chat, Stop and status.
        The laptop does the work and must be paired with each browser. AMP only relays signed messages and keeps no transcript.
      </p>
      <label className="flex items-center gap-3 text-[13px] text-ink cursor-pointer select-none w-fit">
        <input type="checkbox" className="w-4 h-4 accent-[var(--ink)]" disabled={!policy || saving}
          checked={policy?.enabled ?? false} onChange={e => { void toggle(e.target.checked) }} />
        Allow remote control of harness sessions
        {(load.kind === 'loading' || saving) && <Loader2 size={12} className="animate-spin text-ink-3" aria-label="Saving" />}
      </label>
      {policy && (
        <p className="m-0 text-[12px] text-ink-3">
          {policy.enabled
            ? <>On. Sessions end after <span className="amp-num">{policy.idle_timeout_min}</span> minutes without remote activity; paired browsers are trusted for <span className="amp-num">{policy.device_pin_days}</span> days. Turning this off ends every live remote session in the org within seconds.</>
            : 'Off (the default). Harnesses in this org can\'t start remote sessions.'}
        </p>
      )}
      {load.kind === 'unavailable' && <p className="m-0 text-[12.5px] text-ink-2">{load.message}</p>}
      {error && <p className="m-0 text-[12.5px] text-danger-ink" role="alert">{error}</p>}
    </section>
  )
}
