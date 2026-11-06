/**
 * Whether My work can be collected for the caller. My work is one person's
 * list, so it needs an AMP user (KB architecture/waiting-on-you.md). Two ways
 * there isn't one:
 *
 * - `local-dev`: sign-in is off on this AMP (no OIDC_ISSUER_URL) and the
 *   Local Dev identity has no AMP user. Since #124 amp-api provisions one at
 *   startup, so this only happens when that failed (or on an older API).
 * - `no-user`: sign-in is on but this session has no AMP user (signed out,
 *   or the session expired).
 *
 * The server answers top-level My work with an empty list in both cases and
 * project My work with "My work needs a signed-in AMP user"; the UI says which
 * case it is and what to do instead of showing zeros or that message.
 */
import type { Me } from '../types'

export type NoMyWork = 'local-dev' | 'no-user'

/** The server's message when project My work has no AMP user to read for. */
const NO_USER_ERROR = /needs a signed-in AMP user/i

export function isNoUserError(message?: string | null): boolean {
  return !!message && NO_USER_ERROR.test(message)
}

/**
 * Why the caller has no My work, or null when they have one or the identity
 * isn't known yet (still loading, or /api/me failed).
 */
export function noMyWorkReason(me: Me | null | undefined): NoMyWork | null {
  if (!me || me.user_id) return null
  return me.local_dev || me.subject === 'local-dev' ? 'local-dev' : 'no-user'
}
