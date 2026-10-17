import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowLeft, Bot, KeyRound, Loader2, Plus, Power, Trash2 } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import type { AmpUser, APIToken, NewAPIToken } from '../types'
import { AccountBadges, CopyButton, Notice, Pill, SecretReveal, When } from '../components/admin'
import { dangerBtnCls, errMsg, fieldCls, primaryBtnCls, quietBtnCls, smallSelectCls } from '../lib/adminStyles'

// /access — API tokens and agent accounts.
//
// A token lets an agent harness reach AMP's MCP endpoint without a browser
// login. Your own tokens act as you. An agent account is a separate AMP
// user for an autonomous agent: its work is its own, it sits on the teams
// you put it on, and you manage its tokens and can deactivate it.

const mcpURL = () => `${window.location.origin}/mcp`

function Section({ title, icon, description, children }: { title: string; icon: React.ReactNode; description: string; children: React.ReactNode }) {
  return (
    <section className="rounded-2xl p-5 space-y-4" style={{ background: '#0D1726', border: '1px solid #1E2C45' }}>
      <div>
        <h2 className="text-sm font-bold text-[#DDE6F0] flex items-center gap-2">{icon}{title}</h2>
        <p className="text-xs text-[#7E91A8] mt-1 leading-relaxed">{description}</p>
      </div>
      {children}
    </section>
  )
}

/** How to point an MCP client at AMP with a token. */
function McpSetup({ token }: { token: string }) {
  const claude = `claude mcp add --transport http amp ${mcpURL()} --header "Authorization: Bearer ${token}"`
  const json = JSON.stringify({ type: 'http', url: mcpURL(), headers: { Authorization: `Bearer ${token}` } }, null, 2)
  return (
    <div className="space-y-2">
      <p className="text-xs text-[#7E91A8]">Claude Code:</p>
      <div className="flex items-start gap-2">
        <code className="flex-1 min-w-0 px-3 py-2 rounded-lg text-[11px] font-mono text-[#DDE6F0] break-all" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>{claude}</code>
        <CopyButton text={claude} />
      </div>
      <p className="text-xs text-[#7E91A8]">Other MCP clients (server entry):</p>
      <div className="flex items-start gap-2">
        <pre className="flex-1 min-w-0 px-3 py-2 rounded-lg text-[11px] font-mono text-[#DDE6F0] overflow-x-auto" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>{json}</pre>
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
    if (!confirm(`Revoke "${t.name}"? Anything using it stops working at once.`)) return
    setError(null)
    try { await api.revokeToken(t.id); load() } catch (err) { setError(errMsg(err, 'Revoke failed')) }
  }

  const live = (tokens ?? []).filter(t => !t.revoked_at)
  const revoked = (tokens ?? []).length - live.length
  return (
    <div className="space-y-3">
      {fresh && (
        <SecretReveal title={`New token for ${label}: ${fresh.name}`} secret={fresh.token} onDone={() => setFresh(null)}>
          <McpSetup token={fresh.token} />
        </SecretReveal>
      )}
      {tokens == null ? <Loader2 size={14} className="text-[#6366F1] animate-spin" /> : live.length > 0 && (
        <ul className="rounded-xl border border-[#1E2C45] divide-y divide-[#1E2C45]">
          {live.map(t => {
            const expired = t.expires_at && new Date(t.expires_at) < new Date()
            return (
              <li key={t.id} className="px-3 py-2 flex items-center gap-3 text-xs">
                <KeyRound size={12} className="text-[#3D5068] flex-shrink-0" />
                <span className="text-[#DDE6F0] font-medium truncate">{t.name}</span>
                <span className="font-mono text-[#3D5068]">{t.prefix}…</span>
                {expired && <Pill tone="red">expired</Pill>}
                <span className="flex-1" />
                <span className="text-[#3D5068] hidden sm:inline">used <When at={t.last_used_at} /></span>
                <span className="text-[#3D5068] hidden md:inline">{t.expires_at ? <>expires <When at={t.expires_at} /></> : 'no expiry'}</span>
                <button onClick={() => revoke(t)} className={dangerBtnCls}><Trash2 size={12} />Revoke</button>
              </li>
            )
          })}
        </ul>
      )}
      {revoked > 0 && <p className="text-[11px] text-[#3D5068]">{revoked} revoked token{revoked === 1 ? '' : 's'} not shown.</p>}
      <form onSubmit={create} className="flex flex-wrap items-center gap-2">
        <input value={name} onChange={e => setName(e.target.value)} required placeholder="Where it's used, e.g. build-box opencode"
          className={`${fieldCls} py-1.5 flex-1 min-w-[220px]`} />
        <select value={days} onChange={e => setDays(Number(e.target.value))} className={smallSelectCls} aria-label="Expiry">
          <option value={30}>30 days</option>
          <option value={90}>90 days</option>
          <option value={365}>1 year</option>
          <option value={0}>No expiry</option>
        </select>
        <button type="submit" disabled={busy || !name.trim()} className={primaryBtnCls}>
          {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}New token
        </button>
      </form>
      <Notice error={error} />
    </div>
  )
}

function AgentCard({ agent, onChanged }: { agent: AmpUser; onChanged: (a: AmpUser) => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const disabled = !!agent.disabled_at
  const toggle = async () => {
    if (!disabled && !confirm(`Deactivate ${agent.display_name || agent.email}? Every request it makes will be refused until you reactivate it.`)) return
    setBusy(true); setError(null)
    try { onChanged(disabled ? await api.reactivateUser(agent.id) : await api.deactivateUser(agent.id)) }
    catch (e) { setError(errMsg(e, 'Failed')) } finally { setBusy(false) }
  }
  return (
    <li className="rounded-xl p-4 space-y-3" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>
      <div className="flex items-start gap-3">
        <Bot size={16} className="text-[#818CF8] mt-0.5 flex-shrink-0" />
        <div className="flex-1 min-w-0">
          <p className="text-sm font-semibold text-[#DDE6F0] flex items-center gap-2">{agent.display_name}<AccountBadges disabled={disabled} /></p>
          <p className="text-xs text-[#7E91A8] flex items-center gap-1">
            <span className="font-mono truncate">{agent.email}</span><CopyButton text={agent.email} label="" />
          </p>
          <p className="text-[11px] text-[#3D5068]">
            {/* last_seen_at starts at creation; it moves when the agent first makes a request */}
            {new Date(agent.last_seen_at).getTime() - new Date(agent.created_at).getTime() > 60_000
              ? <>last active <When at={agent.last_seen_at} /></> : <>created <When at={agent.created_at} />, not used yet</>} · user {agent.id}
          </p>
        </div>
        <button onClick={toggle} disabled={busy} className={disabled ? quietBtnCls : dangerBtnCls}>
          <Power size={12} />{disabled ? 'Reactivate' : 'Deactivate'}
        </button>
      </div>
      <Notice error={error} />
      {!disabled && <Tokens user={agent.id} label={agent.display_name || agent.email} />}
    </li>
  )
}

export function Access() {
  const { me, loading: authLoading, isAdmin } = useAuth()
  const [agents, setAgents] = useState<AmpUser[] | null>(null)
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
      setAgents(list => [a, ...(list ?? [])]); setName(''); setTeamIds([])
    } catch (err) { setError(errMsg(err, 'Create failed')) } finally { setBusy(false) }
  }

  return (
    <div className="min-h-screen" style={{ background: '#08101F' }}>
      <header style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45' }}>
        <div className="max-w-3xl mx-auto px-4 sm:px-6 py-4 flex items-center gap-3">
          <Link to="/" className="text-[#3D5068] hover:text-[#DDE6F0] transition-colors"><ArrowLeft size={16} /></Link>
          <KeyRound size={16} className="text-[#818CF8]" />
          <h1 className="text-base font-bold text-[#DDE6F0]">Agents &amp; API tokens</h1>
        </div>
      </header>
      <main className="max-w-3xl mx-auto px-4 sm:px-6 py-8 space-y-5">
        {authLoading ? <Loader2 size={18} className="text-[#6366F1] animate-spin" /> : !me?.user_id ? (
          <p className="text-sm text-[#7E91A8]">Sign in to manage tokens and agent accounts.</p>
        ) : (
          <>
            <Section title="Your API tokens" icon={<KeyRound size={14} className="text-[#818CF8]" />}
              description="For an agent harness that works as you, without a browser login: it connects to AMP's MCP endpoint with the token and sees exactly what you see. Prefer an agent account for agents that work on their own.">
              <Tokens user="me" label="you" />
            </Section>

            <Section title={isAdmin ? 'Agent accounts (all, as global admin)' : 'Your agent accounts'} icon={<Bot size={14} className="text-[#818CF8]" />}
              description="An agent account is its own AMP user: tickets it claims and work it does are its own, and it can see only what its teams see. You manage it: its tokens, its teams (from the Orgs page), and whether it's active.">
              <form onSubmit={create} className="space-y-3 rounded-xl p-4" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>
                <input value={name} onChange={e => setName(e.target.value)} required placeholder="Agent name, e.g. Build bot"
                  className={`${fieldCls} w-full`} />
                {teams.length > 0 ? (
                  <div>
                    <p className="text-xs text-[#7E91A8] mb-1.5">Put it on these teams (yours):</p>
                    <div className="flex flex-wrap gap-1.5">
                      {teams.map(t => {
                        const on = teamIds.includes(t.id)
                        return (
                          <button type="button" key={t.id} onClick={() => setTeamIds(ids => on ? ids.filter(i => i !== t.id) : [...ids, t.id])}
                            className="px-2 py-1 rounded-lg text-[11px] font-medium cursor-pointer transition-colors"
                            style={on ? { background: 'rgba(99,102,241,0.18)', color: '#C7D2FE', border: '1px solid rgba(99,102,241,0.45)' }
                              : { color: '#7E91A8', border: '1px solid #1E2C45' }}>
                            {t.name} <span className="text-[#3D5068]">· {orgName(t.org_id)}</span>
                          </button>
                        )
                      })}
                    </div>
                  </div>
                ) : (
                  <p className="text-xs text-[#3D5068]">You aren't on any team yet, so the agent will start with no project access. Add it to a team from the Orgs page.</p>
                )}
                <button type="submit" disabled={busy || !name.trim()} className={primaryBtnCls}>
                  {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}Create agent account
                </button>
                <Notice error={error} />
              </form>
              {agents == null ? <Loader2 size={16} className="text-[#6366F1] animate-spin" /> : agents.length === 0 ? (
                <p className="text-xs text-[#3D5068]">No agent accounts yet.</p>
              ) : (
                <ul className="space-y-3">
                  {agents.map(a => <AgentCard key={a.id} agent={a} onChanged={u => setAgents(list => (list ?? []).map(x => x.id === u.id ? u : x))} />)}
                </ul>
              )}
            </Section>
          </>
        )}
      </main>
    </div>
  )
}
