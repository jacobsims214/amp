import { Lock } from 'lucide-react'
import { identityLabel, initials, type RequesterIdentity } from '../../lib/requesterChrome'

export type { RequesterIdentity }

// The requester's frame (approved mockup artifact 38 v1: the topbar and the
// two-tab bar of the "New request" / "Your requests" screens; build slice J4 of
// discoveries/team-inbox-approved-build-slices.md, ticket AMP #3981).
//
// This is the chrome a customer sees — someone outside the team, invited to
// send requests and track the answers. It is deliberately a different frame
// from `components/shell/AppShell`, and the difference is the point: the app
// shell is the team's frame, with the project tree, org admin, sessions and
// every internal name in it. A requester who is not a member of the org must
// never see any of that, and the cheapest way to guarantee it is to render a
// frame that has no room for it.
//
// What this file therefore does NOT have, on purpose:
//
//  - no import of `api`/`useAuth`/`AppShell`, and no project, epic, story,
//    ticket, run, agent, org or metadata prop. Its whole prop list is what a
//    customer's own header needs (the team's public name, their own name and
//    address, an optional nav slot, an optional notice and an optional
//    sign-out). A component cannot leak a field it was never given.
//  - no route knowledge. The nav is a slot the page fills (ticket #3988 owns
//    routing), so this file neither imports react-router nor invents a URL:
//    "no pages/router" in this ticket's description is kept literally.
//  - no sidebar, no project tree, no breadcrumb, no "skip to project". The only
//    links are the ones the page passes in, and they are the requester's own
//    two screens.
//
// The one team fact it does show is the team's public display name, because the
// approved mockup shows it and a requester has to know who they are writing to.

export interface RequesterChromeProps {
  /** The team's public display name ("Northwind support desk"). Never an org or project name. */
  teamName: string
  /** The signed-in requester. */
  me: RequesterIdentity
  /** The requester's own screens, as the page's own links. Omitted, no nav renders at all. */
  nav?: React.ReactNode
  /** A line under the frame's rule: an access notice, a "your grant was removed" warning. */
  banner?: React.ReactNode
  /** Sign out. Omitted, no sign-out control renders. */
  onSignOut?: () => void
  /** The page's content. */
  children: React.ReactNode
}

export function RequesterChrome({ teamName, me, nav, banner, onSignOut, children }: RequesterChromeProps) {
  const label = identityLabel(me)
  return (
    <div className="min-h-screen bg-bg text-ink">
      <a href="#requester-content" className="amp-skip-link">Skip to the form</a>

      <header className="border-b border-line bg-surface">
        <div className="mx-auto flex h-[52px] max-w-[1060px] items-center gap-3 px-4 sm:px-5">
          {/* The product mark, not a link into the app: a requester has no app to go to. */}
          <span className="flex flex-shrink-0 items-center gap-2.5" aria-label="AMP">
            <span className="amp-mark" aria-hidden="true">
              <i className="bg-band-1" /><i className="bg-band-2" /><i className="bg-band-3" /><i className="bg-band-4" />
            </span>
            <span className="text-[14px] font-semibold uppercase tracking-[0.16em] text-ink">AMP</span>
          </span>

          {/* The one team fact a requester sees, and it is the public name. On a
              phone it is hidden: the page's own heading names the desk there. */}
          <span className="hidden border-l border-line pl-3 font-label text-[12px] font-medium uppercase tracking-[0.14em] text-ink-2 sm:inline">
            {teamName}
          </span>

          <span className="flex-1" />

          <span className="flex min-w-0 items-center gap-2 text-[12.5px] text-ink-2">
            <span
              className="grid h-6 w-6 flex-shrink-0 place-items-center bg-ink font-label text-[10px] font-semibold tracking-[0.06em] text-[color:var(--on-ink)]"
              style={{ borderRadius: 999 }}
              aria-hidden="true"
            >
              {initials(me)}
            </span>
            {/* Truncated, never wrapped: a long address must not push the frame. */}
            <span className="hidden max-w-[240px] truncate sm:inline" title={`${label} · ${me.email}`} data-testid="requester-identity">
              {label}
              {label === me.email ? null : <span className="text-ink-3"> · {me.email}</span>}
            </span>
            <span className="sr-only sm:hidden" data-testid="requester-identity-sr">{label}</span>
          </span>

          {onSignOut && (
            <button type="button" onClick={onSignOut} className="amp-btn amp-btn-ghost amp-btn-sm flex-shrink-0">
              Sign out
            </button>
          )}
        </div>

        {nav && (
          <nav aria-label="Requests" className="mx-auto max-w-[1060px] px-4 sm:px-5">
            <span className="flex gap-0.5">{nav}</span>
          </nav>
        )}
      </header>

      {banner && (
        <div className="border-b border-line bg-surface-2">
          <div className="mx-auto max-w-[1060px] px-4 py-2.5 text-[12.5px] leading-relaxed text-ink-2 sm:px-5">{banner}</div>
        </div>
      )}

      <main id="requester-content" className="mx-auto max-w-[1060px] px-4 pb-24 pt-5 sm:px-5 sm:pb-28">
        {children}
      </main>

      {/* The privacy sentence the mockup closes the submit and status screens
          with. It is a property of the frame, so every requester screen says it
          the same way rather than each page re-wording it. */}
      <footer className="mx-auto max-w-[1060px] px-4 pb-10 sm:px-5">
        <p className="m-0 flex items-start gap-2 text-[11.5px] leading-relaxed text-ink-3">
          <Lock size={13} className="mt-0.5 flex-shrink-0" aria-hidden="true" />
          <span>
            You only ever see your own requests to {teamName} — never anyone else's, and never the team's internal
            planning.
          </span>
        </p>
      </footer>
    </div>
  )
}
