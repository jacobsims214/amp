/**
 * People & agents (global admins). One table of every account, filterable
 * and searchable; selecting one shows everything about it in one place:
 * role, sign-in, API tokens, recent audit entries, and a separate danger
 * zone. Password sign-ins live in Dex (via amp-authadmin), roles and
 * accounts in amp-api; each loads on its own so one failing doesn't hide
 * the other.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Search, UserPlus } from 'lucide-react'
import { AuditLogPanel } from '../components/AuditLogPanel'
import { api, authAdminApi } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { Avatar } from '../components/ui/Avatar'
import { ConfirmButton } from '../components/ui/ConfirmButton'
import type { AmpUser, APIToken, AuthAdminUser } from '../types'
import { BandLoader } from '../components/brand/BandLoader'

type Filter = 'all' | 'people' | 'agents' | 'off'

function ago(iso?: string): string {
  if (!iso) return '—'
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 3_600_000) return `${Math.max(1, Math.round(ms / 60_000))}m`
  if (ms < 172_800_000) return `${Math.round(ms / 3_600_000)}h`
  return `${Math.round(ms / 86_400_000)}d`
}

const name = (u: AmpUser) => u.display_name || u.email.split('@')[0]

export function Users() {
  const { me, loading: authLoading, isAdmin } = useAuth()
  const [users, setUsers] = useState<AmpUser[]>([])
  const [credentials, setCredentials] = useState<AuthAdminUser[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [credError, setCredError] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [filter, setFilter] = useState<Filter>('all')
  const [query, setQuery] = useState('')
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [tokens, setTokens] = useState<APIToken[]>([])
  const [adding, setAdding] = useState(false)
  const [newLogin, setNewLogin] = useState({ email: '', name: '', password: '' })
  const [resetPassword, setResetPassword] = useState('')

  const reload = useCallback(() => {
    Promise.allSettled([api.listUsers(), authAdminApi.listCredentials()]).then(([u, c]) => {
      if (u.status === 'fulfilled') { setUsers(u.value); setError(null) } else setError(String(u.reason?.message ?? u.reason))
      if (c.status === 'fulfilled') { setCredentials(c.value); setCredError(null) }
      else { setCredentials(null); setCredError('The sign-in service didn\'t answer, so password sign-ins can\'t be listed or changed right now.') }
    }).finally(() => setLoading(false))
  }, [])
  useEffect(() => { if (isAdmin) reload() }, [isAdmin, reload])

  const selected = users.find(u => u.id === selectedId) ?? null
  useEffect(() => {
    if (selectedId == null) return
    api.listTokens(selectedId).then(setTokens).catch(() => setTokens([]))
  }, [selectedId])

  const byId = useMemo(() => new Map(users.map(u => [u.id, u])), [users])
  const hasLogin = (u: AmpUser) => credentials?.some(c => c.email.toLowerCase() === u.email.toLowerCase())
  const counts = {
    all: users.length,
    people: users.filter(u => u.kind !== 'agent').length,
    agents: users.filter(u => u.kind === 'agent').length,
    off: users.filter(u => u.disabled_at).length,
  }
  const shown = users
    .filter(u => filter === 'all' || (filter === 'people' && u.kind !== 'agent') || (filter === 'agents' && u.kind === 'agent') || (filter === 'off' && u.disabled_at))
    .filter(u => !query || `${u.email} ${u.display_name}`.toLowerCase().includes(query.toLowerCase()))
    .sort((a, b) => Number(!!a.disabled_at) - Number(!!b.disabled_at) || (b.last_seen_at ?? '').localeCompare(a.last_seen_at ?? ''))

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true); setActionError(null)
    try { await fn(); reload(); if (selectedId != null) api.listTokens(selectedId).then(setTokens).catch(() => {}) }
    catch (e) { setActionError(e instanceof Error ? e.message : 'That didn\'t work') }
    finally { setBusy(false) }
  }

  if (authLoading) return <div className="flex items-center justify-center h-full"><BandLoader /></div>
  if (!isAdmin) {
    return (
      <div className="flex flex-col min-h-full">
        <PageHeader crumb="Admin" title="People & agents" />
        <p className="m-7 amp-panel px-4 py-4 text-[13.5px] text-ink-2">Only global admins can manage people and agents. <Link to="/admin/orgs" className="underline">Teams & orgs</Link> lists the people in your orgs.</p>
      </div>
    )
  }

  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Admin" title="People & agents"
        meta={`${counts.people} people · ${counts.agents} agents${counts.off ? ` · ${counts.off} deactivated` : ''}`}
        actions={<>
          <button type="button" className="amp-btn" onClick={() => setAdding(v => !v)} aria-expanded={adding}><UserPlus size={12} />Add sign-in</button>
          <Link to="/access" className="amp-btn amp-btn-primary">New agent</Link>
        </>}>
        {adding && (
          <form className="mx-7 mb-4 amp-panel p-4 grid gap-3" onSubmit={e => {
            e.preventDefault()
            act(async () => {
              await authAdminApi.createCredential(newLogin.email.trim().toLowerCase(), newLogin.password, newLogin.name.trim() || undefined)
              setNewLogin({ email: '', name: '', password: '' }); setAdding(false)
            })
          }}>
            <span className="amp-label">New password sign-in · stored in Dex, never in AMP</span>
            <div className="grid gap-2 grid-cols-1 md:grid-cols-3">
              <input className="amp-input" type="email" required placeholder="email@example.com" aria-label="Email" value={newLogin.email} onChange={e => setNewLogin({ ...newLogin, email: e.target.value })} />
              <input className="amp-input" placeholder="Display name (optional)" aria-label="Display name" value={newLogin.name} onChange={e => setNewLogin({ ...newLogin, name: e.target.value })} />
              <input className="amp-input" type="password" required minLength={8} placeholder="Temporary password" aria-label="Temporary password" value={newLogin.password} onChange={e => setNewLogin({ ...newLogin, password: e.target.value })} />
            </div>
            <p className="m-0 text-[12px] text-ink-2">They become an AMP member the first time they sign in. Add them to an org and team on <Link to="/admin/orgs" className="underline">Teams & orgs</Link>.</p>
            <div className="flex gap-2"><button type="submit" className="amp-btn amp-btn-primary" disabled={busy}>Create sign-in</button>
              <button type="button" className="amp-btn amp-btn-ghost" onClick={() => setAdding(false)}>Cancel</button></div>
          </form>
        )}
      </PageHeader>

      <div className="p-7 pt-5 grid gap-4">
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {credError && <div className="amp-panel px-4 py-3 text-[12.5px] text-ink-2">{credError}</div>}
        {actionError && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{actionError}</div>}

        <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-[minmax(0,1fr)_380px]">
          <section className="amp-panel min-w-0" aria-label="Accounts">
            <div className="flex flex-wrap items-center gap-3 px-4 py-3 border-b border-line">
              <div className="flex border border-line" role="group" aria-label="Show">
                {([['all', 'All'], ['people', 'People'], ['agents', 'Agents'], ['off', 'Deactivated']] as [Filter, string][]).map(([k, l]) => (
                  <button key={k} type="button" aria-pressed={filter === k} onClick={() => setFilter(k)}
                    className={`font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] px-2.5 py-2 border-l border-line first:border-l-0 cursor-pointer ${filter === k ? 'bg-ink text-on-ink' : 'bg-surface text-ink-2 hover:text-ink'}`}>
                    {l} · {counts[k]}</button>
                ))}
              </div>
              <label className="ml-auto flex items-center gap-2 border border-line px-2.5 py-1.5 min-w-[240px]">
                <Search size={13} className="text-ink-3" />
                <input value={query} onChange={e => setQuery(e.target.value)} placeholder="Search name or email" aria-label="Search accounts"
                  className="flex-1 bg-transparent outline-none text-[13px] text-ink placeholder:text-ink-3" />
              </label>
            </div>
            {loading ? <div className="flex justify-center py-10"><BandLoader label="Loading people and agents" /></div> : (
              <table className="w-full border-collapse text-[13px]">
                <thead><tr className="bg-surface-2">
                  {['Name', 'Kind', 'Role', 'Sign-in', 'Last active', 'Status'].map(h => <th key={h} className="amp-label text-left px-3 py-2.5 border-b border-line whitespace-nowrap">{h}</th>)}
                </tr></thead>
                <tbody>
                  {shown.map(u => {
                    const sel = u.id === selectedId
                    const agent = u.kind === 'agent'
                    const maker = u.managed_by ? byId.get(u.managed_by) : undefined
                    return (
                      <tr key={u.id} onClick={() => setSelectedId(u.id)} aria-selected={sel}
                        className={`cursor-pointer ${sel ? 'bg-[color-mix(in_srgb,var(--accent)_6%,var(--surface))]' : 'hover:bg-surface-2'}`}>
                        <td className="px-3 py-2 border-b border-line" style={sel ? { boxShadow: 'inset 3px 0 0 var(--accent)' } : undefined}>
                          <span className="flex items-center gap-2.5 min-w-0">
                            <Avatar name={name(u)} kind={u.kind} />
                            <span className="min-w-0"><span className="block font-semibold text-ink truncate">{name(u)}{u.id === me?.user_id && <span className="text-ink-3 font-normal"> (you)</span>}</span>
                              <span className="block amp-num text-[11px] text-ink-3 truncate">{u.email}</span></span>
                          </span>
                        </td>
                        <td className="px-3 border-b border-line">{agent ? 'Agent' : 'Person'}</td>
                        <td className="px-3 border-b border-line text-ink-2">{u.roles.includes('admin') ? <span className="text-ink font-semibold">Global admin</span>
                          : agent ? <>Agent{maker ? <> · made by {name(maker)}</> : null}</> : 'Member'}</td>
                        <td className="px-3 border-b border-line amp-num text-[12px] text-ink-2">{agent ? 'API tokens' : credentials == null ? '—' : hasLogin(u) ? 'password' : 'SSO'}</td>
                        <td className="px-3 border-b border-line amp-num text-[12px] text-ink-2">{ago(u.last_seen_at)}</td>
                        <td className="px-3 border-b border-line">{u.disabled_at
                          ? <span className="font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] text-danger-ink">Deactivated</span>
                          : <span className="inline-flex items-center gap-1.5 text-[12px] text-ink-2"><i className="w-2 h-2 bg-success inline-block" />Active</span>}</td>
                      </tr>
                    )
                  })}
                  {shown.length === 0 && <tr><td colSpan={6} className="px-4 py-6 text-center text-ink-2">No accounts match.</td></tr>}
                </tbody>
              </table>
            )}
          </section>

          <div className="grid grid-cols-1 gap-4 min-w-0">
            {!selected ? (
              <Panel label="Selected" title="Nobody selected"><p className="m-0 px-4 py-4 text-[13px] text-ink-2">Pick a person or agent to see their role, sign-in, tokens, and recent changes.</p></Panel>
            ) : <>
              <section className="amp-panel amp-panel-top">
                <div className="flex items-center gap-3 px-4 py-4">
                  <Avatar name={name(selected)} kind={selected.kind} size={36} />
                  <div className="min-w-0"><div className="text-[16px] font-semibold text-ink truncate">{name(selected)}</div>
                    <div className="amp-num text-[11.5px] text-ink-3 truncate">{selected.email}</div></div>
                </div>
                <dl className="m-0 grid grid-cols-[100px_1fr] gap-x-3 gap-y-2 px-4 pb-4 text-[13px] items-baseline">
                  <dt className="amp-label">Kind</dt><dd className="m-0">{selected.kind === 'agent' ? 'Agent account · signs in with API tokens' : 'Person · signs in through Dex'}</dd>
                  <dt className="amp-label">Role</dt><dd className="m-0">{selected.roles.includes('admin') ? 'Global admin' : 'Member'}</dd>
                  {selected.managed_by && <><dt className="amp-label">Made by</dt><dd className="m-0">{byId.get(selected.managed_by) ? name(byId.get(selected.managed_by)!) : `user ${selected.managed_by}`}</dd></>}
                  <dt className="amp-label">Joined</dt><dd className="m-0 amp-num">{new Date(selected.created_at).toLocaleDateString()}</dd>
                  <dt className="amp-label">Last active</dt><dd className="m-0 amp-num">{ago(selected.last_seen_at)} ago</dd>
                </dl>
                {selected.kind !== 'agent' && (
                  <div className="flex flex-wrap gap-2 px-4 pb-4">
                    <button type="button" className="amp-btn amp-btn-sm" disabled={busy || (selected.id === me?.user_id && selected.roles.includes('admin'))}
                      title={selected.id === me?.user_id ? 'You can\'t remove your own admin role here' : undefined}
                      onClick={() => act(() => api.setUserRole(selected.id, 'admin', !selected.roles.includes('admin')))}>
                      {selected.roles.includes('admin') ? 'Remove global admin' : 'Make global admin'}</button>
                  </div>
                )}
              </section>

              {selected.kind !== 'agent' && hasLogin(selected) && (
                <Panel label="Sign-in" title="Password">
                  <form className="flex gap-2 px-4 py-3" onSubmit={e => { e.preventDefault(); act(async () => { await authAdminApi.resetPassword(selected.email, resetPassword); setResetPassword('') }) }}>
                    <input className="amp-input" type="password" required minLength={8} placeholder="New password" aria-label="New password" value={resetPassword} onChange={e => setResetPassword(e.target.value)} />
                    <button type="submit" className="amp-btn amp-btn-sm" disabled={busy}>Reset</button>
                  </form>
                </Panel>
              )}

              <Panel label="API tokens" title={String(tokens.length)} right={selected.kind === 'agent' || selected.id === me?.user_id ? <Link to="/access" className="underline underline-offset-2">new token</Link> : undefined}>
                {tokens.length === 0 ? <p className="m-0 px-4 py-3 text-[13px] text-ink-2">No tokens.{selected.kind === 'agent' ? ' Create one on Agents & tokens to connect this agent.' : ''}</p> : (
                  <table className="w-full border-collapse text-[12.5px]">
                    <thead><tr className="bg-surface-2">{['Name', 'Prefix', 'Last used', ''].map((h, i) => <th key={i} className="amp-label text-left px-3 py-2 border-b border-line">{h}</th>)}</tr></thead>
                    <tbody>
                      {tokens.map(t => (
                        <tr key={t.id}>
                          <td className="px-3 py-2 border-b border-line">{t.name}</td>
                          <td className="px-3 border-b border-line amp-num">{t.prefix}…</td>
                          <td className="px-3 border-b border-line amp-num text-ink-2">{t.last_used_at ? `${ago(t.last_used_at)} ago` : 'never'}</td>
                          <td className="px-3 border-b border-line text-right"><ConfirmButton label="Revoke" confirmLabel="Revoke now" busy={busy} danger onConfirm={() => act(() => api.revokeToken(t.id))} /></td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </Panel>

              <Panel label="Audit" title="Recent changes by them">
                <div className="p-3"><AuditLogPanel key={selected.id} actorId={selected.id} /></div>
              </Panel>

              {selected.id !== me?.user_id && (
                <section className="border border-danger p-4 grid gap-2.5" aria-label="Danger zone">
                  <span className="amp-label" style={{ color: 'var(--danger-ink)' }}>Danger zone</span>
                  <p className="m-0 text-[12.5px] text-ink-2">{selected.disabled_at
                    ? 'This account is deactivated: every sign-in and token is refused. Reactivating lets it back in with its tokens as they were.'
                    : 'Deactivating refuses every sign-in and API token from this account at once and stops new work going to it. Its tickets and history stay.'}</p>
                  <div className="flex flex-wrap gap-2">
                    {selected.disabled_at
                      ? <button type="button" className="amp-btn amp-btn-sm" disabled={busy} onClick={() => act(() => api.reactivateUser(selected.id))}>Reactivate</button>
                      : <ConfirmButton label="Deactivate" confirmLabel="Deactivate now" danger busy={busy} onConfirm={() => act(() => api.deactivateUser(selected.id))} />}
                    {selected.kind !== 'agent' && hasLogin(selected) &&
                      <ConfirmButton label="Remove password sign-in" confirmLabel="Remove it" danger busy={busy} onConfirm={() => act(() => authAdminApi.deleteCredential(selected.email))} />}
                  </div>
                </section>
              )}
            </>}
          </div>
        </div>
      </div>
    </div>
  )
}
