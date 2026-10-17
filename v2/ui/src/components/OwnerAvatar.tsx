import { initials, type Person } from '../lib/people'

function tooltip(person: Person, prefix?: string) {
  const who = person.email ? `${person.name} <${person.email}>` : person.name
  return prefix ? `${prefix}: ${who}` : who
}

/** A person: black circle with initials (agents use Avatar's hexagon). */
export function OwnerAvatar({ person, size = 18, title }: { person: Person; size?: number; title?: string }) {
  return (
    <span
      className="inline-flex items-center justify-center rounded-full bg-ink text-on-ink font-label font-bold flex-shrink-0 select-none"
      style={{ width: size, height: size, fontSize: Math.max(8, Math.round(size * 0.45)), boxShadow: '0 0 0 1.5px var(--surface)' }}
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
      {extra > 0 && <span className="text-[10px] text-ink-2 ml-1 tabular-nums">+{extra}</span>}
    </span>
  )
}

export function UnassignedPill() {
  return (
    <span
      className="font-label text-[10px] font-semibold uppercase tracking-[0.1em] px-1.5 py-px border border-dashed border-line-str text-ink-2 whitespace-nowrap"
      title="No owner — anyone on the project can claim it"
    >
      Unassigned
    </span>
  )
}
