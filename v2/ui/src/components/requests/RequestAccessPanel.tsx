/**
 * "Submit access" — who may send requests to this inbox (AMP #3982; approved
 * contract artifact 36 v2 §2 and §5; approved mockup 37 v1's page-level
 * "Submit access" button).
 *
 * The screen administers three separate things, and the separation is the whole
 * design:
 *
 *  1. **Submit grants** — `team_submit_grants` rows. Grant one, revoke one. It
 *     is not membership, and the panel says so above the controls rather than
 *     in a tooltip: the one wrong idea a screen like this can plant is that
 *     adding somebody here puts them on the team. It does not — they reach no
 *     project and hold no role (contract §2, D14).
 *  2. **Requester invitations** — a pending email invitation that provisions a
 *     *customer* identity at first sign-in and grants no role at all. Separate
 *     from the grant because they are separate acts on the server: an
 *     invitation is an intent to create an account, a grant is access for an
 *     account that already exists.
 *  3. **Submit-only credentials** — an `amp_` token attenuated to this one inbox
 *     (#3772). Minted through the existing `POST /api/users/{id}/tokens` route
 *     with `scope: "submit"`, exactly as `internal/api/request_grants.go` says
 *     it should be: there is deliberately no mint route on this surface. The
 *     secret is shown **once**, through the same `SecretReveal` every other
 *     credential in AMP uses, and it is never written anywhere — it lives in one
 *     React state variable and is cleared the moment a person dismisses it or
 *     closes the panel. Everything the panel can show afterwards is the prefix
 *     the server keeps.
 *
 * Every write goes through `requestApi` (#3977) and nothing here decides
 * permission: the manager gate is `authz.RequireTeamManager`'s answer on every
 * call, and a refusal arrives as the server's own sentence (`lib/apiError.ts`),
 * which the panel renders in place rather than translating into a friendlier
 * half-truth. `canManageSubmitAccess` decides only whether the panel *offers* a
 * control, and its note explains why an optimistic hint is the right shape.
 *
 * Above those three, the panel hands out the way in (#4878, approved mockup #112
 * v1): the requester entry link, built by `requesterNewRequestPath` and copyable.
 * It is not a fourth act on the server — nothing is emailed by the invitation
 * either — it is how the acts above become useful to somebody who is not in the
 * room, so its copy says exactly that.
 */
import { useCallback, useEffect, useState } from 'react'
import { AlertTriangle, Ban, Check, Copy, KeyRound, Loader2, Mail, Plus, ShieldCheck, UserPlus } from 'lucide-react'
import { api, requestApi } from '../../api/client'
import { SecretReveal } from '../admin'
import { ConfirmButton } from '../ui/ConfirmButton'
import { errMsg } from '../../lib/adminStyles'
import { requesterNewRequestPath } from '../../lib/requestRoutes'
import {
  activeGrantCount, canManageSubmitAccess, cannotManageReason, credentialExpired, dayLabel, grantRows,
  grantState, granteeName, REQUESTER_INVITE_RULE, SUBMIT_ACCESS_IS_NOT_MEMBERSHIP, SUBMIT_CREDENTIAL_RULE, SUBMIT_TOKEN_DAYS,
  submitScopedTokens, tokenPrefix,
} from '../../lib/requestAccess'
import type { APIToken, Invite, Me, NewAPIToken, Team, TeamSubmitGrantee } from '../../types'

export interface RequestAccessPanelProps {
  /** The inbox this panel administers. Every route is team-scoped, so this is required rather than inferred. */
  teamId: number
  /**
   * The team's own row. Optional so the panel can be mounted from a page that
   * already has it; when absent the panel reads it (the team's `org_id` is what
   * `canManageSubmitAccess` needs).
   */
  team?: Team | null
  /**
   * The caller's identity. Absent means the auth context has not answered; the
   * panel then reads as "sign in to see who can send requests", never as an
   * assumption that nobody may.
   */
  me?: Me | null
  /** `auth.isAdmin` from the auth context; a global admin manages any inbox. */
  isAdmin?: boolean
  /** The caller's own user id, for "(you)" on a grant row. */
  meId?: number
  /** Close the panel (the inbox page shows it in a modal). Also drops the minted secret. */
  onClose?: () => void
}

/** Which section an act belonged to, so its refusal lands beside it rather than at the foot of the panel. */
type Section = 'grants' | 'invites' | 'credentials'

/** The three lists' state, one variable at a time. */
interface Lists {
  grants: TeamSubmitGrantee[]
  invites: Invite[]
  tokens: APIToken[]
  team: Team | null
  loading: boolean
  error: string | null
}

const EMPTY_LISTS: Lists = { grants: [], invites: [], tokens: [], team: null, loading: true, error: null }

/** One write's progress, so two controls cannot race and only the acted-on row shows a spinner. */
type Busy = { section: Section; key: string } | null

export function RequestAccessPanel({ teamId, team: teamProp, me, isAdmin = false, meId, onClose }: RequestAccessPanelProps) {
  const [lists, setLists] = useState<Lists>(EMPTY_LISTS)
  const [busy, setBusy] = useState<Busy>(null)
  const [failure, setFailure] = useState<{ section: Section; message: string } | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  /** The mint's secret: the only place it ever exists in this app, cleared on dismissal. */
  const [fresh, setFresh] = useState<NewAPIToken | null>(null)
  const [grantWho, setGrantWho] = useState('')
  const [inviteEmail, setInviteEmail] = useState('')
  const [tokenName, setTokenName] = useState('')
  /**
   * One clock reading for the whole panel, taken per mount.
   *
   * Every date label and the expired check read this value, so two rows can
   * never disagree about "now", and a re-render never moves a label under a
   * reader's eyes (react-hooks' purity rule; lib/requestAccess's `dayLabel`).
   */
  const [now] = useState(() => Date.now())

  const team = teamProp ?? lists.team
  const canManage = canManageSubmitAccess(me, team, isAdmin)

  /**
   * Read the panel's three lists.
   *
   * Read together rather than on demand: a manager opening this screen is
   * asking one question — who can send here — and three sequential reads would
   * let them read a half-answer.
   */
  const load = useCallback(async () => {
    setLists(prev => ({ ...prev, loading: true, error: null }))
    try {
      const [grants, invites, tokens, teamAnswer] = await Promise.all([
        requestApi.listGrants(teamId),
        requestApi.listInvites(teamId),
        // The tokens are the caller's own: a submit-only credential may only be
        // minted for yourself (service.CreateScopedToken's `accountFor` refuses
        // anyone else's account), so this is the only list that can be right.
        api.listTokens('me'),
        teamProp ? Promise.resolve(null) : api.getTeam(teamId),
      ])
      setLists({
        grants, invites, tokens, team: teamProp ?? teamAnswer?.team ?? null, loading: false, error: null,
      })
    } catch (e) {
      setLists(prev => ({ ...prev, loading: false, error: errMsg(e, 'Could not read this inbox’s submit access') }))
    }
  }, [teamId, teamProp])

  useEffect(() => { void load() }, [load])

  /** Run one write: its refusal lands beside its own section, and a success re-reads every list. */
  const run = async (section: Section, key: string, fn: () => Promise<string>) => {
    setBusy({ section, key })
    setFailure(null)
    setNotice(null)
    try {
      const said = await fn()
      setNotice(said)
      await load()
    } catch (e) {
      // The server's sentence, never a bare "forbidden" (lib/apiError's job, already applied).
      setFailure({ section, message: errMsg(e, 'That change was not made') })
    } finally {
      setBusy(null)
    }
  }

  const grant = (who: string) => run('grants', `grant:${who.toLowerCase()}`, async () => {
    await requestApi.grantSubmit(teamId, who)
    setGrantWho('')
    return `${who} can now send requests to this inbox. They are not on the team and reach no project.`
  })

  const revoke = (g: TeamSubmitGrantee) => run('grants', `grant:${g.user_id}`, async () => {
    await requestApi.revokeSubmit(teamId, g.user_id)
    return `${granteeName(g)}’s submit access is revoked: their very next request is refused. Requests they already sent stay readable.`
  })

  const invite = (email: string) => run('invites', `invite:${email.toLowerCase()}`, async () => {
    await requestApi.inviteRequester(teamId, email)
    setInviteEmail('')
    return `Invited ${email} as a requester. They get a customer account at first sign-in, with no org or team role.`
  })

  const revokeInvite = (inv: Invite) => run('invites', `invite:${inv.id}`, async () => {
    await requestApi.revokeInvite(teamId, inv.id)
    return `${inv.email}’s invitation is withdrawn. If they never sign in, no account is created.`
  })

  /**
   * Mint a submit-only credential for the caller, bound to this inbox.
   *
   * The scope is what makes it narrow, and it rides the *existing* token route:
   * the answer carries the secret exactly once, and the panel keeps it in
   * `fresh` only until a person says they copied it.
   */
  const mint = (name: string) => run('credentials', `mint:${name.toLowerCase()}`, async () => {
    const token = await api.createToken('me', name, SUBMIT_TOKEN_DAYS, { scope: 'submit', scopeTeamId: teamId })
    setFresh(token)
    setTokenName('')
    return 'Submit-only credential minted. It is shown once, below.'
  })

  const revokeToken = (t: APIToken) => run('credentials', `token:${t.id}`, async () => {
    await api.revokeToken(t.id)
    return 'Credential revoked: every request it makes is refused from now on.'
  })

  const rows = grantRows(lists.grants)
  const active = activeGrantCount(lists.grants)
  const credentials = submitScopedTokens(lists.tokens, teamId)
  const close = () => { setFresh(null); onClose?.() }

  return (
    <div className="grid gap-4 min-w-0">
      {/* The rule, before any control: this screen grants sending, never membership. */}
      <p className="m-0 text-[12.5px] leading-relaxed text-ink-2 flex items-start gap-2"
        style={{ borderLeft: '3px solid var(--line-str)', paddingLeft: 10 }}>
        <ShieldCheck size={13} className="flex-shrink-0 mt-[3px] text-ink-3" aria-hidden="true" />
        <span><b className="text-ink">Sending is not joining.</b> {SUBMIT_ACCESS_IS_NOT_MEMBERSHIP}</span>
      </p>

      {/*
        The entry link, before the lists: it is the thing a manager came here
        for, and it is not one of the three acts below — nothing about it is
        written. Managers only: a viewer who may not change access cannot
        complete the act it starts, and the approved mockup (#112 v1) draws it
        as one of the panel's controls, absent from its not-permitted state.
      */}
      {canManage && team && <RequesterLink teamId={teamId} teamName={team.name} />}

      {lists.loading && lists.grants.length === 0 && lists.invites.length === 0 ? (
        <div className="flex items-center gap-2 py-4 text-[12.5px] text-ink-3" role="status">
          <Loader2 size={14} className="animate-spin" aria-hidden="true" />Loading who can send requests…
        </div>
      ) : lists.error ? (
        <div role="alert" className="flex flex-wrap items-start gap-2 px-3 py-2.5 border text-[12.5px]"
          style={{ borderColor: 'var(--danger)', background: 'color-mix(in srgb, var(--danger) 6%, var(--surface))', borderRadius: 'var(--radius)' }}>
          <AlertTriangle size={13} className="flex-shrink-0 mt-[2px] text-danger-ink" aria-hidden="true" />
          <span className="min-w-0 text-danger-ink">{lists.error}</span>
          <button type="button" onClick={() => void load()} className="amp-btn amp-btn-sm ml-auto">Try again</button>
        </div>
      ) : (
        <>
          {/* Standings first: a reader learns "who can send here" before "how to change it". */}
          <section className="grid gap-2 min-w-0" aria-labelledby="submit-who">
            <h2 id="submit-who" className="amp-label m-0">
              Can send · {active === 0 ? 'nobody yet' : `${active} ${active === 1 ? 'person' : 'people'}`}
            </h2>
            <GrantList grants={rows} meId={meId} busy={busy} canManage={canManage} onRevoke={revoke} now={now} />
            <SectionError failure={failure} section="grants" />
            {canManage && (
              <GrantForm busy={busy?.section === 'grants'} value={grantWho} onChange={setGrantWho} onSubmit={grant} />
            )}
          </section>

          {canManage ? (
            <>
              <section className="grid gap-2 min-w-0" aria-labelledby="submit-invites">
                <h2 id="submit-invites" className="amp-label m-0">
                  Invited, not signed in yet · {lists.invites.length}
                </h2>
                <p className="m-0 text-[12px] leading-snug text-ink-3">{REQUESTER_INVITE_RULE}</p>
                <InviteList invites={lists.invites} busy={busy} onRevoke={revokeInvite} now={now} />
                <SectionError failure={failure} section="invites" />
                <InviteForm busy={busy?.section === 'invites'} value={inviteEmail} onChange={setInviteEmail} onSubmit={invite} />
              </section>

              <section className="grid gap-2 min-w-0" aria-labelledby="submit-credentials">
                <h2 id="submit-credentials" className="amp-label m-0">Submit-only credentials</h2>
                <p className="m-0 text-[12px] leading-snug text-ink-3">{SUBMIT_CREDENTIAL_RULE}</p>

                {fresh && (
                  <SecretReveal
                    title={`New submit-only credential: ${fresh.name}`}
                    secret={fresh.token}
                    note={`Copy it now: AMP keeps only a hash, so it can't be shown again. It can send requests to this inbox and nothing else, for ${SUBMIT_TOKEN_DAYS} days.`}
                    onDone={() => setFresh(null)}
                  />
                )}

                <CredentialList tokens={credentials} busy={busy} onRevoke={revokeToken} now={now} />
                <SectionError failure={failure} section="credentials" />
                <MintForm busy={busy?.section === 'credentials'} value={tokenName} onChange={setTokenName} onSubmit={mint} />
              </section>
            </>
          ) : (
            // No control at all, and the reason stated: "no permission" says who can,
            // rather than leaving a viewer to guess whether the screen is broken.
            <p className="m-0 text-[12.5px] leading-snug text-ink-2" data-testid="submit-access-managers-only">
              {cannotManageReason(me)}
            </p>
          )}

          {/* A write's confirmation, near the acts it confirms. */}
          {notice && (
            <p role="status" className="m-0 text-[12.5px] leading-snug text-success-ink" data-testid="submit-access-notice">
              {notice}
            </p>
          )}
        </>
      )}

      {onClose && (
        <div className="flex justify-end">
          <button type="button" onClick={close} className="amp-btn">Close</button>
        </div>
      )}
    </div>
  )
}

/* ── the requester's way in ───────────────────────────────────────────────── */

/**
 * The requester entry link, as something a manager copies and sends.
 *
 * This block is the delivery mechanism, and the copy says so. Nothing in AMP
 * sends mail — `POST /api/teams/{id}/request-invites` writes a pending
 * invitation row and mails nobody (`internal/api/request_grants.go`) — so a
 * manager who believed the invitation had been delivered would leave a
 * requester waiting for a message that never comes. The link is also the only
 * thing that works for a requester with no AMP account at all, and its two
 * query parameters exist because their own projection never carries a team id
 * (`lib/requestRoutes.ts` §2): hence `requesterNewRequestPath` rather than a
 * template literal here, so those two facts stay in the one file that explains
 * them.
 *
 * The copy reports both outcomes. A refused clipboard is not a failed action —
 * the link is on screen, selectable — so it says "select it and copy" rather
 * than claiming a success that did not happen (the same shape as
 * `pages/RequesterSubmit`'s reference copy, which is the other place in the
 * product where a person's only route in is a string on the screen).
 */
function RequesterLink({ teamId, teamName }: { teamId: number; teamName: string }) {
  // Built here, never assembled: the builder is what keeps `?team=` and
  // `?desk=` travelling together, and the absolute form is what a recipient
  // can paste (the rest of the app copies links the same way).
  const url = `${window.location.origin}${requesterNewRequestPath({ teamId, teamName })}`
  const [copied, setCopied] = useState<'idle' | 'copied' | 'failed'>('idle')

  // The outcome clears itself, so a second copy is possible without a stale
  // "Copied" from a minute ago sitting under a button nobody pressed.
  useEffect(() => {
    if (copied === 'idle') return
    const t = setTimeout(() => setCopied('idle'), 4000)
    return () => clearTimeout(t)
  }, [copied])

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(url)
      setCopied('copied')
    } catch {
      setCopied('failed')
    }
  }

  return (
    <section className="grid gap-1.5 min-w-0" aria-labelledby="requester-link-head">
      <h2 id="requester-link-head" className="amp-label m-0">Requester link</h2>
      <div className="flex flex-wrap items-center gap-2 min-w-0">
        <code
          data-testid="requester-link-url"
          className="amp-num flex-1 min-w-[200px] px-3 py-2 text-[12px] text-ink border border-line select-all"
          style={{ background: 'var(--surface-2)', borderRadius: 'var(--radius)', overflowWrap: 'anywhere' }}
        >
          {url}
        </code>
        <button
          type="button"
          onClick={() => void copy()}
          // A stable name: a button that renames itself to "Copied" loses its
          // own label for voice and screen-reader navigation, and the outcome is
          // announced below either way.
          aria-label="Copy the requester link"
          className="amp-btn amp-btn-sm flex-shrink-0"
        >
          {copied === 'copied'
            ? <><Check size={12} className="text-success-ink" aria-hidden="true" />Copied</>
            : <><Copy size={12} aria-hidden="true" />Copy link</>}
        </button>
      </div>
      <p role="status" className="m-0 min-h-[1em] text-[11.5px] leading-snug text-ink-3" data-testid="requester-link-status">
        {copied === 'copied' && 'Link copied. Send it to them in whatever way you already reach them.'}
        {copied === 'failed' && 'Your browser refused the clipboard — select the link above and copy it.'}
      </p>
      <p className="m-0 text-[11.5px] leading-snug text-ink-3" data-testid="requester-link-note">
        Nothing is emailed, not by this panel and not by a requester invitation:{' '}
        <b className="text-ink-2">this link is what the requester needs</b>, so send it to them yourself. Someone with
        no AMP account still has to be invited below first — the link opens the form, it does not let them in.
      </p>
    </section>
  )
}

/* ── the shared refusal line ──────────────────────────────────────────────── */

function SectionError({ failure, section }: { failure: { section: Section; message: string } | null; section: Section }) {
  if (!failure || failure.section !== section) return null
  return (
    <p role="alert" className="m-0 flex items-start gap-1.5 text-[12px] leading-snug text-danger-ink">
      <AlertTriangle size={12} className="flex-shrink-0 mt-[2px]" aria-hidden="true" />
      <span className="min-w-0">{failure.message}</span>
    </p>
  )
}

/**
 * The separator between two facts on a row.
 *
 * It is rendered *inside* the chunk it introduces rather than as its own flex
 * item: a standalone separator is a wrap opportunity of its own, and at 390 px
 * that left a line ending in a lone "·" with the fact it belonged to on the
 * next line.
 */
function Middot() {
  return <span aria-hidden="true" className="mr-1.5">·</span>
}

/* ── who can send ─────────────────────────────────────────────────────────── */

/**
 * The grants, in force first.
 *
 * A revoked row is kept and marked, not hidden: "who was taken away" is half of
 * the question a manager brings here, and a list that silently dropped them
 * would make a revocation look like a grant that never existed. Revoking is a
 * quiet confirm-then-act (`ConfirmButton`), because it is reversed by granting
 * again.
 */
function GrantList({ grants, meId, busy, canManage, onRevoke, now }: {
  grants: TeamSubmitGrantee[]
  meId?: number
  busy: Busy
  canManage: boolean
  onRevoke: (g: TeamSubmitGrantee) => void
  now: number
}) {
  if (grants.length === 0) {
    return (
      <p className="m-0 text-[12.5px] text-ink-3" data-testid="no-submit-grants">
        Nobody has a submit grant on this inbox. Team members can still send requests — being on the team is enough.
      </p>
    )
  }
  return (
    <ul className="m-0 p-0 list-none border border-line overflow-hidden" style={{ borderRadius: 'var(--radius)' }}>
      {grants.map((g, i) => {
        const state = grantState(g)
        const revoked = state === 'revoked'
        const personBusy = busy?.key === `grant:${g.user_id}`
        return (
          <li key={`${g.user_id}:${g.granted_at}`} data-state={state}
            className={`flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 ${i > 0 ? 'border-t border-line' : ''}`}>
            <span className="min-w-0 flex-1 grid gap-0.5 leading-tight">
              <span className="flex items-baseline gap-1.5 min-w-0">
                <span className={`text-[13px] truncate ${revoked ? 'text-ink-2' : 'text-ink font-medium'}`}
                  title={granteeName(g)}>{granteeName(g)}</span>
                {meId != null && g.user_id === meId && <span className="text-[11px] text-ink-3 flex-shrink-0">(you)</span>}
                {revoked && (
                  <span className="inline-flex items-center gap-1 flex-shrink-0 border px-1.5 py-[2px] font-label text-[9.5px] font-semibold uppercase tracking-[0.1em]"
                    style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line-str)', color: 'var(--ink-3)' }}>
                    <Ban size={9} aria-hidden="true" />revoked {dayLabel(g.revoked_at, now)}
                  </span>
                )}
              </span>
              {/* Wraps rather than truncates: "can send requests" is the answer to
                  the question this screen exists to ask, so a phone must not turn
                  it into an ellipsis. Each separator sits INSIDE the chunk that
                  follows it, so a wrapped line can never end on a dangling middot
                  (a separator that is its own flex item can). */}
              <span className="text-[11.5px] text-ink-3 flex flex-wrap gap-x-1.5 min-w-0">
                <span className="break-all">{g.email || `User #${g.user_id}`}</span>
                <span className="whitespace-nowrap"><Middot />granted {dayLabel(g.granted_at, now)}</span>
                {!revoked && <span className="whitespace-nowrap"><Middot />can send requests</span>}
              </span>
            </span>
            {!revoked && canManage && (
              <ConfirmButton label="Revoke" confirmLabel="Revoke submit access" danger busy={personBusy}
                onConfirm={() => onRevoke(g)} />
            )}
          </li>
        )
      })}
    </ul>
  )
}

/** Grant one person submit access, by email or AMP user id — the two ways the server resolves a person. */
function GrantForm({ busy, value, onChange, onSubmit }: {
  busy: boolean
  value: string
  onChange: (v: string) => void
  onSubmit: (who: string) => void
}) {
  const id = 'submit-grant-who'
  const hintId = `${id}-hint`
  const errId = `${id}-error`
  const [missing, setMissing] = useState(false)
  return (
    <form className="grid gap-1.5" noValidate onSubmit={e => {
      e.preventDefault()
      const who = value.trim()
      // Not a disabled button: an empty submit says what is missing and puts the
      // caret in the field, rather than a control that refuses to explain itself
      // (design/ux-patterns.md, "Forms").
      if (!who) { setMissing(true); return }
      setMissing(false)
      onSubmit(who)
    }}>
      <label htmlFor={id} className="text-[12px] text-ink-2">Give someone submit access</label>
      <div className="flex flex-wrap items-start gap-2">
        <input id={id} value={value} onChange={e => { onChange(e.target.value); if (missing) setMissing(false) }}
          aria-describedby={missing ? `${hintId} ${errId}` : hintId} aria-invalid={missing || undefined}
          placeholder="email@example.com or AMP user id"
          className="amp-input flex-1 min-w-[200px]" autoComplete="off" />
        <button type="submit" disabled={busy} className="amp-btn amp-btn-sm amp-btn-primary flex-shrink-0">
          {busy ? <Loader2 size={12} className="animate-spin" aria-hidden="true" /> : <UserPlus size={12} aria-hidden="true" />}
          Grant submit access
        </button>
      </div>
      <p id={hintId} className="m-0 text-[11.5px] text-ink-3 leading-snug">
        They must already have an AMP account, or be invited below first. This adds no role and no project access.
      </p>
      {missing && <p id={errId} role="alert" className="m-0 text-[12px] text-danger-ink">
        Enter their email address or AMP user id.
      </p>}
    </form>
  )
}

/* ── invited requesters ───────────────────────────────────────────────────── */

function InviteList({ invites, busy, onRevoke, now }: {
  invites: Invite[]; busy: Busy; onRevoke: (i: Invite) => void; now: number
}) {
  if (invites.length === 0) {
    return <p className="m-0 text-[12.5px] text-ink-3" data-testid="no-requester-invites">No invitations are waiting on a first sign-in.</p>
  }
  return (
    <ul className="m-0 p-0 list-none border border-line overflow-hidden" style={{ borderRadius: 'var(--radius)' }}>
      {invites.map((inv, i) => {
        const personBusy = busy?.key === `invite:${inv.id}`
        return (
          <li key={inv.id} className={`flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 ${i > 0 ? 'border-t border-line' : ''}`}>
            <Mail size={13} className="flex-shrink-0 text-ink-3" aria-hidden="true" />
            <span className="min-w-0 flex-1 grid gap-0.5 leading-tight">
              <span className="text-[13px] text-ink break-all" title={inv.email}>{inv.email}</span>
              <span className="text-[11.5px] text-ink-3 flex flex-wrap gap-x-1.5">
                <span className="whitespace-nowrap">invited {dayLabel(inv.created_at, now)}</span>
                <span className="whitespace-nowrap"><Middot />customer account on first sign-in</span>
              </span>
            </span>
            <ConfirmButton label="Withdraw" confirmLabel="Withdraw invitation" danger busy={personBusy}
              onConfirm={() => onRevoke(inv)} />
          </li>
        )
      })}
    </ul>
  )
}

function InviteForm({ busy, value, onChange, onSubmit }: {
  busy: boolean
  value: string
  onChange: (v: string) => void
  onSubmit: (email: string) => void
}) {
  const id = 'submit-invite-email'
  const hintId = `${id}-hint`
  const errId = `${id}-error`
  const [missing, setMissing] = useState(false)
  return (
    <form className="grid gap-1.5" noValidate onSubmit={e => {
      e.preventDefault()
      const email = value.trim()
      if (!email) { setMissing(true); return }
      setMissing(false)
      onSubmit(email)
    }}>
      <label htmlFor={id} className="text-[12px] text-ink-2">Invite a requester</label>
      <div className="flex flex-wrap items-start gap-2">
        <input id={id} type="email" inputMode="email" value={value}
          onChange={e => { onChange(e.target.value); if (missing) setMissing(false) }}
          aria-describedby={missing ? `${hintId} ${errId}` : hintId} aria-invalid={missing || undefined}
          placeholder="customer@example.com"
          className="amp-input flex-1 min-w-[200px]" autoComplete="off" />
        <button type="submit" disabled={busy} className="amp-btn amp-btn-sm flex-shrink-0">
          {busy ? <Loader2 size={12} className="animate-spin" aria-hidden="true" /> : <Plus size={12} aria-hidden="true" />}
          Invite requester
        </button>
      </div>
      <p id={hintId} className="m-0 text-[11.5px] text-ink-3 leading-snug">
        An email that already has an AMP account can’t be invited as a requester — grant it submit access instead.
      </p>
      {missing && <p id={errId} role="alert" className="m-0 text-[12px] text-danger-ink">
        Enter the email address to invite.
      </p>}
    </form>
  )
}

/* ── submit-only credentials ──────────────────────────────────────────────── */

function CredentialList({ tokens, busy, onRevoke, now }: {
  tokens: APIToken[]; busy: Busy; onRevoke: (t: APIToken) => void; now: number
}) {
  if (tokens.length === 0) {
    return (
      <p className="m-0 text-[12.5px] text-ink-3" data-testid="no-submit-credentials">
        No submit-only credentials for this inbox. A connector or script that only sends requests needs one of these,
        not a personal token.
      </p>
    )
  }
  return (
    <ul className="m-0 p-0 list-none border border-line overflow-hidden" style={{ borderRadius: 'var(--radius)' }}>
      {tokens.map((t, i) => {
        const personBusy = busy?.key === `token:${t.id}`
        const expired = credentialExpired(t, now)
        return (
          <li key={t.id} data-token-id={t.id} className={`flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 ${i > 0 ? 'border-t border-line' : ''}`}>
            <KeyRound size={13} className="flex-shrink-0 text-ink-3" aria-hidden="true" />
            <span className="min-w-0 flex-1 grid gap-0.5 leading-tight">
              <span className="flex items-baseline gap-1.5 min-w-0">
                <span className="text-[13px] text-ink truncate" title={t.name}>{t.name}</span>
                <span className="amp-num text-[11px] text-ink-3 flex-shrink-0">{tokenPrefix(t)}</span>
              </span>
              <span className="text-[11.5px] text-ink-3 flex flex-wrap gap-x-1.5">
                <span className="whitespace-nowrap">last used {dayLabel(t.last_used_at, now)}</span>
                <span className="whitespace-nowrap">
                  <Middot />{expired ? 'expired' : t.expires_at ? <>expires {dayLabel(t.expires_at, now)}</> : 'no expiry'}
                </span>
              </span>
            </span>
            <ConfirmButton label="Revoke" confirmLabel="Revoke credential" danger busy={personBusy}
              onConfirm={() => onRevoke(t)} />
          </li>
        )
      })}
    </ul>
  )
}

function MintForm({ busy, value, onChange, onSubmit }: {
  busy: boolean
  value: string
  onChange: (v: string) => void
  onSubmit: (name: string) => void
}) {
  const id = 'submit-token-name'
  const hintId = `${id}-hint`
  const errId = `${id}-error`
  const [missing, setMissing] = useState(false)
  return (
    <form className="grid gap-1.5" noValidate onSubmit={e => {
      e.preventDefault()
      const name = value.trim()
      if (!name) { setMissing(true); return }
      setMissing(false)
      onSubmit(name)
    }}>
      <label htmlFor={id} className="text-[12px] text-ink-2">Mint a submit-only credential</label>
      <div className="flex flex-wrap items-start gap-2">
        <input id={id} value={value} onChange={e => { onChange(e.target.value); if (missing) setMissing(false) }}
          aria-describedby={missing ? `${hintId} ${errId}` : hintId} aria-invalid={missing || undefined}
          placeholder="Where it’s used, e.g. support-portal connector"
          className="amp-input flex-1 min-w-[200px]" autoComplete="off" />
        <button type="submit" disabled={busy} className="amp-btn amp-btn-sm flex-shrink-0">
          {busy ? <Loader2 size={12} className="animate-spin" aria-hidden="true" /> : <KeyRound size={12} aria-hidden="true" />}
          Mint credential
        </button>
      </div>
      <p id={hintId} className="m-0 text-[11.5px] text-ink-3 leading-snug">
        It belongs to you, bound to this inbox, and lives {SUBMIT_TOKEN_DAYS} days. The secret is shown once and never stored.
      </p>
      {missing && <p id={errId} role="alert" className="m-0 text-[12px] text-danger-ink">
        Name where the credential will be used, e.g. “support-portal connector”.
      </p>}
    </form>
  )
}
