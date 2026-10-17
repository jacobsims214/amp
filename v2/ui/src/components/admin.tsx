import { useState } from 'react'
import { Bot, Check, Copy, EyeOff } from 'lucide-react'
import { quietBtnCls } from '../lib/adminStyles'

// Small pieces shared by the admin screens: access, agents and tokens,
// webhooks, and the audit log.

const tones = {
  indigo: { background: 'rgba(99,102,241,0.12)', color: '#818CF8', border: '1px solid rgba(99,102,241,0.22)' },
  green: { background: 'rgba(16,185,129,0.12)', color: '#34D399', border: '1px solid rgba(16,185,129,0.22)' },
  red: { background: 'rgba(239,68,68,0.10)', color: '#F87171', border: '1px solid rgba(239,68,68,0.22)' },
  amber: { background: 'rgba(245,158,11,0.10)', color: '#FBBF24', border: '1px solid rgba(245,158,11,0.22)' },
  gray: { background: 'rgba(126,145,168,0.10)', color: '#7E91A8', border: '1px solid rgba(126,145,168,0.20)' },
}

export function Pill({ tone = 'indigo', title, children }: { tone?: keyof typeof tones; title?: string; children: React.ReactNode }) {
  return (
    <span title={title} className="inline-flex items-center gap-1 text-[10px] font-semibold px-1.5 py-0.5 rounded-md whitespace-nowrap" style={tones[tone]}>
      {children}
    </span>
  )
}

/** "agent" and "deactivated" markers next to a person's name. */
export function AccountBadges({ kind, disabled }: { kind?: string; disabled?: boolean }) {
  return (
    <>
      {kind === 'agent' && <Pill tone="indigo" title="An agent account: signs in with API tokens"><Bot size={9} />agent</Pill>}
      {disabled && <Pill tone="red" title="Deactivated: every request it makes is refused">deactivated</Pill>}
    </>
  )
}

/** "3m ago", "2h ago", "Oct 14" — with the full time on hover. */
export function When({ at }: { at?: string }) {
  if (!at) return <span className="text-[#3D5068]">never</span>
  return <RelativeTime at={at} />
}

function RelativeTime({ at }: { at: string }) {
  // The time this rendered; good enough for "3m ago" in an admin list.
  const [now] = useState(Date.now)
  const d = new Date(at)
  const secs = (now - d.getTime()) / 1000
  let label: string
  if (secs < 0) label = d.toLocaleString()
  else if (secs < 60) label = 'just now'
  else if (secs < 3600) label = `${Math.floor(secs / 60)}m ago`
  else if (secs < 86400) label = `${Math.floor(secs / 3600)}h ago`
  else label = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: secs > 300 * 86400 ? 'numeric' : undefined })
  return <span title={d.toLocaleString()}>{label}</span>
}

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button type="button" className={quietBtnCls}
      onClick={() => { navigator.clipboard?.writeText(text).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500) }) }}>
      {copied ? <Check size={12} className="text-[#34D399]" /> : <Copy size={12} />}{copied ? 'Copied' : label}
    </button>
  )
}

/**
 * A secret shown exactly once (a new API token, a webhook signing secret),
 * with a copy button and whatever else helps use it.
 */
export function SecretReveal({ title, secret, note = "Copy it now: AMP keeps only a hash, so it can't be shown again.", onDone, children }: {
  title: string
  secret: string
  note?: string
  onDone: () => void
  children?: React.ReactNode
}) {
  return (
    <div className="rounded-xl p-4 space-y-3" style={{ background: 'rgba(16,185,129,0.06)', border: '1px solid rgba(16,185,129,0.25)' }}>
      <div className="flex items-start gap-2">
        <EyeOff size={14} className="text-[#34D399] mt-0.5 flex-shrink-0" />
        <div className="flex-1 min-w-0">
          <p className="text-sm font-semibold text-[#DDE6F0]">{title}</p>
          <p className="text-xs text-[#7E91A8]">{note}</p>
        </div>
      </div>
      <div className="flex items-center gap-2">
        <code className="flex-1 min-w-0 px-3 py-2 rounded-lg text-xs font-mono text-[#DDE6F0] break-all" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>
          {secret}
        </code>
        <CopyButton text={secret} />
      </div>
      {children}
      <button type="button" onClick={onDone} className={quietBtnCls}>I've copied it</button>
    </div>
  )
}

export function Notice({ error, info }: { error?: string | null; info?: string | null }) {
  if (error) return <p className="text-xs text-[#F87171] mt-2">{error}</p>
  if (info) return <p className="text-xs text-[#34D399] mt-2">{info}</p>
  return null
}
