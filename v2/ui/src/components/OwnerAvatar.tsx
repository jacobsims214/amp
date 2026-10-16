import { initials, type Person } from '../lib/people'

// Muted accent pairs from the board palette; picked by user id so a person
// keeps the same colour everywhere.
const AVATAR_COLORS: { bg: string; fg: string; border: string }[] = [
  { bg: 'rgba(99,102,241,0.18)',  fg: '#A5B4FC', border: 'rgba(99,102,241,0.35)' },
  { bg: 'rgba(16,185,129,0.15)',  fg: '#34D399', border: 'rgba(16,185,129,0.3)' },
  { bg: 'rgba(245,158,11,0.15)',  fg: '#FBBF24', border: 'rgba(245,158,11,0.3)' },
  { bg: 'rgba(56,189,248,0.15)',  fg: '#7DD3FC', border: 'rgba(56,189,248,0.3)' },
  { bg: 'rgba(236,72,153,0.15)',  fg: '#F9A8D4', border: 'rgba(236,72,153,0.3)' },
]

function tooltip(person: Person, prefix?: string) {
  const who = person.email ? `${person.name} <${person.email}>` : person.name
  return prefix ? `${prefix}: ${who}` : who
}

export function OwnerAvatar({ person, size = 18, title }: { person: Person; size?: number; title?: string }) {
  const c = AVATAR_COLORS[Math.abs(person.id) % AVATAR_COLORS.length]
  return (
    <span
      className="inline-flex items-center justify-center rounded-full font-bold flex-shrink-0 select-none"
      style={{
        width: size, height: size,
        fontSize: Math.max(8, Math.round(size * 0.45)),
        background: c.bg, color: c.fg, border: `1px solid ${c.border}`,
      }}
      title={tooltip(person, title)}
    >
      {initials(person.name)}
    </span>
  )
}

/** Overlapping avatar stack for epic/story owners; shows up to `max`, then +N. */
export function OwnerStack({ people, max = 3, size = 16 }: { people: Person[]; max?: number; size?: number }) {
  if (people.length === 0) return null
  const shown = people.slice(0, max)
  const extra = people.length - shown.length
  return (
    <span className="inline-flex items-center flex-shrink-0" title={`Owners: ${people.map(p => p.name).join(', ')}`}>
      {shown.map((p, i) => (
        <span key={p.id} style={{ marginLeft: i === 0 ? 0 : -4 }}>
          <OwnerAvatar person={p} size={size} title="Owner" />
        </span>
      ))}
      {extra > 0 && <span className="text-[10px] text-[#7E91A8] ml-1 tabular-nums">+{extra}</span>}
    </span>
  )
}

export function UnassignedPill() {
  return (
    <span
      className="text-[10px] font-semibold px-1.5 py-px rounded-full border border-dashed border-[#283A57] text-[#7E91A8] whitespace-nowrap"
      title="No owner — anyone on the project can claim it"
    >
      Unassigned
    </span>
  )
}
