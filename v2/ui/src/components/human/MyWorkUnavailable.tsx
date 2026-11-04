/**
 * What My work shows when it has nothing it can count: never zeros or a
 * green "all clear" (AMP critique #1). Two states:
 *
 * - MyWorkUnavailable: there's no AMP user to collect a list for (sign-in is
 *   off, or this session isn't signed in). Says why in plain words and what to
 *   do instead; no Try again, since trying again can't change the answer.
 * - MyWorkLoadError: the load failed. Says nothing is counted, shows the
 *   server's reason, and offers Try again.
 */
import { Link } from 'react-router-dom'
import { ChevronRight, LogIn, TriangleAlert, UserX } from 'lucide-react'
import type { NoMyWork } from '../../lib/myWorkAccess'
import { projectLink } from '../../lib/projectLink'

/** Sign in, then come back here (oauth2-proxy's start URL, as AuthProvider uses). */
function signInHref(): string {
  const here = window.location.pathname + window.location.search
  return `/oauth2/start?rd=${encodeURIComponent(here)}`
}

export function MyWorkUnavailable({ reason, projectId }: { reason: NoMyWork; projectId?: number }) {
  const local = reason === 'local-dev'
  // Where every ticket is, with its actions: this project's Board, or the project list.
  const board = projectId ? { to: projectLink(projectId, 'board'), label: 'Open the Board' } : { to: '/projects', label: 'Open a project' }
  return (
    <section className="amp-panel border-l-[3px] px-5 py-4 grid gap-2.5" style={{ borderLeftColor: 'var(--warning)' }}
      aria-label={local ? 'My work is off: sign-in is turned off' : 'My work: not signed in'}>
      <span className="amp-label inline-flex items-center gap-1.5" style={{ color: 'var(--warning-ink)' }}>
        <UserX size={14} aria-hidden="true" />{local ? 'My work is off' : 'Not signed in'}
      </span>
      <h2 className="m-0 text-[17px] font-semibold text-ink">
        {local ? 'Sign-in is turned off on this AMP, so there is no list of yours to show.' : 'Sign in to see what waits on you.'}
      </h2>
      <p className="m-0 text-[13.5px] text-ink-2 leading-normal max-w-[64ch]">
        {local
          ? 'This AMP runs without sign-in (local development). Everyone is the shared Local Dev identity, which is not an AMP user, so no plan, question or merge can be waiting on you. Nothing has been counted, so no counts are shown.'
          : 'My work is one person\'s list, and this session is not signed in as an AMP user: you may have signed out, or your session expired. Nothing has been counted, so no counts are shown.'}
      </p>
      <ul className="m-0 pl-5 grid gap-1 text-[13px] text-ink-2 leading-normal list-disc">
        {!local && <li>Sign in, and this page reloads with your list.</li>}
        <li>To see what is waiting right now, open the Board: every ticket is there, and its panel has the actions.</li>
        {local && <li>To get a My work of your own, run AMP with sign-in on (set <code className="amp-num text-[12px]">OIDC_ISSUER_URL</code>) and sign in.</li>}
      </ul>
      <div className="flex flex-wrap gap-2.5 mt-1">
        {!local && <a href={signInHref()} className="amp-btn amp-btn-primary amp-btn-sm inline-flex items-center gap-1.5"><LogIn size={14} aria-hidden="true" />Sign in</a>}
        <Link to={board.to} className="amp-btn amp-btn-sm inline-flex items-center gap-1">{board.label}<ChevronRight size={14} aria-hidden="true" /></Link>
      </div>
    </section>
  )
}

export function MyWorkLoadError({ error, onRetry }: { error: string; onRetry: () => void }) {
  return (
    <div className="amp-panel border-l-[3px] px-4 py-3 flex flex-wrap items-center gap-x-3 gap-y-2" style={{ borderLeftColor: 'var(--danger)' }} role="alert">
      <TriangleAlert size={16} className="text-danger-ink shrink-0" aria-hidden="true" />
      <div className="grid gap-0.5 min-w-0 flex-1">
        <span className="text-[13px] text-danger-ink">Couldn't load My work: {error}</span>
        <span className="text-[12px] text-ink-3">Nothing is counted until it loads, so no counts are shown.</span>
      </div>
      <button type="button" className="amp-btn amp-btn-sm" onClick={onRetry}>Try again</button>
    </div>
  )
}
