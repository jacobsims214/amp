/**
 * The pieces the request detail's three right-rail panels share (AMP #3980,
 * build slice J2; approved mockup artifact 37 v1, "Request detail" right
 * column).
 *
 * They are here rather than in each panel because all three say the same few
 * things in the same way, and the way they say them is the design: an
 * attribute row is a `dt`/`dd` pair, a relation is a reference plus a title plus
 * one status tag, and every explanation a disabled control gives is a sentence
 * next to the control rather than a tooltip nobody opens. A panel that
 * re-invented any of those would drift from the approved mockup and from its
 * siblings, which is exactly what the two rails beside these ones would notice
 * first.
 */
import { AlertTriangle, CircleSlash, ExternalLink, ShieldAlert } from 'lucide-react'
import { Avatar } from '../ui/Avatar'
import { ago } from '../../lib/approval'
import type { EvidenceTone } from '../../lib/requestControls'
import type { Person } from '../../lib/people'

/** The tag tones the mockup uses: in review (blue), approved/merged (green), needs a person (amber), gone (grey). */
const TONE_STYLE: Record<EvidenceTone, React.CSSProperties> = {
  rev: { borderColor: 'var(--link)', color: 'var(--link)' },
  ok: { borderColor: 'var(--success)', color: 'var(--success-ink)' },
  chg: { borderColor: 'var(--warning)', color: 'var(--warning-ink)' },
  gone: { borderColor: 'var(--line-str)', color: 'var(--ink-3)' },
}

/**
 * One status word on a row: "awaiting approval", "merged", "pending verify",
 * "deleted".
 *
 * It is a word and a tone, never a colour alone: the tone is repeated in the
 * text so the tag still reads without colour (the mockup's own `arttag` does
 * the same).
 */
export function RailTag({ tone, children, title }: { tone: EvidenceTone; children: React.ReactNode; title?: string }) {
  return (
    <span
      title={title}
      data-tone={tone}
      className="inline-flex items-center gap-1 whitespace-nowrap border px-1.5 py-[3px] font-label text-[10.5px] font-semibold uppercase leading-none tracking-[0.1em]"
      style={{ borderRadius: 'var(--radius)', ...TONE_STYLE[tone] }}
    >
      {tone === 'gone' && <CircleSlash size={9} aria-hidden="true" />}
      {children}
    </span>
  )
}

/**
 * A relation row: `PLANNED AS` / `AMP-V2-3712 — Berlin SSO group mapping` /
 * `awaiting approval`.
 *
 * The monospace label is the mockup's, and it is what makes three different
 * relations legible in one column without a legend. A row whose target no
 * longer resolves keeps its place with an em dash title and a "deleted" tag:
 * the link outlived its target, and hiding the row would silently rewrite what
 * the request was planned as (the tombstone rule, 055).
 */
export function RailLinkRow({ kind, title, href, target, tone, tag, title_ }: {
  /** The relation, in the mockup's words: "Planned as", "Delivered by", "Relates to". */
  kind: string
  title: string
  /** Where the row goes when the target still exists. Absent makes the row plain text. */
  href?: string
  /** The target reference shown in mono before the title ("AMP-V2-3712", "REQ-8DXT0"). */
  target?: string
  tone?: EvidenceTone
  tag?: string
  /** The tag's explanatory sentence, for the hover and the accessible name. */
  title_?: string
}) {
  // The title truncates inside the row rather than pushing the tag out of it:
  // flex-1 + min-w-0 on the label is what actually bounds a truncate, since a
  // flex item's automatic minimum content size would otherwise be the whole
  // string and the tag would ride over it.
  const label = (
    <span className="flex items-center gap-1.5 min-w-0 flex-1">
      {target && <span className="amp-num text-[11px] text-ink-3 flex-shrink-0">{target}</span>}
      <span className="truncate min-w-0">{title}</span>
    </span>
  )
  return (
    <div className="flex items-center gap-2.5 py-2 border-b border-line-sub last:border-b-0 text-[13px] min-w-0">
      <span className="amp-label w-[86px] flex-shrink-0 !text-[9.5px]">{kind}</span>
      {href
        ? <a href={href} className="flex items-center gap-1.5 min-w-0 flex-1 no-underline text-ink hover:text-link">
            {label}
            <ExternalLink size={11} className="flex-shrink-0 text-ink-3" aria-hidden="true" />
          </a>
        : <span className="flex items-center min-w-0 flex-1 text-ink">{label}</span>}
      {tag && tone && <span className="flex-shrink-0"><RailTag tone={tone} title={title_}>{tag}</RailTag></span>}
    </div>
  )
}

/**
 * A banner the rail states in place: the handoff warning, a claim conflict, an
 * action's failure.
 *
 * `warn` is the un-removable no-auto-approval note and `err` is a refusal;
 * `info` is a rule the reader benefits from knowing before they act. The alert
 * role belongs to `err` only — a conflict is an interruption, a standing
 * warning is part of the page.
 */
export function RailBanner({ tone, title, children, actions }: {
  tone: 'warn' | 'err' | 'info'
  title: React.ReactNode
  children?: React.ReactNode
  actions?: React.ReactNode
}) {
  const Icon = tone === 'warn' ? AlertTriangle : tone === 'err' ? ShieldAlert : null
  return (
    <div
      role={tone === 'err' ? 'alert' : undefined}
      data-banner={tone}
      className="flex items-start gap-2.5 px-3.5 py-2.5 border text-[12.5px] leading-[1.45]"
      style={{
        borderRadius: 'var(--radius)',
        ...(tone === 'warn'
          ? { borderColor: 'var(--warning)', background: 'color-mix(in srgb, var(--warning) 9%, var(--surface))' }
          : tone === 'err'
            ? { borderColor: 'var(--danger)', background: 'color-mix(in srgb, var(--danger) 7%, var(--surface))' }
            : { borderColor: 'var(--line)', background: 'var(--surface-2)', color: 'var(--ink-2)' }),
      }}
    >
      {Icon && <Icon size={13} className="flex-shrink-0 mt-[2px]" style={{ color: tone === 'warn' ? 'var(--warning-ink)' : 'var(--danger-ink)' }} aria-hidden="true" />}
      <div className="min-w-0">
        <span className="font-semibold text-ink">{title}</span>
        {children && <> {children}</>}
        {actions && <div className="mt-2 flex gap-2 flex-wrap">{actions}</div>}
      </div>
    </div>
  )
}

/** One attribute of the request, as the rail's own `dt`/`dd` row. */
export function RailField({ label, children, hint }: { label: string; children: React.ReactNode; hint?: React.ReactNode }) {
  return (
    <>
      <dt className="text-[12px] text-ink-3 self-start pt-[2px]">{label}</dt>
      <dd className="m-0 min-w-0 text-[13px]">
        {children}
        {hint && <div className="text-[11.5px] text-ink-3 mt-1 leading-[1.4]">{hint}</div>}
      </dd>
    </>
  )
}

/**
 * "Priya · 26h": who holds the claim and how long they have held it.
 *
 * The age is not decoration. Claims never expire (D4), so a stale claim is only
 * visible if its age is, and a rail that showed the name alone would make an
 * abandoned claim look like active triage.
 */
export function ClaimWho({ person, at, now }: { person: Person | null; at: string | null; now?: number }) {
  const name = person?.name ?? 'Unknown'
  return (
    <span className="inline-flex items-center gap-1.5 min-w-0">
      <Avatar name={name} size={20} />
      <span className="truncate">{name}</span>
      {at && <span className="text-ink-3 text-[12px] whitespace-nowrap">· {ago(at, now)}</span>}
    </span>
  )
}

/**
 * The rule that keeps claim and assignee from merging into one field, stated
 * once under both (the approved mockup's own caption).
 */
export const CLAIM_VS_ASSIGNEE =
  'Claim = who\u2019s triaging now (one active claim, first-write-wins, never expires). ' +
  'Assignee = who owns it to delivery. A transfer clears the claim.'

/**
 * A control the viewer cannot use, and the sentence that says why.
 *
 * The mockup's disabled controls all explain themselves; doing that as one
 * component is what keeps a panel from shipping a greyed-out button with no
 * reason attached (and what makes the reason assertable in a test).
 */
export function DisabledHint({ id, children }: { id: string; children: React.ReactNode }) {
  return <p id={id} className="m-0 text-[11.5px] text-ink-3 leading-[1.45]">{children}</p>
}
