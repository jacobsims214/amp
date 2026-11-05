/**
 * A ticket's human ref and canonical link (v2/docs/url-state.md).
 *
 * The ref is the project code and the ticket id, `BA-34`
 * (amp-api domain.TaskRef). Ticket ids are global, so the id alone finds the
 * ticket: `/t/BA-34`, `/t/ba-34` and `/t/34` all open ticket 34, wherever it
 * lives. The code is there for people reading the link.
 */
import { projectLink } from './projectLink'

/** `BA-34` for a project code and id; just the id when the code is unknown. */
export function ticketRef(code: string | null | undefined, id: number): string {
  const c = (code ?? '').trim().toUpperCase()
  return c ? `${c}-${id}` : String(id)
}

/** The canonical, project-independent link to a ticket: `/t/BA-34`. */
export function ticketPath(code: string | null | undefined, id: number): string {
  // Encoded: a code is only conventionally letters, and a ? or / would break the link.
  return `/t/${encodeURIComponent(ticketRef(code, id))}`
}

/** The longest `?ref=` the drawer will quote. */
const MAX_TYPED_REF = 40

/**
 * The ref a `/t/` link was typed with (`?ref=`, set by TicketRedirect), when
 * it's worth a notice: it looks like a ref (`CODE-<n>`, at most 40
 * characters), its number is this ticket's id, and its code isn't this
 * ticket's project's. Anything else in `?ref=` (hand-edited, spoofed) is
 * ignored, so the drawer never quotes arbitrary text.
 */
export function mismatchedRef(raw: string | null, id: number, projectCode: string | null | undefined): string | null {
  if (!raw || !projectCode || raw.length > MAX_TYPED_REF) return null
  const ref = raw.trim().toUpperCase()
  const m = /^([A-Z0-9][A-Z0-9-]*)-(\d+)$/.exec(ref)
  if (!m || Number(m[2]) !== id) return null
  return ref === ticketRef(projectCode, id) ? null : ref
}

/** A run's canonical link: `/r/<run id>`. */
export function runPath(runId: number): string {
  return `/r/${runId}`
}

/**
 * Read a ref typed or pasted after /t/: `BA-34`, `ba-34`, `34`, `#34`. The
 * trailing integer is the ticket id; what comes before it (minus the dash) is
 * the code the link claims, or null when there is none. Anything without a
 * positive trailing integer is not a ref.
 */
export function parseTicketRef(raw: string): { code: string | null; id: number } | null {
  let text = raw
  try { text = decodeURIComponent(raw) } catch { /* a stray % : read it as typed */ }
  const m = /^#?(.*?\D)?(\d+)$/.exec(text.trim())
  if (!m) return null
  const id = Number(m[2])
  if (!Number.isSafeInteger(id) || id <= 0) return null
  const code = (m[1] ?? '').replace(/[-_ ]+$/, '').trim().toUpperCase()
  return { code: code || null, id }
}

/**
 * Where `/t/<ref>` lands: ticket `id` on its project's Board, drawer open,
 * keeping the other params in `search` (`tab`, `run`, `event`) plus `extra`.
 */
export function ticketLanding(projectId: number, id: number, search: string, extra: Record<string, string> = {}): string {
  const params = new URLSearchParams(search)
  params.delete('focus')
  for (const [k, v] of Object.entries(extra)) params.set(k, v)
  const rest = params.toString()
  return `${projectLink(projectId, 'board', id)}${rest ? `&${rest}` : ''}`
}
