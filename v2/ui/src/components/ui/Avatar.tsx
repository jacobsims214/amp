import { initials } from '../../lib/people'
import type { UserKind } from '../../types'

// People are black circles; agents are outlined hexagons, so who did
// something reads at a glance anywhere in AMP.

export function Avatar({ name, kind, size = 22, title }: { name: string; kind?: UserKind | 'agent' | 'human'; size?: number; title?: string }) {
  const agent = kind === 'agent'
  const font = Math.max(8, Math.round(size * 0.42))
  if (agent) {
    return (
      <span className="relative inline-grid place-items-center flex-shrink-0 select-none" style={{ width: size, height: size }} title={title ?? `${name} (agent)`}>
        <svg viewBox="0 0 24 24" width={size} height={size} className="absolute inset-0" aria-hidden="true">
          <polygon points="6,1.5 18,1.5 23,12 18,22.5 6,22.5 1,12" fill="var(--surface)" stroke="var(--ink)" strokeWidth="1.8" />
        </svg>
        <span className="relative font-label font-bold text-ink" style={{ fontSize: font, letterSpacing: '0.02em' }}>{agentInitials(name)}</span>
      </span>
    )
  }
  return (
    <span className="inline-grid place-items-center rounded-full bg-ink text-on-ink font-label font-bold flex-shrink-0 select-none"
      style={{ width: size, height: size, fontSize: font, letterSpacing: '0.02em' }} title={title ?? name}>
      {initials(name)}
    </span>
  )
}

/** "claude-build-1" → "C1", "codex-data" → "CD". */
export function agentInitials(name: string): string {
  const parts = name.split(/[\s_-]+/).filter(Boolean)
  if (parts.length === 0) return '?'
  const last = parts[parts.length - 1]
  if (parts.length > 1 && /^\d+$/.test(last)) return (parts[0][0] + last).toUpperCase().slice(0, 3)
  return (parts[0][0] + (parts[1]?.[0] ?? parts[0][1] ?? '')).toUpperCase()
}
