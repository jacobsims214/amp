/**
 * A request's discussion (AMP #3979; the `Discussion` and `Clarification`
 * sections of approved mockup 37 v1, and the shared thread artifact 38 v1 shows
 * a requester).
 *
 * The one thing this component exists to get right is that a thread row's
 * *kind* is not decoration: it decides who can read the row.
 *
 *   - `comment` — the desk talking among itself. Internal, and never shown to a
 *     requester. (The one exception the server already applies: a requester's
 *     own plain comment comes back to them, which is why requester mode keeps a
 *     `comment` row only when `from` is `you`.)
 *   - `question` — the desk asking this requester something. Shared by
 *     definition (I5 makes it the prerequisite of `needs_information`).
 *   - `answer` — the requester's reply, and the only kind that returns the
 *     request to the queue. Shared by definition.
 *   - `system` — the record's own voice. It can name internal actors, so it is
 *     never shared.
 *
 * That rule is the server's (`requesterSharedComments` in
 * `internal/service/request_views.go`). The component mirrors it rather than
 * interpreting it: in requester mode it can only be handed the projection's row
 * type at compile time — `RequesterComment` has no author, no id and no user id
 * — and it drops anything whose kind is not shareable at runtime, so an
 * accidentally-passed team array cannot leak an internal note.
 *
 * Markdown is the shared `Markdown` in `compact` mode, which is what makes a
 * request's text safe to show: raw HTML stays literal and a remote image renders
 * as an inert placeholder with its URL, never a fetch (D13).
 */
import { useState } from 'react'
import { CornerDownRight, Loader2, Send } from 'lucide-react'
import { Markdown } from '../ui/Markdown'
import { Avatar } from '../ui/Avatar'
import { RequestPanelEmpty, RequestPanelError, RequestPanelSection } from './RequestPanelSection'
import { requestApi } from '../../api/client'
import { ago, isRelayed, relayedFor } from '../../lib/approval'
import { REQUEST_COMMENT_LABEL, REQUEST_COMMENT_SHARED } from '../../lib/requestLabels'
import type { RequestComment, RequestCommentAnswer, RequesterComment } from '../../types'

/** The team's thread: every row, each with the author label the DTO recorded at write time. */
export interface TeamDiscussionProps {
  mode: 'team'
  comments: RequestComment[]
  /** The viewer, so their own rows read "You" rather than their recorded name. */
  viewerUserId?: number | null
  /** The post's tenant and serial. Omitted (or `canComment` false) means read-only. */
  tenant?: { teamId: number; requestId: number }
  /** Whether the caller may post. The server decides the same question again on the write. */
  canComment?: boolean
  /** Called after a comment was written, so the page can re-read the thread. */
  onPosted?: (answer: RequestCommentAnswer) => void
}

/**
 * The requester's thread: only the row type the projection carries. There is no
 * `author`, no `id`, no `user_id` and no revision in it, so a component built on
 * this arm cannot name a team member even by accident.
 */
export interface RequesterDiscussionProps {
  mode: 'requester'
  comments: RequesterComment[]
}

export type RequestDiscussionProps = TeamDiscussionProps | RequesterDiscussionProps

/** The kind marker beside a row: shared-with-the-requester kinds are marked, internal ones are quiet. */
function KindTag({ kind }: { kind: RequestComment['kind'] }) {
  const shared = REQUEST_COMMENT_SHARED[kind]
  return (
    <span
      className="inline-flex items-center whitespace-nowrap border px-1.5 py-[3px] font-label text-[9.5px] font-semibold uppercase leading-none tracking-[0.1em]"
      style={{
        borderRadius: 'var(--radius)',
        borderColor: shared ? 'var(--line-str)' : 'var(--line)',
        color: shared ? 'var(--ink-2)' : 'var(--ink-3)',
        background: shared ? 'var(--surface-2)' : 'transparent',
      }}
      title={shared
        ? 'This row is part of the exchange with the requester.'
        : 'Internal. A requester never sees this row.'}
    >
      {REQUEST_COMMENT_LABEL[kind]}
    </span>
  )
}

/**
 * A one-line preview of a row's words for the answer's quote.
 *
 * The quoted question is already in the thread above, so the preview only has to
 * point at it. Markdown emphasis marks are dropped rather than rendered: a
 * truncated markdown fragment can cut a code span in half and read as garbage,
 * and the answer belongs to a row the reader can scroll to.
 */
function preview(body: string, max = 140): string {
  const flat = body.replace(/`+/g, '').replace(/\s+/g, ' ').trim()
  return flat.length > max ? `${flat.slice(0, max)}…` : flat
}

/** One team row: who, what kind, when, and the words — as markdown, images inert. */
function TeamComment({ comment, viewerUserId, answerTo }: {
  comment: RequestComment
  viewerUserId?: number | null
  /** The question an answer replies to, resolved from the list by id order. */
  answerTo?: RequestComment
}) {
  const mine = viewerUserId != null && comment.author_user_id === viewerUserId
  const relayed = isRelayed(comment.author)
  const author = relayed ? relayedFor(comment.author) : comment.author.trim()
  return (
    <article className="flex items-start gap-2.5 min-w-0" data-testid="discussion-row" data-kind={comment.kind}>
      <Avatar name={author || 'Unknown'} size={20} title={author || 'Unknown author'} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 mb-1">
          <span className="text-[12.5px] font-semibold text-ink break-all">{mine ? 'You' : author || 'Unknown author'}</span>
          <KindTag kind={comment.kind} />
          {relayed ? (
            <span className="font-label text-[9.5px] font-semibold uppercase tracking-[0.1em] px-1 py-[2px] border border-dashed border-line-str text-ink-2"
              title="An agent passed this on for the person over MCP. They didn't type it in AMP themselves.">
              Relayed by agent
            </span>
          ) : null}
          <time className="text-[11.5px] text-ink-3 ml-auto flex-shrink-0 tabular-nums" dateTime={comment.created_at} title={new Date(comment.created_at).toLocaleString()}>
            {ago(comment.created_at)}
          </time>
        </div>
        {answerTo ? (
          <blockquote className="m-0 mb-1.5 pl-2.5 text-[12px] text-ink-3 border-l-2" style={{ borderColor: 'var(--line-str)' }}>
            <CornerDownRight size={11} className="inline mr-1" aria-hidden="true" />
            Answering: {preview(answerTo.body)}
          </blockquote>
        ) : null}
        <div className="text-ink" data-testid="discussion-body">
          <Markdown size="compact">{comment.body}</Markdown>
        </div>
      </div>
    </article>
  )
}

/** One requester-visible row: no author identity, only "you" or "the desk". */
function RequesterCommentRow({ comment }: { comment: RequesterComment }) {
  const mine = comment.from === 'you'
  return (
    <article className="flex items-start gap-2.5 min-w-0" data-testid="discussion-row" data-kind={comment.kind}>
      <Avatar name={mine ? 'You' : 'Desk'} size={20} title={mine ? 'You' : 'The desk'} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 mb-1">
          <span className="text-[12.5px] font-semibold text-ink">{mine ? 'You' : 'The desk'}</span>
          <KindTag kind={comment.kind} />
          <time className="text-[11.5px] text-ink-3 ml-auto flex-shrink-0 tabular-nums" dateTime={comment.at} title={new Date(comment.at).toLocaleString()}>
            {ago(comment.at)}
          </time>
        </div>
        <div className="text-ink" data-testid="discussion-body">
          <Markdown size="compact">{comment.body}</Markdown>
        </div>
      </div>
    </article>
  )
}

/**
 * The composer: one internal comment, posted in the caller's own voice.
 *
 * There is deliberately no kind picker. A `question` goes through the lifecycle's
 * needs-information route (asking obliges an answer, I5, and moves the request),
 * an `answer` is the requester's alone, and `system` is nobody's to post — so a
 * picker here would only offer refusals. The server re-checks the voice anyway.
 */
function CommentComposer({ tenant, onPosted }: { tenant: { teamId: number; requestId: number }; onPosted?: (a: RequestCommentAnswer) => void }) {
  const [body, setBody] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [posted, setPosted] = useState(false)
  const canPost = body.trim() !== '' && !busy
  const send = async () => {
    if (!canPost) return
    setBusy(true); setError(null); setPosted(false)
    try {
      const answer = await requestApi.comment(tenant.teamId, tenant.requestId, { kind: 'comment', body: body.trim() })
      setBody('')
      setPosted(true)
      onPosted?.(answer)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'That comment was not posted.')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex flex-col gap-2" data-testid="discussion-composer">
      <label htmlFor="request-comment" className="amp-label">Comment</label>
      <textarea
        id="request-comment"
        className="amp-input min-h-[68px] resize-y"
        placeholder="Comment — visible to the team only"
        value={body}
        onChange={e => { setBody(e.target.value); setPosted(false) }}
        onKeyDown={e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); void send() } }}
      />
      {error ? <RequestPanelError message={error} /> : null}
      {posted && !error ? (
        <p className="m-0 text-[12px] text-ink-3" role="status" data-testid="comment-posted">
          Posted. The thread shows it on the next read.
        </p>
      ) : null}
      <div className="flex items-center gap-2 flex-wrap">
        <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" disabled={!canPost} onClick={() => void send()}>
          {busy ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Send size={11} aria-hidden="true" />}Post
        </button>
        <span className="text-[11.5px] text-ink-3 min-w-0">Posted in your own name. ⌘↵ sends.</span>
      </div>
    </div>
  )
}

/** Whether a projection row may be shown to its requester, at runtime. */
function shareable(c: RequesterComment): boolean {
  switch (c.kind) {
    case 'question':
    case 'answer':
      return true
    case 'comment':
      // A plain comment is shared only when the requester wrote it — the
      // server's own rule, restated so a hand-built array cannot leak one.
      return c.from === 'you'
    default:
      return false
  }
}

/** The answer row's question: the last `question` posted before it. The thread is append order. */
function questionBefore(rows: RequestComment[], answer: RequestComment): RequestComment | undefined {
  let found: RequestComment | undefined
  for (const row of rows) {
    if (row.id === answer.id) break
    if (row.kind === 'question') found = row
  }
  return found
}

function TeamDiscussion(props: TeamDiscussionProps) {
  const rows = props.comments
  return (
    <RequestPanelSection
      label="Discussion"
      note="Internal comments stay internal"
      actions={<span className="text-[11.5px] text-ink-3 tabular-nums">{rows.length} {rows.length === 1 ? 'row' : 'rows'}</span>}
    >
      {rows.length === 0 ? (
        <RequestPanelEmpty>
          No discussion yet. A comment here stays internal; asking the requester something moves the request to Needs
          information and obliges an answer.
        </RequestPanelEmpty>
      ) : null}
      <div className="flex flex-col gap-3.5 min-w-0" role="list" aria-label="Discussion">
        {rows.map(c => (
          <div role="listitem" key={c.id} className="min-w-0">
            <TeamComment comment={c} viewerUserId={props.viewerUserId} answerTo={c.kind === 'answer' ? questionBefore(rows, c) : undefined} />
          </div>
        ))}
      </div>
      {props.tenant && props.canComment !== false
        ? <CommentComposer tenant={props.tenant} onPosted={props.onPosted} />
        : <p className="m-0 text-[12px] text-ink-3">Commenting needs a signed-in team member with the inbox open.</p>}
    </RequestPanelSection>
  )
}

function RequesterDiscussion({ comments }: RequesterDiscussionProps) {
  // The projection's own rule, applied once more at render: only the two
  // directed kinds, and a plain comment only when the requester wrote it.
  const rows = comments
    .filter(shareable)
    .map(c => ({ row: c, key: `${c.kind}-${c.at}-${c.body.length}-${c.body.slice(0, 24)}` }))
  return (
    <RequestPanelSection
      label="Discussion"
      note="Messages shared with you"
      actions={<span className="text-[11.5px] text-ink-3 tabular-nums">{rows.length} {rows.length === 1 ? 'message' : 'messages'}</span>}
    >
      {rows.length === 0 ? (
        <RequestPanelEmpty>No messages yet. Anything the desk shares with you appears here.</RequestPanelEmpty>
      ) : null}
      <div className="flex flex-col gap-3.5 min-w-0" role="list" aria-label="Discussion">
        {rows.map(({ row, key }) => (
          <div role="listitem" key={key} className="min-w-0"><RequesterCommentRow comment={row} /></div>
        ))}
      </div>
    </RequestPanelSection>
  )
}

/**
 * A request's discussion thread. The `mode` discriminant decides both the row
 * shape and the visibility rule, so the requester arm cannot be handed a team
 * row: `RequesterComment` and `RequestComment` share no field that names a
 * person, and the union refuses a mixed array at compile time.
 */
export function RequestDiscussion(props: RequestDiscussionProps) {
  return props.mode === 'requester' ? <RequesterDiscussion {...props} /> : <TeamDiscussion {...props} />
}
