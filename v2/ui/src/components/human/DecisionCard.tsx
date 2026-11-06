/**
 * DecisionCard: a judgment ticket's decision doc in its drawer
 * (docs/specs/human-decision-docs.md §5). It shows the question and the
 * options of the doc's current version (OptionPicker), the doc's prose
 * (context, trade-offs, why the recommendation), and "Decided means": the
 * ticket's acceptance criteria, read-only.
 *
 * Provenance: the agent's lines and options carry a left rule in the accent
 * colour and an "Agent" tag on hover; the owner's have no mark (line blame
 * across the doc's versions, lib/provenance.ts).
 *
 * The header says "blocks N · waiting Xd" (J1) while the ticket is open,
 * and its decide-by date (DecideBy, #146): relative and absolute, red once
 * overdue; its person sets or changes it there.
 *
 * Edit doc opens the markdown (with a preview) and the option editor; Save
 * writes both as one new version. A 409 keeps the draft and says why. It is
 * offered while the ticket still waits on its dependencies too, so the owner
 * can prepare the decision; recording waits for them. A new current version
 * (this card's save or another tab's) clears the picked option, whose index
 * may now mean another option.
 *
 * Record decision freezes the current version with the chosen option and the
 * decided text, completes the ticket and posts the decision. After that the
 * card is read-only: "Decided by <name> · vN · <date>". A record that stopped
 * part way can be retried after a minute, with what was stored.
 */
import { useEffect, useMemo, useState } from 'react'
import { Check, Eye, Loader2, Pencil, Play, Plus, RotateCcw, Sparkles, Trash2, X } from 'lucide-react'
import { api } from '../../api/client'
import { checklistItems } from '../../lib/brief'
import { CLAIM_STALE_MS, waitLine, type OptionChoice } from '../../lib/decisions'
import { blame, blameRuns, type Author } from '../../lib/provenance'
import type { DecisionAsk, DecisionDoc, Task } from '../../types'
import { Markdown } from '../ui/Markdown'
import { OptionPicker } from './OptionPicker'
import { DecideBy } from './DecideBy'

export interface DecisionCardProps {
  task: Task
  doc: DecisionDoc
  /** the person may decide (its owner, or anyone on an unassigned ticket) */
  canAct: boolean
  ownerName?: string
  onChanged?: (t: Task) => void
  /** the doc after an edit or a record */
  onDocChanged?: (d: DecisionDoc) => void
  /** open tickets downstream of it, transitively (undefined: not known yet) */
  blocks?: number
  /** when it started waiting on its person */
  waitingSince?: string
  /** now, for the claim's age (tests) */
  now?: () => number
}

interface Draft { content: string; ask: DecisionAsk }

const MIN_OPTIONS = 2
const MAX_OPTIONS = 4

function when(iso: string) {
  return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
}

/** The agent's mark: an accent rule, and "Agent" on hover. */
function AgentMark({ author, children, testId }: { author: Author; children: React.ReactNode; testId?: string }) {
  if (author !== 'agent') return <div data-testid={testId} data-author="person">{children}</div>
  return (
    <div className="group relative pl-3" style={{ boxShadow: 'inset 2px 0 0 var(--accent)' }} title="Agent" data-testid={testId} data-author="agent">
      {children}
      <span className="amp-label absolute right-0 top-0 hidden group-hover:inline px-1 bg-surface text-ink-3">Agent</span>
    </div>
  )
}

function validDraft(d: Draft): string | null {
  if (!d.ask.question.trim()) return 'The question is empty'
  for (const [i, o] of d.ask.options.entries()) {
    if (!o.label.trim()) return `Option ${i + 1} needs a label`
    if (!o.consequence.trim()) return `Option ${i + 1} needs a consequence: its trade-offs or cost`
  }
  if (!d.content.trim()) return "The doc's text is empty"
  return null
}

export function DecisionCard({ task, doc, canAct, ownerName, onChanged, onDocChanged, blocks, waitingSince, now = Date.now }: DecisionCardProps) {
  const [choice, setChoice] = useState<OptionChoice>(null)
  // A new current version may reorder or drop options: the index picked on
  // the old one would silently point at another option.
  const [choiceVersion, setChoiceVersion] = useState(doc.current_version)
  if (choiceVersion !== doc.current_version) {
    setChoiceVersion(doc.current_version)
    setChoice(null)
  }
  const [text, setText] = useState('')
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [preview, setPreview] = useState(false)
  const [editError, setEditError] = useState<string | null>(null)
  const ask = doc.ask
  const criteria = task.acceptance_criteria?.trim() ?? ''
  const checklist = checklistItems(criteria)
  const blocked = task.state === 'blocked'
  const open = task.state !== 'completed'
  const decided = doc.decided_at != null
  const claimed = doc.decided_version != null && (doc.recording_started_at != null || decided)
  const claimStale = claimed && !decided && doc.recording_started_at != null && now() - Date.parse(doc.recording_started_at) > CLAIM_STALE_MS
  const actionable = canAct && open && !blocked && !claimed
  // The server takes an edit until the record is claimed, waiting or not.
  const editable = canAct && open && !claimed
  // A pending claim turns into "Recording didn't finish. Retry" when it is a
  // minute old: re-render then, since nothing else may.
  const [, setTick] = useState(0)
  useEffect(() => {
    if (!claimed || decided || claimStale || doc.recording_started_at == null) return
    const wait = Date.parse(doc.recording_started_at) + CLAIM_STALE_MS - now()
    const id = setTimeout(() => setTick(n => n + 1), Math.max(0, wait) + 50)
    return () => clearTimeout(id)
  }, [claimed, decided, claimStale, doc.recording_started_at, now])
  const textId = `decision-text-${task.id}`

  // Provenance, when the versions came with the doc and end at its current one.
  const provenance = useMemo(() => {
    const vs = doc.versions ?? []
    if (!vs.length || vs[vs.length - 1].version !== doc.current_version) return null
    return blame(vs)
  }, [doc.versions, doc.current_version])

  const finish = async (body: { version: number; chosen_option: number | null; decision_text: string }) => {
    setBusy('record'); setError(null)
    try {
      // Not onDocChanged?.(await …): an optional call skips its arguments.
      const recorded = await api.recordDecision(task.id, body)
      onDocChanged?.(recorded)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'That did not work')
      // A record can stop after its claim: reload the doc so the card shows
      // the claim (and, after a minute, Retry) rather than the open form.
      api.getTaskDecision(task.id).then(d => onDocChanged?.(d)).catch(() => {})
      setBusy(null)
      return
    }
    try {
      const updated = await api.getTask(task.id)
      onChanged?.(updated)
    } catch {
      // Recorded; the drawer catches up on the ticket's own event.
    } finally {
      setBusy(null)
    }
  }
  const start = async () => {
    setBusy('start'); setError(null)
    try { const t = await api.startHumanTask(task.id); onChanged?.(t) }
    catch (e) { setError(e instanceof Error ? e.message : 'That did not work') }
    finally { setBusy(null) }
  }
  const acceptRecommendation = () => {
    if (!ask) return
    setChoice(ask.recommended)
    setText(ask.options[ask.recommended]?.label ?? '')
    setError(null)
  }
  const record = () => {
    if (ask && choice === null) { setError('Pick an option, or Something else'); return }
    if (!text.trim()) { setError('Write what you decided'); return }
    finish({ version: doc.current_version, chosen_option: typeof choice === 'number' ? choice : null, decision_text: text.trim() })
  }
  const retry = () => {
    if (doc.decided_version == null) return
    finish({ version: doc.decided_version, chosen_option: doc.chosen_option, decision_text: doc.decision_text ?? '' })
  }

  // Editing: the markdown and the options, saved as one version.
  const beginEdit = () => {
    const base = ask ?? { question: '', options: [{ label: '', consequence: '' }, { label: '', consequence: '' }], recommended: 0 }
    setDraft({ content: doc.text, ask: { question: base.question, recommended: base.recommended, options: base.options.map(o => ({ ...o })) } })
    setPreview(false); setEditError(null)
  }
  const setAsk = (f: (a: DecisionAsk) => DecisionAsk) => setDraft(d => d && { ...d, ask: f(d.ask) })
  const setOption = (i: number, field: 'label' | 'consequence', v: string) =>
    setAsk(a => ({ ...a, options: a.options.map((o, j) => j === i ? { ...o, [field]: v } : o) }))
  const removeOption = (i: number) => setAsk(a => {
    const options = a.options.filter((_, j) => j !== i)
    const recommended = a.recommended === i ? 0 : a.recommended > i ? a.recommended - 1 : a.recommended
    return { ...a, options, recommended }
  })
  const save = async () => {
    if (!draft) return
    const invalid = validDraft(draft)
    if (invalid) { setEditError(invalid); return }
    setBusy('save'); setEditError(null)
    try {
      const saved = await api.saveDecisionDoc(task.id, { base_version: doc.current_version, content: draft.content, ask: draft.ask })
      setDraft(null)
      // The options changed under any choice made before the edit (and the
      // new current_version clears it too).
      setChoice(null)
      onDocChanged?.(saved)
    } catch (e) {
      // The draft stays; the server says why (a 409: the doc changed in
      // another tab, or it was recorded meanwhile).
      setEditError(e instanceof Error ? e.message : 'Could not save the doc')
    } finally {
      setBusy(null)
    }
  }

  const docText = doc.text.trim() ? doc.text : ''
  const runs = provenance && provenance.lines.map(l => l.text).join('\n') === doc.text.replace(/\r\n/g, '\n') ? blameRuns(provenance.lines) : null

  return (
    <div className="grid gap-3" data-testid="decision-card">
      {open && blocks !== undefined && (
        <span className="text-[12.5px] text-ink-2" data-testid="decision-wait">{waitLine(blocks, waitingSince, now())}</span>
      )}
      {open && !claimed && <DecideBy task={task} canEdit={canAct} onChanged={onChanged} now={now} />}
      <span className="text-[12px] text-ink-3" data-testid="decision-meta">
        Decision doc · drafted by {(doc.versions?.[0]?.author_kind ?? 'agent') === 'agent' ? 'an agent' : 'a person'} · v{doc.current_version}
        {' · '}<a href={`/artifacts/${doc.artifact_id}`} className="underline underline-offset-2 hover:text-ink">open the doc</a>
      </span>

      {draft ? (
        <div className="grid gap-3" data-testid="decision-editor">
          <div className="grid gap-1">
            <label className="amp-label" htmlFor={`decision-q-${task.id}`}>Question</label>
            <input id={`decision-q-${task.id}`} className="amp-input" value={draft.ask.question}
              onChange={e => setAsk(a => ({ ...a, question: e.target.value }))} />
          </div>
          <fieldset className="grid gap-2 m-0 p-0 border-0">
            <legend className="amp-label mb-1">Options</legend>
            {draft.ask.options.map((o, i) => (
              <div key={i} className="grid gap-1.5 p-2 border border-line" style={{ borderRadius: 'var(--radius)' }} data-testid="option-row">
                <div className="flex gap-2 items-center">
                  <input className="amp-input flex-1" aria-label={`Option ${i + 1} label`} value={o.label} onChange={e => setOption(i, 'label', e.target.value)} />
                  <label className="flex items-center gap-1 text-[12px] text-ink-2 whitespace-nowrap">
                    <input type="radio" name={`decision-rec-${task.id}`} checked={draft.ask.recommended === i}
                      onChange={() => setAsk(a => ({ ...a, recommended: i }))} aria-label={`Recommend option ${i + 1}`} />Recommended
                  </label>
                  <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" aria-label={`Remove option ${i + 1}`}
                    disabled={draft.ask.options.length <= MIN_OPTIONS} onClick={() => removeOption(i)}><Trash2 size={12} aria-hidden="true" /></button>
                </div>
                <input className="amp-input" aria-label={`Option ${i + 1} consequence`} placeholder="Trade-offs, cost" value={o.consequence}
                  onChange={e => setOption(i, 'consequence', e.target.value)} />
              </div>
            ))}
            {draft.ask.options.length < MAX_OPTIONS && (
              <button type="button" className="amp-btn amp-btn-sm justify-self-start"
                onClick={() => setAsk(a => ({ ...a, options: [...a.options, { label: '', consequence: '' }] }))}>
                <Plus size={12} aria-hidden="true" />Add option</button>
            )}
          </fieldset>
          <div className="grid gap-1">
            <div className="flex items-center gap-2">
              <span className="amp-label">Doc</span>
              <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm ml-auto" aria-pressed={preview} onClick={() => setPreview(p => !p)}>
                {preview ? <Pencil size={12} aria-hidden="true" /> : <Eye size={12} aria-hidden="true" />}{preview ? 'Write' : 'Preview'}</button>
            </div>
            {preview
              ? <div className="border border-line p-3 max-h-[40vh] overflow-y-auto" data-testid="decision-preview"><Markdown size="compact">{draft.content}</Markdown></div>
              : <textarea className="amp-input min-h-[200px] resize-y font-mono text-[12.5px]" aria-label="Doc text" value={draft.content}
                  onChange={e => setDraft(d => d && { ...d, content: e.target.value })} />}
          </div>
          {editError && <span role="alert" className="text-[12px] text-danger-ink">{editError}</span>}
          <div className="flex gap-2">
            <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" disabled={!!busy} onClick={save}>
              {busy === 'save' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Check size={12} aria-hidden="true" />}Save</button>
            <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={!!busy} onClick={() => { setDraft(null); setEditError(null) }}>
              <X size={12} aria-hidden="true" />Cancel</button>
          </div>
        </div>
      ) : <>
        {ask ? <>
          <div className="grid gap-1">
            <span className="amp-label">Question</span>
            <p className="m-0 text-[14px] leading-relaxed text-ink" data-testid="decision-question">{ask.question}</p>
          </div>
          <div className="grid gap-1.5">
            <span className="amp-label">Options</span>
            <OptionPicker ask={ask} value={claimed ? (doc.chosen_option ?? 'other') : choice} onChange={v => { setChoice(v); setError(null) }}
              disabled={!actionable || !!busy} name={`decision-${task.id}`} authors={provenance?.options.map(o => o.author)} />
          </div>
        </> : (
          <p className="m-0 text-[13px] text-warning-ink" role="note">This doc's version has no options; write the decision below.</p>
        )}

        {docText && (
          <div className="max-h-[32vh] overflow-y-auto pr-1 grid gap-2" data-testid="decision-doc">
            {runs
              ? runs.filter(r => r.text.trim()).map((r, i) => (
                  <AgentMark key={i} author={r.author} testId="doc-run"><Markdown size="compact">{r.text}</Markdown></AgentMark>
                ))
              : <Markdown size="compact">{docText}</Markdown>}
          </div>
        )}
      </>}

      {criteria && (
        <div>
          <div className="amp-label mb-1.5">Decided means</div>
          {checklist ? (
            <ul className="m-0 pl-4 grid gap-1 text-[13px] text-ink" aria-label="Decided means">
              {checklist.map((item, i) => <li key={i} data-testid="decided-means-item"><Markdown size="compact">{item.text}</Markdown></li>)}
            </ul>
          ) : <Markdown size="compact">{criteria}</Markdown>}
        </div>
      )}

      {decided ? (
        <div className="grid gap-1 text-[13px]" data-testid="decision-recorded">
          <span className="text-ink">
            <Check size={12} className="inline mr-1 text-success-ink" aria-hidden="true" />
            Decided by {doc.decided_by_name || 'someone'} · v{doc.decided_version}{doc.decided_at ? <> · {when(doc.decided_at)}</> : null}
          </span>
          {doc.decision_text && <span className="text-ink-2">{doc.decision_text}</span>}
          <span className="text-[12px] text-ink-3">The doc is read-only now. To change the decision, plan a new judgment ticket that depends on this one.</span>
        </div>
      ) : claimed ? (
        <div className="grid gap-2 text-[13px]" data-testid="decision-recording">
          {claimStale ? <>
            <span className="text-warning-ink">Recording didn't finish.</span>
            {canAct && (
              <button type="button" className="amp-btn amp-btn-primary amp-btn-sm justify-self-start" disabled={!!busy} onClick={retry}>
                {busy === 'record' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <RotateCcw size={12} aria-hidden="true" />}Retry</button>
            )}
          </> : <span className="text-ink-2"><Loader2 size={11} className="inline mr-1 animate-spin" aria-hidden="true" />Being recorded…</span>}
          {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
        </div>
      ) : !open ? null : blocked ? (
        <div className="grid gap-2">
          {editable && !draft && (
            <div>
              <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy} onClick={beginEdit}>
                <Pencil size={12} aria-hidden="true" />Edit doc</button>
            </div>
          )}
          <span className="text-[12.5px] text-ink-2">Waiting on #{(task.blocked_by_ids?.length ? task.blocked_by_ids : task.dependency_ids).join(', #')} before it can start.</span>
        </div>
      ) : !canAct ? (
        <span className="text-[12.5px] text-ink-2">Only {ownerName || 'its owner'} decides this.</span>
      ) : draft ? null : (
        <form className="grid gap-2" onSubmit={e => { e.preventDefault(); record() }}>
          <div>
            <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy} onClick={beginEdit}>
              <Pencil size={12} aria-hidden="true" />Edit doc</button>
          </div>
          <label htmlFor={textId} className="amp-label">Your decision</label>
          <textarea id={textId} className="amp-input min-h-[56px] resize-y" value={text} aria-required
            onChange={e => { setText(e.target.value); setError(null) }}
            placeholder="What you decided. The tickets waiting on this receive it" />
          {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
          <div className="flex gap-2 flex-wrap items-center">
            {task.state === 'backlog' && (
              <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy} onClick={start}>
                {busy === 'start' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Play size={12} aria-hidden="true" />}Start</button>
            )}
            {ask && (
              <button type="button" className="amp-btn amp-btn-sm ml-auto" disabled={!!busy} onClick={acceptRecommendation}>
                <Sparkles size={12} aria-hidden="true" />Accept recommendation</button>
            )}
            <button type="submit" className={`amp-btn amp-btn-primary amp-btn-sm${ask ? '' : ' ml-auto'}`} disabled={!!busy}>
              {busy === 'record' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Check size={12} aria-hidden="true" />}Record decision</button>
          </div>
        </form>
      )}
    </div>
  )
}
