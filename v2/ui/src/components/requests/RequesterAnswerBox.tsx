/**
 * The requester's answer box (AMP #3987, build slice J6; approved mockup artifact
 * 38 v1's "The desk needs a bit more from you" card).
 *
 * It exists in exactly one state of one screen — `needs_information` — and the
 * reason is the lifecycle, not the layout: an answer is the only way out of that
 * state (`planRequestAnswer`), the destination is read from the request's history
 * rather than chosen by the caller, and the server refuses an answer from any
 * other state. So this component renders a question and a reply, and nothing
 * else: no state picker, no "and also" fields, no way to close the request.
 *
 * ## The three things it is careful about
 *
 * 1. **The words are never lost.** A refusal leaves the answer in the box with
 *    the notice above it, exactly like the submit form: a person who wrote three
 *    paragraphs and lost them to a 409 does not write them again.
 *
 * 2. **A double send cannot answer twice.** `answer_to` is the *question's*
 *    comment id from the thread, not a client-side guess, and the act is a
 *    transition: a second submission of the same answer is refused by the server
 *    as a conflict rather than appended twice. The button is disabled while the
 *    request is in flight, which is the UI half of the same rule.
 *
 * 3. **The revision travels when the page has one.** `expected_rev` is passed
 *    through when the page knows it; the answer route accepts an absent one
 *    ("whatever the row is now"), so a page that never saw a revision still
 *    works — but a page that did see one gets a 409 instead of silently
 *    answering a question that was withdrawn while they typed.
 */
import { useState } from 'react'
import { Loader2, Send } from 'lucide-react'
import { requestApi } from '../../api/client'
import { ago } from '../../lib/approval'
import { answerCounter, requesterActNotice, validateAnswer, type RequesterActNotice } from '../../lib/requesterStatus'
import type { RequesterQuestion } from '../../types'

export interface RequesterAnswerBoxProps {
  /** The reference the requester holds. Every requester act is addressed by it. */
  publicRef: string
  /** The desk's open question, as the projection carries it. */
  question: RequesterQuestion
  /** The desk's public name, for the card's attribution line. */
  teamName?: string
  /**
   * The revision the page read, when it has one. A page holding the projection
   * does not (the projection carries no rev), so this is optional and the route
   * accepts its absence.
   */
  expectedRev?: number
  /**
   * The question comment's id, when the page knows it. The projection does not
   * carry comment ids, so this is normally absent and the server answers the
   * question currently open — which is the newest one in the thread.
   */
  questionId?: number
  /** Called after the server recorded the answer, with the state it returned to. */
  onAnswered?: (state: string, rev: number) => void
}

export function RequesterAnswerBox({
  publicRef, question, teamName, expectedRev, questionId, onAnswered,
}: RequesterAnswerBoxProps) {
  const [answer, setAnswer] = useState('')
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<RequesterActNotice | null>(null)
  const [error, setError] = useState<string | null>(null)

  const invalid = validateAnswer(answer)
  const counter = answerCounter(answer)
  const canSend = !busy && invalid === null

  const send = async () => {
    if (!canSend) {
      setError(invalid)
      return
    }
    setBusy(true)
    setError(null)
    setNotice(null)
    try {
      const res = await requestApi.answer(publicRef, {
        answer: answer.trim(),
        ...(questionId && questionId > 0 ? { answer_to: questionId } : {}),
        ...(expectedRev && expectedRev > 0 ? { expected_rev: expectedRev } : {}),
      })
      // The answer is gone from the box only once the server has it: a cleared
      // box after a failure would be the same lie as a cleared form.
      setAnswer('')
      onAnswered?.(res.request.state, res.request.rev)
    } catch (e) {
      setNotice(requesterActNotice(e, 'answer'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div
      className="border bg-surface px-4 py-3.5"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)', borderLeft: '5px solid var(--accent)' }}
      data-testid="requester-answer-box"
    >
      <h3 className="m-0 mb-2 font-label text-[12px] font-semibold uppercase tracking-[0.16em]" style={{ color: 'var(--danger-ink)' }}>
        The desk needs a bit more from you
      </h3>

      <div className="mb-2 font-mono text-[11.5px] text-ink-3" data-testid="answer-question-meta">
        {teamName || 'The desk'} · {ago(question.at) || 'just now'}
      </div>
      <div className="mb-3 whitespace-pre-wrap break-words text-[13.5px] leading-relaxed text-ink" data-testid="answer-question">
        {question.body}
      </div>

      {notice && (
        <div className="mb-3">
          <div
            role="alert"
            data-testid="answer-notice"
            className="border px-3 py-2.5"
            style={{ borderRadius: 'var(--radius)', borderColor: 'var(--danger)', background: 'var(--surface)' }}
          >
            <b className="block text-[12.5px] font-semibold text-ink">{notice.title}</b>
            <p className="m-0 mt-1 text-[12px] leading-relaxed text-ink-2">{notice.body}</p>
          </div>
        </div>
      )}

      <form
        noValidate
        onSubmit={e => { e.preventDefault(); void send() }}
        aria-busy={busy}
      >
        <label htmlFor="requester-answer" className="mb-1.5 block font-label text-[11px] font-semibold uppercase tracking-[0.14em] text-ink-2">
          Your answer
        </label>
        <textarea
          id="requester-answer"
          className="amp-input min-h-[88px] resize-y text-base leading-relaxed sm:text-[13px]"
          value={answer}
          onChange={e => { setAnswer(e.target.value); if (error) setError(null) }}
          placeholder="e.g. The October close — the live books match fine."
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? 'requester-answer-err' : undefined}
          disabled={busy}
        />
        {error && (
          <p id="requester-answer-err" className="m-0 mt-1.5 text-[12px] leading-snug" style={{ color: 'var(--danger-ink)' }}>
            {error}
          </p>
        )}
        {counter && (
          <p className="m-0 mt-1 text-[11px] tabular-nums" style={{ color: 'var(--ink-3)' }} aria-live="polite">
            {counter}
          </p>
        )}

        <div className="mt-2.5 flex flex-wrap items-center gap-2">
          <button
            type="submit"
            className="amp-btn amp-btn-primary w-full sm:w-auto"
            style={{ minHeight: 44 }}
            disabled={busy}
          >
            {busy
              ? <><Loader2 size={12} className="animate-spin" aria-hidden="true" />Sending…</>
              : <><Send size={11} aria-hidden="true" />Send answer</>}
          </button>
          <span className="m-0 text-[11.5px] leading-snug text-ink-3">
            Answering puts your request straight back in the desk's queue — nothing restarts.
          </span>
        </div>
      </form>
    </div>
  )
}
