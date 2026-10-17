import { useCallback, useEffect, useState } from 'react'
import { Loader2, Plus, Power } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import type { AmpUser, APIToken, NewAPIToken } from '../types'
import { CopyButton, Notice, Pill, SecretReveal, When } from '../components/admin'
import { errMsg } from '../lib/adminStyles'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { Avatar } from '../components/ui/Avatar'
import { ConfirmButton } from '../components/ui/ConfirmButton'

// /access — API tokens and agent accounts.
//
// A token lets an agent harness reach AMP's MCP endpoint without a browser
// login. Your own tokens act as you. An agent account is a separate AMP
// user for an autonomous agent: its work is its own, it sits on the teams
// you put it on, and you manage its tokens and can deactivate it.

const mcpURL = () => `${window.location.origin}/mcp`
const thCls = 'amp-label text-left px-3 py-2.5 border-b border-line whitespace-nowrap'
const tdCls = 'px-3 py-2 border-b border-line'

/** How to point an MCP client at AMP with a token. */
function McpSetup({ token }: { token: string }) {
  const claude = `claude mcp add --transport http amp ${mcpURL()} --header "Authorization: Bearer ${token}"`
  const json = JSON.stringify({ type: 'http', url: mcpURL(), headers: { Authorization: `Bearer ${token}` } }, null, 2)
  return (
    <div className="grid gap-2">
      <span className="amp-label">Claude Code</span>
      <div className="flex items-start gap-2">
        <code className="flex-1 min-w-0 px-3 py-2 text-[11.5px] amp-num text-ink break-all bg-surface-2 border border-line">{claude}</code>
        <CopyButton text={claude} />
      </div>
      <span className="amp-label pt-1">Other MCP clients · server entry</span>
      <div className="flex items-start gap-2">
        <pre className="m-0 flex-1 min-w-0 px-3 py-2 text-[11.5px] amp-num text-ink overflow-x-auto bg-surface-2 border border-line">{json}</pre>
        <CopyButton text={json} />
      </div>
    </div>
  )
}

/** Tokens of one account (yours, or an agent you manage). */
function Tokens({ user, label }: { user: number | 'me'; label: string }) {
  const [tokens, setTokens] = useState<APIToken[] | null>(null)
  const [name, setName] = useState('')
  const [days, setDays] = useState(90)
  const [fresh, setFresh] = useState<NewAPIToken | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(() => {
    api.listTokens(user).then(setTokens).catch(e => setError(errMsg(e, 'Failed to load tokens')))
  }, [user])
  useEffect(load, [load])

  const create = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true); setError(null)
    try {
      const t = await api.createToken(user, name.trim(), days)
      setFresh(t); setName(''); load()
    } catch (err) { setError(errMsg(err, 'Create failed')) } finally { setBusy(false) }
  }
  const revoke = async (t: APIToken) => {
    setError(null)
    try { await api.revokeToken(t.id); load() } catch (err) { setError(errMsg(err, 'Revoke failed')) }
  }

  const live = (tokens ?? []).filter(t => !t.revoked_at)
  const revoked = (tokens ?? []).length - live.length
  return (
    <div className="grid gap-3">
      {fresh && (
        <SecretReveal title={`New token for ${label}: ${fresh.name}`} secret={fresh.token} onDone={() => setFresh(null)}>
          <McpSetup token={fresh.token} />
        </SecretReveal>
      )}
      {tokens == null ? <Loader2 size={14} className="text-ink-3 animate-spin" /> : live.length === 0 ? (
        <p className="m-0 text-[13px] text-ink-2">No tokens yet. Create one to connect {label === 'you' ? 'an agent harness that works as you' : 'this agent'}.</p>
      ) : (
        <table className="w-full border-collapse text-[12.5px] border border-line">
          <thead><tr className="bg-surface-2">{['Name', 'Prefix', 'Last used', 'Expires', ''].map((h, i) => <th key={i} className={thCls}>{h}</th>)}</tr></thead>
          <tbody>
            {live.map(t => {
              const expired = t.expires_at && new Date(t.expires_at) < new Date()
              return (
                <tr key={t.id}>
                  <td className={`${tdCls} font-medium text-ink`}>{t.name}</td>
                  <td className={`${tdCls} amp-num text-ink-2`}>{t.prefix}…</td>
                  <td className={`${tdCls} amp-num text-ink-2`}><When at={t.last_used_at} /></td>
                  <td className={`${tdCls} amp-num text-ink-2`}>{expired ? <Pill tone="red">expired</Pill> : t.expires_at ? <When at={t.expires_at} /> : 'never'}</td>
                  <td className={`${tdCls} text-right`}><ConfirmButton label="Revoke" confirmLabel="Revoke now" danger onConfirm={() => revoke(t)} /></td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
      {revoked > 0 && <p className="m-0 text-[11.5px] text-ink-3">{revoked} revoked token{revoked === 1 ? '' : 's'} not shown.</p>}
      <form onSubmit={create} className="flex flex-wrap items-center gap-2">
        <label htmlFor={`token-name-${user}`} className="sr-only">Token name</label>
        <input id={`token-name-${user}`} value={name} onChange={e => setName(e.target.value)} required placeholder="Where it's used, e.g. build-box opencode"
          className="amp-input flex-1 min-w-[220px]" />
        <select value={days} onChange={e => setDays(Number(e.target.value))} className="amp-input !w-auto" aria-label="Expiry">
          <option value={30}>30 days</option>
          <option value={90}>90 days</option>
          <option value={365}>1 year</option>
          <option value={0}>No expiry</option>
        </select>
        <button type="submit" disabled={busy || !name.trim()} className="amp-btn amp-btn-primary">
          {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}New token
        </button>
      </form>
      <Notice error={error} />
    </div>
  )
}

function used(a: AmpUser) {
  // last_seen_at starts at creation; it moves when the agent first makes a request.
  return new Date(a.last_seen_at).getTime() - new Date(a.created_at).getTime() > 60_000
}

export function Access() {
  const { me, loading: authLoading, isAdmin } = useAuth()
  const [agents, setAgents] = useState<AmpUser[] | null>(null)
  const [selected, setSelected] = useState<number | 'me'>('me')
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState('')
  const [teamIds, setTeamIds] = useState<number[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const teams = me?.teams ?? []
  const orgName = (id: number) => me?.orgs?.find(o => o.id === id)?.name ?? `org ${id}`

  useEffect(() => {
    if (me?.user_id) api.listAgents().then(setAgents).catch(e => setError(errMsg(e, 'Failed to load agents')))
  }, [me?.user_id])

  const create = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true); setError(null)
    try {
      const a = await api.createAgent(name.trim(), teamIds)
      setAgents(list => [a, ...(list ?? [])]); setName(''); setTeamIds([]); setCreating(false); setSelected(a.id)
    } catch (err) { setError(errMsg(err, 'Create failed')) } finally { setBusy(false) }
  }
  const replace = (u: AmpUser) => setAgents(list => (list ?? []).map(x => x.id === u.id ? u : x))
  const toggle = async (a: AmpUser) => {
    setBusy(true); setError(null)
    try { replace(a.disabled_at ? await api.reactivateUser(a.id) : await api.deactivateUser(a.id)) }
    catch (err) { setError(errMsg(err, 'Failed')) } finally { setBusy(false) }
  }

  const agent = selected === 'me' ? null : (agents ?? []).find(a => a.id === selected) ?? null
  const rowCls = (on: boolean) => `cursor-pointer ${on ? 'bg-[color-mix(in_srgb,var(--accent)_6%,var(--surface))]' : 'hover:bg-surface-2'}`

  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Overview" title="Agents & tokens"
        meta="Give each agent its own account and token, or use a token that works as you."
        actions={me?.user_id ? <button type="button" className="amp-btn amp-btn-primary" onClick={() => setCreating(v => !v)} aria-expanded={creating}><Plus size={12} />New agent</button> : undefined}>
        {creating && (
          <form onSubmit={create} className="mx-7 mb-4 amp-panel p-4 grid gap-3">
            <span className="amp-label">New agent account · its own AMP user, on the teams you pick</span>
            <label htmlFor="agent-name" className="sr-only">Agent name</label>
            <input id="agent-name" value={name} onChange={e => setName(e.target.value)} required placeholder="Agent name, e.g. claude-build-3" className="amp-input" autoFocus />
            {teams.length > 0 ? (
              <div className="grid gap-1.5">
                <span className="text-[12px] text-ink-2">Put it on these teams (yours):</span>
                <div className="flex flex-wrap gap-1.5">
                  {teams.map(t => {
                    const on = teamIds.includes(t.id)
                    return (
                      <button type="button" key={t.id} aria-pressed={on} onClick={() => setTeamIds(ids => on ? ids.filter(i => i !== t.id) : [...ids, t.id])}
                        className={`px-2 py-1 text-[12px] border cursor-pointer ${on ? 'bg-ink text-on-ink border-ink' : 'bg-surface text-ink-2 border-line hover:border-ink'}`}>
                        {t.name} <span className={on ? 'opacity-70' : 'text-ink-3'}>· {orgName(t.org_id)}</span>
                      </button>
                    )
                  })}
                </div>
              </div>
            ) : (
              <p className="m-0 text-[12px] text-ink-3">You aren't on any team yet, so the agent starts with no project access. Add it to a team on Teams & orgs.</p>
            )}
            <div className="flex gap-2">
              <button type="submit" disabled={busy || !name.trim()} className="amp-btn amp-btn-primary">
                {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}Create agent</button>
              <button type="button" className="amp-btn amp-btn-ghost" onClick={() => setCreating(false)}>Cancel</button>
            </div>
          </form>
        )}
      </PageHeader>

      <div className="p-7 pt-5 grid gap-4">
        {error && <p className="m-0 amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</p>}
        {authLoading ? <Loader2 size={18} className="text-ink-3 animate-spin" /> : !me?.user_id ? (
          <p className="m-0 amp-panel px-4 py-4 text-[13px] text-ink-2">Sign in to manage tokens and agent accounts.</p>
        ) : (
          <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
            <section className="amp-panel min-w-0" aria-label="Accounts">
              <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
                <span className="amp-label">Accounts</span>
                <h2 className="m-0 text-[14.5px] font-semibold text-ink">{isAdmin ? 'You and every agent' : 'You and your agents'}</h2>
              </div>
              <table className="w-full border-collapse text-[13px]">
                <thead><tr className="bg-surface-2">{['Account', 'Last active', 'Status'].map(h => <th key={h} className={thCls}>{h}</th>)}</tr></thead>
                <tbody>
                  <tr className={rowCls(selected === 'me')} onClick={() => setSelected('me')} aria-selected={selected === 'me'}>
                    <td className={tdCls} style={selected === 'me' ? { boxShadow: 'inset 3px 0 0 var(--accent)' } : undefined}>
                      <span className="flex items-center gap-2.5"><Avatar name={me.name || me.email} kind="human" />
                        <span className="min-w-0"><span className="block font-semibold text-ink">You</span><span className="block amp-num text-[11px] text-ink-3 truncate">tokens that act as {me.email}</span></span></span>
                    </td>
                    <td className={`${tdCls} amp-num text-[12px] text-ink-2`}>now</td>
                    <td className={tdCls}><span className="inline-flex items-center gap-1.5 text-[12px] text-ink-2"><i className="w-2 h-2 bg-success inline-block" />Active</span></td>
                  </tr>
                  {agents == null && <tr><td colSpan={3} className="px-4 py-4"><Loader2 size={14} className="text-ink-3 animate-spin" /></td></tr>}
                  {(agents ?? []).map(a => (
                    <tr key={a.id} className={rowCls(selected === a.id)} onClick={() => setSelected(a.id)} aria-selected={selected === a.id}>
                      <td className={tdCls} style={selected === a.id ? { boxShadow: 'inset 3px 0 0 var(--accent)' } : undefined}>
                        <span className="flex items-center gap-2.5"><Avatar name={a.display_name || a.email} kind="agent" />
                          <span className="min-w-0"><span className="block font-semibold text-ink truncate">{a.display_name}</span><span className="block amp-num text-[11px] text-ink-3 truncate">{a.email}</span></span></span>
                      </td>
                      <td className={`${tdCls} amp-num text-[12px] text-ink-2`}>{used(a) ? <When at={a.last_seen_at} /> : 'never'}</td>
                      <td className={tdCls}>{a.disabled_at
                        ? <span className="font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] text-danger-ink">Deactivated</span>
                        : <span className="inline-flex items-center gap-1.5 text-[12px] text-ink-2"><i className="w-2 h-2 bg-success inline-block" />Active</span>}</td>
                    </tr>
                  ))}
                  {agents?.length === 0 && <tr><td colSpan={3} className="px-4 py-4 text-[13px] text-ink-2">No agent accounts yet. <button type="button" className="underline cursor-pointer" onClick={() => setCreating(true)}>Create one</button> to give an agent its own identity.</td></tr>}
                </tbody>
              </table>
            </section>

            <div className="grid grid-cols-1 gap-4 min-w-0">
              {agent == null ? (
                <Panel label="Your tokens" title="Tokens that act as you" top>
                  <div className="px-4 py-4 grid gap-3">
                    <p className="m-0 text-[12.5px] text-ink-2 leading-relaxed">For an agent harness that works as you, without a browser login. It connects to AMP's MCP endpoint with the token and sees exactly what you see. Prefer an agent account for agents that work on their own.</p>
                    <Tokens key="me" user="me" label="you" />
                  </div>
                </Panel>
              ) : (
                <>
                  <section className="amp-panel amp-panel-top">
                    <div className="flex items-center gap-3 px-4 py-4">
                      <Avatar name={agent.display_name || agent.email} kind="agent" size={36} />
                      <div className="min-w-0 flex-1"><div className="text-[16px] font-semibold text-ink truncate">{agent.display_name}</div>
                        <div className="amp-num text-[11.5px] text-ink-3 truncate flex items-center gap-1">{agent.email}<CopyButton text={agent.email} label="" /></div></div>
                      {agent.disabled_at && <Pill tone="red">deactivated</Pill>}
                    </div>
                    <dl className="m-0 grid grid-cols-[100px_1fr] gap-x-3 gap-y-2 px-4 pb-4 text-[13px] items-baseline">
                      <dt className="amp-label">Kind</dt><dd className="m-0">Agent account · its work and claims are its own</dd>
                      <dt className="amp-label">Created</dt><dd className="m-0 amp-num"><When at={agent.created_at} /></dd>
                      <dt className="amp-label">Last active</dt><dd className="m-0 amp-num">{used(agent) ? <When at={agent.last_seen_at} /> : 'not used yet'}</dd>
                      <dt className="amp-label">Teams</dt><dd className="m-0 text-ink-2">Manage on Teams & orgs</dd>
                    </dl>
                  </section>
                  {!agent.disabled_at && (
                    <Panel label="API tokens" title="Tokens for this agent">
                      <div className="px-4 py-4"><Tokens key={agent.id} user={agent.id} label={agent.display_name || agent.email} /></div>
                    </Panel>
                  )}
                  <section className="border border-danger p-4 grid gap-2.5 bg-surface" aria-label="Danger zone">
                    <span className="amp-label" style={{ color: 'var(--danger-ink)' }}>Danger zone</span>
                    <p className="m-0 text-[12.5px] text-ink-2">{agent.disabled_at
                      ? 'Deactivated: every request it makes is refused. Reactivating lets it back in with its tokens as they were.'
                      : 'Deactivating refuses every request from this agent at once and stops new work going to it. Its tickets and history stay.'}</p>
                    <div>{agent.disabled_at
                      ? <button type="button" className="amp-btn amp-btn-sm" disabled={busy} onClick={() => toggle(agent)}><Power size={12} />Reactivate</button>
                      : <ConfirmButton label="Deactivate" confirmLabel="Deactivate now" danger busy={busy} icon={<Power size={12} />} onConfirm={() => toggle(agent)} />}</div>
                  </section>
                </>
              )}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
