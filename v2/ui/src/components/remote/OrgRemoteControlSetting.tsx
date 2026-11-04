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

/** The idle limits an admin can pick, in minutes (0: none, the default). The API takes 5-240. */
const IDLE_OPTIONS = [0, 15, 30, 60, 120, 240]
/** The lifetimes an admin can pick, in hours (0: none, the default). The API takes 1-720. */
const LIFETIME_OPTIONS = [0, 4, 12, 24, 72, 168, 720]

function span(v: number, unit: 'min' | 'h'): string {
  if (unit === 'min') return v % 60 === 0 && v >= 60 ? `${v / 60} h` : `${v} min`
  return v % 24 === 0 && v >= 48 ? `${v / 24} days` : `${v} h`
}

/** What ends a session (#2424): only people, unless the admin added limits. */
function limitsText(p: OrgRemotePolicy): string {
  const idle = p.idle_timeout_min > 0 ? `after ${span(p.idle_timeout_min, 'min')} without remote activity` : null
  const life = (p.max_lifetime_hours ?? 0) > 0 ? `${span(p.max_lifetime_hours ?? 0, 'h')} after it started` : null
  if (!idle && !life) {
    return 'A remote session has no time limit: it reconnects by itself after sleep or a network change, and stays on until its owner ends it (/remote off or quitting the chat on the laptop, Disconnect or Revoke all on Sessions), this setting is turned off, or the owner loses access.'
  }
  const limits = [idle, life].filter(Boolean).join(', or ')
  return `A remote session ends ${limits}, or earlier when its owner ends it or this setting is turned off.`
}

function LimitSelect({ label, value, options, unit, disabled, onChange }: {
  label: string; value: number; options: number[]; unit: 'min' | 'h'; disabled: boolean; onChange: (v: number) => void
}) {
  // A value set through the API that isn't in the list still shows.
  const all = options.includes(value) ? options : [...options, value].sort((a, b) => a - b)
  return (
    <label className="flex flex-col gap-1 text-[12px] text-ink-2">
      {label}
      <select className="w-fit text-[13px] text-ink bg-transparent border border-line bg-surface rounded px-2 py-1" value={value}
        disabled={disabled} onChange={e => onChange(Number(e.target.value))}>
        {all.map(v => <option key={v} value={v}>{v === 0 ? 'No limit (default)' : span(v, unit)}</option>)}
      </select>
    </label>
  )
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

  const save = async (patch: Partial<OrgRemotePolicy>) => {
    setSaving(true); setError(null)
    try { setLoad({ kind: 'ready', policy: await setOrgRemotePolicy(orgId, patch) }) }
    catch (e) { setError(e instanceof RelayError && e.status === 403 ? 'Only org admins can change this setting.' : e instanceof Error ? e.message : 'Couldn\'t change the setting.') }
    finally { setSaving(false) }
  }
  const toggle = (enabled: boolean) => save({ enabled })

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
        <p className="m-0 text-[12px] text-ink-3 max-w-[640px]">
          {policy.enabled
            ? <>On. {limitsText(policy)} Paired browsers are trusted for <span className="amp-num">{policy.device_pin_days}</span> days. Turning this off ends every live remote session in the org within seconds.</>
            : 'Off (the default). Harnesses in this org can\'t start remote sessions.'}
        </p>
      )}
      {policy?.enabled && (
        <div className="flex flex-wrap gap-x-6 gap-y-2 pt-1">
          <LimitSelect label="End a session after no remote activity for" value={policy.idle_timeout_min}
            options={IDLE_OPTIONS} unit="min" disabled={saving}
            onChange={v => { void save({ idle_timeout_min: v }) }} />
          <LimitSelect label="End a session this long after it started" value={policy.max_lifetime_hours ?? 0}
            options={LIFETIME_OPTIONS} unit="h" disabled={saving || policy.max_lifetime_hours === undefined}
            onChange={v => { void save({ max_lifetime_hours: v }) }} />
        </div>
      )}
      {load.kind === 'unavailable' && <p className="m-0 text-[12.5px] text-ink-2">{load.message}</p>}
      {error && <p className="m-0 text-[12.5px] text-danger-ink" role="alert">{error}</p>}
    </section>
  )
}
