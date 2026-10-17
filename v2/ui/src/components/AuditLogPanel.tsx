import { useCallback, useEffect, useState } from 'react'
import { KeyRound, Loader2 } from 'lucide-react'
import { api } from '../api/client'
import type { AuditEntry } from '../types'
import { Pill, When } from './admin'
import { errMsg, smallSelectCls } from '../lib/adminStyles'

// The audit log for one scope: an org (its own changes and its projects'),
// a project, or everything (global admins). Newest first, paged backwards.

const FILTERS: { value: string; label: string }[] = [
  { value: '', label: 'Everything' },
  { value: 'org.', label: 'Org and members' },
  { value: 'team.', label: 'Teams' },
  { value: 'invite.', label: 'Invites' },
  { value: 'project.', label: 'Projects and access' },
  { value: 'webhook.', label: 'Webhooks' },
  { value: 'user.', label: 'Accounts' },
  { value: 'agent.', label: 'Agent accounts' },
  { value: 'token.', label: 'API tokens' },
  { value: 'task.delete', label: 'Deleted tickets' },
  { value: 'story.delete', label: 'Deleted stories' },
  { value: 'epic.delete', label: 'Deleted epics' },
]

const PAGE = 50

function actionTone(action: string) {
  if (action.endsWith('.delete') || action.endsWith('.remove') || action.endsWith('.revoke') || action === 'user.deactivate' || action === 'project.reset') return 'red' as const
  if (action.startsWith('token.') || action.startsWith('user.') || action.startsWith('agent.')) return 'amber' as const
  return 'indigo' as const
}

export function AuditLogPanel({ orgId, projectId, actorId }: { orgId?: number; projectId?: number; actorId?: number }) {
  const [action, setAction] = useState('')
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [more, setMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async (before?: number) => {
    setLoading(true); setError(null)
    try {
      const page = await api.audit({ orgId, projectId, actorId, action, before, limit: PAGE })
      setEntries(prev => before ? [...prev, ...page] : page)
      setMore(page.length === PAGE)
    } catch (e) {
      setError(errMsg(e, 'Failed to load the audit log'))
    } finally {
      setLoading(false)
    }
  }, [orgId, projectId, actorId, action])

  useEffect(() => { load() }, [load])

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <select value={action} onChange={e => setAction(e.target.value)} className={smallSelectCls} aria-label="Filter the audit log">
          {FILTERS.map(f => <option key={f.value} value={f.value}>{f.label}</option>)}
        </select>
        {loading && <Loader2 size={13} className="text-[#6366F1] animate-spin" />}
      </div>
      {error && <p className="text-xs text-[#F87171]">{error}</p>}
      <div className="rounded-xl border border-[#1E2C45] overflow-hidden">
        {entries.length === 0 && !loading ? (
          <p className="px-4 py-6 text-center text-sm text-[#3D5068]">Nothing recorded yet</p>
        ) : (
          <ul>
            {entries.map((e, i) => (
              <li key={e.id} className={`px-4 py-2.5 flex items-start gap-3 ${i ? 'border-t border-[#1E2C45]' : ''}`}>
                <span className="text-[11px] text-[#3D5068] w-16 flex-shrink-0 pt-0.5"><When at={e.at} /></span>
                <div className="flex-1 min-w-0">
                  <p className="text-sm text-[#DDE6F0] break-words">
                    <span className="text-[#818CF8]">{e.actor_email || 'AMP'}</span> {e.summary}
                  </p>
                  <div className="flex flex-wrap items-center gap-1.5 mt-1">
                    <Pill tone={actionTone(e.action)}>{e.action}</Pill>
                    {e.via_token && <Pill tone="gray" title="Made with an API token"><KeyRound size={9} />API token</Pill>}
                    {e.project_id != null && !projectId && <span className="text-[10px] text-[#3D5068]">project {e.project_id}</span>}
                  </div>
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
      {more && (
        <button onClick={() => load(entries[entries.length - 1]?.id)} disabled={loading}
          className="text-xs font-semibold text-[#818CF8] hover:underline disabled:opacity-50 cursor-pointer">
          Older entries
        </button>
      )}
    </div>
  )
}
