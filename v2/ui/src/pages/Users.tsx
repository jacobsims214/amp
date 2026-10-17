import { useState, useEffect, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { ArrowLeft, Loader2, ShieldCheck, UserPlus, Trash2, KeyRound, ShieldOff, Power } from 'lucide-react'
import { AccountBadges } from '../components/admin'
import { AuditLogPanel } from '../components/AuditLogPanel'
import { api, authAdminApi } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import type { AmpUser, AuthAdminUser } from '../types'

// Admin-only page: manage Dex login credentials (amp-authadmin) and role
// assignment (amp-api). Two separate systems on purpose — Dex remains the
// sole source of truth for passwords; amp-api only tracks role assignment
// for JIT-provisioned identities.
export function Users() {
  const { me, loading: authLoading, isAdmin } = useAuth()

  const [ampUsers, setAmpUsers] = useState<AmpUser[]>([])
  const [credentials, setCredentials] = useState<AuthAdminUser[]>([])
  const [roleError, setRoleError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const [newEmail, setNewEmail] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [newName, setNewName] = useState('')
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)

  const [busyEmail, setBusyEmail] = useState<string | null>(null)
  const [resetTarget, setResetTarget] = useState<string | null>(null)
  const [resetPassword, setResetPassword] = useState('')

  const reload = useCallback(() => {
    setLoading(true)
    // Loaded separately: one failing (e.g. amp-authadmin down) shouldn't hide the other.
    Promise.allSettled([api.listUsers(), authAdminApi.listCredentials()])
      .then(([users, creds]) => {
        if (users.status === 'fulfilled') setAmpUsers(users.value)
        if (creds.status === 'fulfilled') setCredentials(creds.value)
        const failed = [users, creds].find(r => r.status === 'rejected') as PromiseRejectedResult | undefined
        setError(failed ? String(failed.reason?.message ?? failed.reason) : null)
      })
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    if (isAdmin) reload()
  }, [isAdmin, reload])

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    setCreating(true)
    setCreateError(null)
    try {
      await authAdminApi.createCredential(newEmail.trim().toLowerCase(), newPassword, newName.trim() || undefined)
      setNewEmail('')
      setNewPassword('')
      setNewName('')
      reload()
    } catch (e) {
      setCreateError(e instanceof Error ? e.message : 'Create failed')
    } finally {
      setCreating(false)
    }
  }

  async function handleDelete(email: string) {
    if (!confirm(`Remove login credential for ${email}? They will no longer be able to sign in.`)) return
    setBusyEmail(email)
    try {
      await authAdminApi.deleteCredential(email)
      reload()
    } catch (e) {
      alert(e instanceof Error ? e.message : 'Delete failed')
    } finally {
      setBusyEmail(null)
    }
  }

  async function handleResetSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!resetTarget) return
    setBusyEmail(resetTarget)
    try {
      await authAdminApi.resetPassword(resetTarget, resetPassword)
      setResetTarget(null)
      setResetPassword('')
    } catch (e) {
      alert(e instanceof Error ? e.message : 'Reset failed')
    } finally {
      setBusyEmail(null)
    }
  }

  async function toggleActive(u: AmpUser) {
    const deactivate = !u.disabled_at
    if (deactivate && !confirm(`Deactivate ${u.email}? Every request from this account (sign-ins and API tokens) will be refused, and it can't be given work. Its tickets and history stay.`)) return
    setBusyEmail(u.email)
    setRoleError(null)
    try {
      await (deactivate ? api.deactivateUser(u.id) : api.reactivateUser(u.id))
      reload()
    } catch (e) {
      setRoleError(e instanceof Error ? e.message : 'Update failed')
    } finally {
      setBusyEmail(null)
    }
  }

  async function toggleAdmin(u: AmpUser) {
    const grant = !u.roles.includes('admin')
    setBusyEmail(u.email)
    setRoleError(null)
    try {
      await api.setUserRole(u.id, 'admin', grant)
      reload()
    } catch (e) {
      setRoleError(e instanceof Error ? e.message : 'Update failed')
    } finally {
      setBusyEmail(null)
    }
  }

  if (authLoading) {
    return (
      <div className="min-h-full flex items-center justify-center" style={{ background: 'var(--bg)' }}>
        <Loader2 size={22} className="text-primary animate-spin" />
      </div>
    )
  }

  if (!isAdmin) {
    return (
      <div className="min-h-full flex flex-col items-center justify-center gap-3" style={{ background: 'var(--bg)' }}>
        <ShieldOff size={28} className="text-ink-3" />
        <p className="text-sm text-ink-2">
          {me ? 'Your account does not have admin access.' : 'Sign in required.'}
        </p>
        <Link to="/" className="text-xs text-link hover:underline">← Back to projects</Link>
      </div>
    )
  }

  return (
    <div className="min-h-full" style={{ background: 'var(--bg)' }}>
      <header style={{ background: 'var(--surface)', borderBottom: '1px solid var(--line)' }}>
        <div className="max-w-4xl mx-auto px-6 py-4 flex items-center gap-3">
          <Link to="/" className="text-ink-3 hover:text-ink transition-colors">
            <ArrowLeft size={16} />
          </Link>
          <ShieldCheck size={16} className="text-link" />
          <h1 className="text-base font-bold text-ink">Users &amp; Access</h1>
        </div>
      </header>

      <main className="max-w-4xl mx-auto px-6 py-10 space-y-10">
        {error && (
          <div className="rounded-xl border border-danger/20 bg-danger/8 px-4 py-3 text-sm text-danger-ink">
            {error}
          </div>
        )}

        {/* Create new login */}
        <section>
          <h2 className="text-sm font-bold text-ink mb-3">Add a user</h2>
          <form onSubmit={handleCreate} className="flex flex-wrap items-start gap-2">
            <input
              type="email" required placeholder="email@example.com" value={newEmail}
              onChange={e => setNewEmail(e.target.value)}
              className="px-3 py-2 rounded-lg text-sm bg-surface border border-line text-ink placeholder-ink-3 outline-none focus:border-primary"
            />
            <input
              type="text" placeholder="Display name (optional)" value={newName}
              onChange={e => setNewName(e.target.value)}
              className="px-3 py-2 rounded-lg text-sm bg-surface border border-line text-ink placeholder-ink-3 outline-none focus:border-primary"
            />
            <input
              type="password" required placeholder="Temporary password (min 8 chars)" value={newPassword}
              onChange={e => setNewPassword(e.target.value)}
              minLength={8}
              className="px-3 py-2 rounded-lg text-sm bg-surface border border-line text-ink placeholder-ink-3 outline-none focus:border-primary"
            />
            <button
              type="submit" disabled={creating}
              className="flex items-center gap-1.5 px-3 py-2 rounded-lg text-sm font-semibold text-white disabled:opacity-50"
              style={{ background: 'var(--primary)' }}
            >
              {creating ? <Loader2 size={14} className="animate-spin" /> : <UserPlus size={14} />}
              Create
            </button>
          </form>
          {createError && <p className="text-xs text-danger-ink mt-2">{createError}</p>}
          <p className="text-xs text-ink-3 mt-2">
            Credentials are stored in Dex, never in amp-api. New users get the <code>member</code> role by default —
            make them a global admin below once they've signed in at least once (most people don't need it — add them to an org and team instead).
          </p>
        </section>

        {/* Credentials table */}
        <section>
          <h2 className="text-sm font-bold text-ink mb-3">Login credentials (Dex)</h2>
          {loading ? (
            <Loader2 size={18} className="text-primary animate-spin" />
          ) : (
            <div className="rounded-xl border border-line overflow-hidden">
              <table className="w-full text-sm">
                <thead>
                  <tr className="text-left text-ink-3 text-xs" style={{ background: 'var(--surface)' }}>
                    <th className="px-4 py-2 font-semibold">Email</th>
                    <th className="px-4 py-2 font-semibold">Username</th>
                    <th className="px-4 py-2 font-semibold w-40">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {credentials.map(c => (
                    <tr key={c.email} className="border-t border-line">
                      <td className="px-4 py-2 text-ink">{c.email}</td>
                      <td className="px-4 py-2 text-ink-2">{c.username}</td>
                      <td className="px-4 py-2">
                        <div className="flex items-center gap-2">
                          <button
                            onClick={() => { setResetTarget(c.email); setResetPassword('') }}
                            title="Reset password"
                            className="text-ink-2 hover:text-link"
                          >
                            <KeyRound size={14} />
                          </button>
                          <button
                            onClick={() => handleDelete(c.email)}
                            disabled={busyEmail === c.email}
                            title="Remove login"
                            className="text-ink-2 hover:text-danger-ink disabled:opacity-50"
                          >
                            <Trash2 size={14} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                  {credentials.length === 0 && (
                    <tr><td colSpan={3} className="px-4 py-6 text-center text-ink-3 text-sm">No credentials yet</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          )}

          {resetTarget && (
            <form onSubmit={handleResetSubmit} className="mt-3 flex items-center gap-2">
              <span className="text-xs text-ink-2">Reset password for {resetTarget}:</span>
              <input
                type="password" required minLength={8} placeholder="New password" value={resetPassword}
                onChange={e => setResetPassword(e.target.value)}
                className="px-3 py-1.5 rounded-lg text-sm bg-surface border border-line text-ink outline-none focus:border-primary"
              />
              <button type="submit" className="px-3 py-1.5 rounded-lg text-xs font-semibold text-white" style={{ background: 'var(--primary)' }}>
                Save
              </button>
              <button type="button" onClick={() => setResetTarget(null)} className="text-xs text-ink-3 hover:text-ink-2">
                Cancel
              </button>
            </form>
          )}
        </section>

        {/* Roles table */}
        <section>
          <h2 className="text-sm font-bold text-ink mb-3">People, agents &amp; global admins</h2>
          <p className="text-xs text-ink-3 mb-3">
            People appear here the first time they sign in. A <strong>global admin</strong> manages the whole platform:
            logins on this page, creating orgs, and every org, team and project — in the UI and through their agents&apos;
            MCP admin tools. It takes effect on their next request; no re-login needed. Org and team roles are set per org
            on the <a href="/admin/orgs" className="text-link hover:underline">Orgs</a> page.
            Deactivating an account refuses everything it does (including its API tokens) without deleting its work;
            agent accounts are created on <a href="/access" className="text-link hover:underline">Agents &amp; tokens</a>.
          </p>
          {roleError && (
            <p className="text-xs text-danger-ink mb-3">{roleError}</p>
          )}
          <div className="rounded-xl border border-line overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-ink-3 text-xs" style={{ background: 'var(--surface)' }}>
                  <th className="px-4 py-2 font-semibold">Email</th>
                  <th className="px-4 py-2 font-semibold">Name</th>
                  <th className="px-4 py-2 font-semibold">Access</th>
                  <th className="px-4 py-2 font-semibold w-44">Actions</th>
                </tr>
              </thead>
              <tbody>
                {ampUsers.map(u => (
                  <tr key={u.id} className="border-t border-line">
                    <td className="px-4 py-2 text-ink">
                      <span className="flex items-center gap-1.5 flex-wrap">{u.email}<AccountBadges kind={u.kind} disabled={!!u.disabled_at} /></span>
                    </td>
                    <td className="px-4 py-2 text-ink-2">{u.display_name || '—'}</td>
                    <td className="px-4 py-2">
                      {u.roles.includes('admin') ? (
                        <span
                          className="text-[10px] font-semibold px-1.5 py-0.5 rounded-md"
                          style={{ background: 'color-mix(in srgb, var(--link) 12%, transparent)', color: 'var(--link)', border: '1px solid color-mix(in srgb, var(--link) 22%, transparent)' }}
                        >
                          global admin
                        </span>
                      ) : (
                        <span className="text-[10px] text-ink-3">member</span>
                      )}
                    </td>
                    <td className="px-4 py-2">
                      <div className="flex items-center gap-3">
                        {u.kind !== 'agent' && (
                          <button
                            onClick={() => toggleAdmin(u)}
                            disabled={busyEmail === u.email}
                            className="text-xs font-semibold text-link hover:underline disabled:opacity-50 whitespace-nowrap"
                          >
                            {u.roles.includes('admin') ? 'Revoke global admin' : 'Make global admin'}
                          </button>
                        )}
                        {u.id !== me?.user_id && (
                          <button
                            onClick={() => toggleActive(u)}
                            disabled={busyEmail === u.email}
                            title={u.disabled_at ? 'Reactivate' : 'Deactivate'}
                            className={`flex items-center gap-1 text-xs font-semibold disabled:opacity-50 whitespace-nowrap ${u.disabled_at ? 'text-success-ink' : 'text-ink-2 hover:text-danger-ink'}`}
                          >
                            <Power size={12} />{u.disabled_at ? 'Reactivate' : 'Deactivate'}
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                ))}
                {ampUsers.length === 0 && (
                  <tr><td colSpan={4} className="px-4 py-6 text-center text-ink-3 text-sm">No one has signed in yet</td></tr>
                )}
              </tbody>
            </table>
          </div>
        </section>

        <section>
          <h2 className="text-sm font-bold text-ink mb-1">Audit log</h2>
          <p className="text-xs text-ink-3 mb-3">
            Every change to access, accounts, API tokens, settings, and webhooks, and every deletion, across all orgs.
            Org admins see their org's on the Orgs page; project owners see their project's in its settings.
          </p>
          <AuditLogPanel />
        </section>
      </main>
    </div>
  )
}
