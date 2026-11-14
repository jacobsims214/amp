/**
 * The one section shell the team-inbox request panels share (AMP #3979).
 *
 * It is the `amp-panel` + `amp-label` header + body pattern from approved
 * mockup 37 v1, extracted so the discussion, history and attachment panels
 * cannot drift into three slightly different headers. A section's label is a
 * heading (`h3`) and its optional note sits beside it on a wide screen and under
 * it at 390 px, because the note is a sentence, not a badge: squeezing it to
 * nothing on a phone hides the rule it exists to state.
 */
import type { ReactNode } from 'react'

export function RequestPanelSection({ label, note, actions, children, className = '' }: {
  /** The section's name, in the interface's small-caps label style. */
  label: string
  /** One sentence about what the section is or what it will not do. */
  note?: ReactNode
  /** Controls that belong to the section as a whole (a count, a refresh). */
  actions?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <section className={`amp-panel min-w-0 ${className}`}>
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-3.5 py-2.5 border-b border-line-sub">
        <h3 className="amp-label m-0">{label}</h3>
        {note ? <span className="text-[12px] text-ink-3 min-w-0 leading-snug">{note}</span> : null}
        {actions ? <div className="ml-auto flex items-center gap-2 flex-shrink-0">{actions}</div> : null}
      </div>
      <div className="px-3.5 py-3.5 flex flex-col gap-3 min-w-0">{children}</div>
    </section>
  )
}

/** One quiet line where a list would be: used for an empty thread, history or file list. */
export function RequestPanelEmpty({ children }: { children: ReactNode }) {
  return <p className="m-0 text-[12.5px] text-ink-3 leading-snug" data-testid="request-panel-empty">{children}</p>
}

/** The one sentence a section uses when its own read or write could not be completed. */
export function RequestPanelError({ message }: { message: string }) {
  return (
    <p role="alert" className="m-0 text-[12.5px] leading-snug text-danger-ink border border-danger px-2.5 py-2"
      style={{ borderRadius: 'var(--radius)', background: 'color-mix(in srgb, var(--danger) 6%, var(--surface))' }}>
      {message}
    </p>
  )
}
