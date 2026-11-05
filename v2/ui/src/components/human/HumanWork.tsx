/**
 * HumanWork: a human ticket's panel in the TaskDrawer, the first thing its
 * owner reads. It leads with the brief (what to do, and what done means),
 * says where the ticket sits and what finishing it releases, then offers the
 * same Start and Mark done… as My work (HumanActions).
 *
 * Everything here comes from the ticket and the project's plan as the API
 * already returns them; dependents are worked out client-side from each
 * ticket's dependency_ids. A judgment ticket with a decision doc shows the
 * DecisionCard (question, options, the doc with its provenance, Decided
 * means, Edit doc, Record decision) in place of the brief's checklist and
 * Mark done…; it reloads when the doc changes (decision.updated, and after
 * the card's own edit or record). Its header says "blocks N · waiting Xd".
 *
 * A ticket the server says has a doc (has_decision_doc) never falls back to
 * Mark done, which the server refuses for it: while the doc loads it says
 * so, and a failed load shows the error with Retry. Only a confirmed "no doc"
 * (404) on a judgment ticket shows the J3 brief and Mark done….
 */
import { useCallback, useEffect, useState } from 'react'
import { AlertTriangle, ArrowRight, Pencil, RotateCcw, Square, SquareCheck, UserRound } from 'lucide-react'
import { Link } from 'react-router-dom'
import { api } from '../../api/client'
import { useSSE } from '../../hooks/useSSE'
import { checklistItems } from '../../lib/brief'
import { isHuman, reasonHint, reasonLabel, reasonVerb } from '../../lib/human'
import { lookupPerson, type Person } from '../../lib/people'
import { openDependents, waitingSince } from '../../lib/decisions'
import type { ProjectPlan } from '../../hooks/useProjectPlan'
import type { DecisionDoc, Task } from '../../types'
import { Markdown } from '../ui/Markdown'
import { DecisionCard } from './DecisionCard'
import { HumanActions } from './HumanActions'

/** How many dependents are listed by name before "+N more". */
const UNBLOCKS_SHOWN = 3

export interface HumanWorkProps {
  task: Task
  meId?: number
  people: Map<number, Person>
  plan: ProjectPlan
  /** may edit the ticket's text: the empty brief offers Edit */
  canPlan: boolean
  onEdit: () => void
  /** where a "#N" reference links: the current view with that ticket focused */
  ticketHref: (id: number) => string
  onChanged: (t: Task) => void
}

/** Who does a dependent ticket, as a short tag: its owner for a person's, its agent otherwise. */
function doneBy(t: Task, people: Map<number, Person>): string {
  if (isHuman(t)) return lookupPerson(people, t.owner_user_id)?.name ?? 'a person'
  return t.agent_id || t.assigned_to || 'an agent'
}

export function HumanWork({ task, meId, people, plan, canPlan, onEdit, ticketHref, onChanged }: HumanWorkProps) {
  const owner = lookupPerson(people, task.owner_user_id)
  const creator = lookupPerson(people, task.created_by)
  // Its owner does it; an unassigned one is claimed by whoever starts it. Local dev has no ownership.
  const mine = meId === undefined || task.owner_user_id == null || task.owner_user_id === meId
  const who = owner ? owner.name : 'its owner'
  const rules = `${reasonHint(task.human_reason)}. Agents never pick it up, and they can't mark it done: only ${who} can, here in AMP. The work that depends on it starts then.`

  const epic = plan.epics.find(e => e.id === task.epic_id)
  const story = plan.stories.find(s => s.id === task.story_id)
  const unblocks = plan.tasks.filter(t => t.id !== task.id && t.state !== 'completed' && (t.dependency_ids ?? []).includes(task.id))

  const description = task.description?.trim() ?? ''
  const criteria = task.acceptance_criteria?.trim() ?? ''
  const checklist = checklistItems(criteria)

  // A judgment ticket's decision doc, when it has one: 'none' once the
  // server confirms there is none (404), 'error' when it couldn't be read.
  const hasDoc = !!task.has_decision_doc
  const wantsDoc = hasDoc || task.human_reason === 'judgment'
  const [loaded, setLoaded] = useState<DecisionDoc | 'none' | 'error' | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const loadDoc = useCallback(() => {
    if (!wantsDoc) return
    // The doc with its versions, for provenance; 404 when it has none.
    api.getTaskDecision(task.id)
      .then(d => { setLoaded(d?.artifact_id ? d : 'none'); setLoadError(null) })
      .catch((e: unknown) => {
        if ((e as { status?: number } | null)?.status === 404) { setLoaded('none'); setLoadError(null); return }
        setLoaded(cur => (typeof cur === 'object' && cur ? cur : 'error'))
        setLoadError(e instanceof Error ? e.message : 'Could not load the decision doc')
      })
  }, [wantsDoc, task.id])
  useEffect(() => { loadDoc() }, [loadDoc])
  const doc = typeof loaded === 'object' ? loaded : null
  // The card's own edit returns the doc with its versions; a record doesn't.
  const docChanged = useCallback((d: DecisionDoc) => { if (d.versions) setLoaded(d); else loadDoc() }, [loadDoc])
  useSSE(task.project_id, e => {
    if (e.type === 'decision.updated' && (e.payload as { task_id?: number } | undefined)?.task_id === task.id) loadDoc()
  })

  return (
    <div className="px-6 py-4 border-b border-line grid gap-3" data-testid="human-work"
      style={{ background: 'var(--surface-2)', boxShadow: 'inset 3px 0 0 var(--ink)' }}>
      <div className="grid gap-1">
        <h3 className="m-0 amp-label flex items-center gap-1.5 text-ink" title={rules}>
          <UserRound size={12} aria-hidden="true" />
          {reasonVerb(task.human_reason)} · {owner ? `${owner.name} does this` : 'a person does this'}
        </h3>
        <span className="text-[12px] text-ink-3" data-testid="human-context">
          {(epic || story) && <>{epic?.name ?? 'Epic'} › {story?.name ?? `Story #${task.story_id}`} · </>}
          {reasonLabel(task.human_reason)} · planned by {creator ? <span className="text-ink-2">{creator.name}{task.created_by === meId ? ' (you)' : ''}</span> : 'someone unknown'}
        </span>
      </div>

      {/* The brief: capped so a long one scrolls here and the actions stay in reach. */}
      {doc ? (description && (
        <div className="max-h-[24vh] overflow-y-auto pr-1" data-testid="human-brief">
          <div className="amp-label mb-1.5">What you need to do</div>
          <Markdown size="compact">{description}</Markdown>
        </div>
      )) : description || criteria ? (
        <div className="grid gap-3 max-h-[38vh] overflow-y-auto pr-1" data-testid="human-brief">
          {description && (
            <div>
              <div className="amp-label mb-1.5">What you need to do</div>
              <Markdown size="compact">{description}</Markdown>
            </div>
          )}
          {criteria && (
            <div>
              <div className="amp-label mb-1.5">Done when</div>
              {checklist ? (
                <ul className="m-0 p-0 list-none grid gap-1.5" aria-label="Done when">
                  {checklist.map((item, i) => (
                    <li key={i} className="flex items-start gap-2" data-testid="checklist-item" data-done={item.done || undefined}>
                      {item.done
                        ? <SquareCheck size={14} className="mt-[5px] flex-shrink-0 text-success-ink" aria-label="done" />
                        : <Square size={14} className="mt-[5px] flex-shrink-0 text-ink-3" aria-label="to do" />}
                      <div className="min-w-0 flex-1"><Markdown size="compact">{item.text}</Markdown></div>
                    </li>
                  ))}
                </ul>
              ) : <Markdown size="compact">{criteria}</Markdown>}
            </div>
          )}
        </div>
      ) : (
        <div className="flex items-start gap-2 text-[13px] leading-relaxed text-warning-ink" role="note" data-testid="human-brief-empty">
          <AlertTriangle size={14} className="mt-[3px] flex-shrink-0" aria-hidden="true" />
          <span className="flex-1">
            The planner left no instructions. Ask {creator ? creator.name : 'whoever planned it'} on the ticket{canPlan ? ', or edit it to add them' : ''}.
          </span>
          {canPlan && (
            <button type="button" className="amp-btn amp-btn-sm" onClick={onEdit}>
              <Pencil size={11} aria-hidden="true" />Edit</button>
          )}
        </div>
      )}

      {/* What finishing it releases: open tickets that list this one as a dependency. */}
      {plan.loaded && (
        <div className="text-[12.5px] text-ink-2 flex items-start gap-2 flex-wrap" data-testid="human-unblocks">
          <span className="amp-label pt-px">{unblocks.length > 0 ? `Unblocks ${unblocks.length}` : 'Unblocks'}</span>
          {unblocks.length === 0 ? <span className="text-ink-3">Nothing waits on this</span> : (
            <ul className="m-0 p-0 list-none grid gap-0.5 min-w-0 flex-1">
              {unblocks.slice(0, UNBLOCKS_SHOWN).map(t => (
                <li key={t.id} className="min-w-0 truncate">
                  <ArrowRight size={11} className="inline mr-1 text-ink-3" aria-hidden="true" />
                  <Link to={ticketHref(t.id)} className="amp-num text-ink underline underline-offset-2 hover:text-accent">#{t.id}</Link>
                  {' '}<span className="text-ink">{t.name}</span>
                  <span className="text-ink-3"> · {doneBy(t, people)}</span>
                </li>
              ))}
              {unblocks.length > UNBLOCKS_SHOWN && <li className="text-ink-3">+{unblocks.length - UNBLOCKS_SHOWN} more</li>}
            </ul>
          )}
        </div>
      )}

      {doc ? (
        <DecisionCard task={task} doc={doc} canAct={mine} ownerName={owner?.name} onChanged={onChanged} onDocChanged={docChanged}
          blocks={plan.loaded ? openDependents(task.id, plan.tasks).length : undefined} waitingSince={waitingSince(task)} />
      ) : loaded === 'error' ? (
        <div className="flex items-start gap-2 text-[13px] text-danger-ink" role="alert" data-testid="decision-load-error">
          <AlertTriangle size={14} className="mt-[3px] flex-shrink-0" aria-hidden="true" />
          <span className="flex-1">Couldn't load the decision doc{loadError ? `: ${loadError}` : ''}.</span>
          <button type="button" className="amp-btn amp-btn-sm" onClick={loadDoc}>
            <RotateCcw size={11} aria-hidden="true" />Retry</button>
        </div>
      ) : hasDoc && loaded !== 'none' ? (
        <span className="text-[12.5px] text-ink-2" data-testid="decision-loading">Loading the decision doc…</span>
      ) : <HumanActions task={task} kind="do" canAct={mine} ownerName={owner?.name} onChanged={t => { if (t) onChanged(t) }} />}
    </div>
  )
}
