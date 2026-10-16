import { useState } from 'react'
import { Loader2, X } from 'lucide-react'
import { OwnerAvatar } from './OwnerAvatar'
import { lookupPerson, personName, type Person } from '../lib/people'
import type { ProjectMember } from '../types'

interface Props {
  ownerIds: number[]
  people: Map<number, Person>
  members: ProjectMember[]
  meId?: number
  /** Owner changes apply immediately (not on the modal's Save). */
  onAdd: (userId: number) => Promise<void>
  onRemove: (userId: number) => Promise<void>
}

// Owners list for an epic or story: chips with remove buttons plus an
// "Add owner…" select of project members. API errors (e.g. 403 when the
// caller is neither an owner nor a project owner) are shown inline.
export function OwnersEditor({ ownerIds, people, members, meId, onAdd, onRemove }: Props) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError(null)
    try { await fn() }
    catch (e) { setError(e instanceof Error ? e.message : 'Update failed') }
    finally { setBusy(false) }
  }

  const candidates = members.filter(m => !ownerIds.includes(m.user_id))

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-1.5">
        {ownerIds.length === 0 && <span className="text-xs text-[#3D5068] italic">No owners</span>}
        {ownerIds.map(id => {
          const p = lookupPerson(people, id)!
          return (
            <span
              key={id}
              className="flex items-center gap-1.5 pl-1 pr-1.5 py-0.5 rounded-full text-xs text-[#DDE6F0]"
              style={{ background: '#111E30', border: '1px solid #1E2C45' }}
              title={p.email}
            >
              <OwnerAvatar person={p} size={18} />
              {p.name}{id === meId ? ' (you)' : ''}
              <button
                type="button"
                onClick={() => run(() => onRemove(id))}
                disabled={busy}
                className="p-0.5 rounded-full text-[#3D5068] hover:text-[#F87171] hover:bg-[#EF4444]/10 transition-colors cursor-pointer disabled:opacity-50"
                title={`Remove ${p.name}`}
              >
                <X size={10} />
              </button>
            </span>
          )
        })}
        {busy && <Loader2 size={12} className="text-[#7E91A8] animate-spin self-center" />}
      </div>
      {candidates.length > 0 && (
        <select
          value=""
          disabled={busy}
          onChange={e => { const id = Number(e.target.value); if (id) run(() => onAdd(id)) }}
          className="w-full bg-[#08101F] border border-[#1E2C45] rounded-lg px-3 py-2 text-sm text-[#DDE6F0] focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all cursor-pointer disabled:opacity-50"
        >
          <option value="">Add owner…</option>
          {candidates.map(m => (
            <option key={m.user_id} value={m.user_id}>{personName(m)}{m.user_id === meId ? ' (me)' : ''}</option>
          ))}
        </select>
      )}
      {error && <p className="text-xs text-[#F87171]">{error}</p>}
    </div>
  )
}
