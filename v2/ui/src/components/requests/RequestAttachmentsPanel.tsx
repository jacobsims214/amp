/**
 * A request's private files (AMP #3979; the `Attachments` block of approved
 * mockup 37 v1's request card, the restricted-file card of its edge sheet, and
 * the `What you asked for` / delivered file rows of artifact 38 v1).
 *
 * Request attachments are private and separate from artifacts (contract §1).
 * This panel shows the metadata a listing answers — never bytes — and offers
 * only the acts the server says are possible right now:
 *
 *   - **download** mints a short-lived, single-use, team-scoped URL
 *     (`POST …/attachments/{id}/download`), then navigates to it. The bytes are
 *     served by a second route that re-checks the caller, so a URL is an
 *     additional capability, never a way around signing in. The panel never
 *     builds a URL itself and never holds a token in state longer than the
 *     navigation.
 *   - **keep / unkeep** is the team's retention promise (D3): a kept file is
 *     never purged. A requester cannot pin their own upload, so the control is
 *     the team's alone.
 *   - **delete** removes the file and its bytes. A second delete is the surface's
 *     404, not a silent success, so the panel treats "already gone" as the
 *     not-found state and says so.
 *   - **release** of a held file is a *team lead's* act with its own audited
 *     path, and there is deliberately no route for it in this build. The panel
 *     therefore renders the hold truthfully and offers no release control at all
 *     — a disabled button labelled "Release" would imply the capability exists
 *     here and is merely blocked, which would be a lie about authorization.
 *
 * Two facts the panel must state rather than hide, because the mockups make them
 * promises: no malware scanner ships in v1 (`scan_state: 'unsupported'` is
 * *unknown*, not clean), and files are purged 90 days after the request closes
 * unless somebody keeps them. `state.ready` is the server's single decision —
 * both the mint and the byte-serving route call the same function — so the panel
 * disables the download on `state.ready === false` and prints `state.refusal`'s
 * sentence instead of attempting the write and translating the refusal.
 */
import { useEffect, useRef, useState } from 'react'
import {
  Download, FileArchive, FileCode2, FileImage, FileText, Loader2, Lock, Pin, PinOff, Trash2,
} from 'lucide-react'
import { requestApi } from '../../api/client'
import { goToDownload } from '../../lib/download'
import { RequestPanelEmpty, RequestPanelError, RequestPanelSection } from './RequestPanelSection'
import {
  requestFileSize,
  requestFileType,
  requestRefusalText,
  requestScanText,
} from '../../lib/requestLabels'
import type { RequestAttachmentFacts, RequestAttachmentLimits, RequestAttachmentPanel } from '../../types'

/** What a file row may do; absent values mean "not offered to this viewer". */
export interface AttachmentActions {
  /** The caller's own user id, for "you uploaded this" and the requester's own-file rules. */
  viewerUserId?: number | null
  /** Team members may keep/unkeep and delete; a requester may only delete their own. */
  canManage?: boolean
  /** A team lead may release a held file — there is no route yet, so this only changes the copy. */
  isTeamLead?: boolean
}

export interface RequestAttachmentsPanelProps extends AttachmentActions {
  /**
   * The inbox the request belongs to. Required, not inferred: every attachment
   * route is team-scoped (the request id alone is the cross-tenant lookup this
   * surface exists without), and the panel's own answer names no team.
   */
  teamId: number
  /** The panel read, or null while it has not arrived. */
  panel: RequestAttachmentPanel | null
  /** The read's failure, if it failed. */
  error?: string | null
  /** Retry the read. */
  onRetry?: () => void
  /** Called with the panel after a keep/delete succeeded, so the page can re-read. */
  onChanged?: () => void
  /** True while the panel is loading and nothing is shown yet. */
  loading?: boolean
}

/** The icon for a sniffed content type. Display only: it never decides whether a file may be served. */
function FileIcon({ contentType }: { contentType: string }) {
  const type = (contentType || '').toLowerCase()
  const Icon = type.includes('zip') || type.includes('tar') ? FileArchive
    : type.startsWith('image/') ? FileImage
    : type.includes('xml') || type.includes('json') || type.includes('text/html') ? FileCode2
    : FileText
  return <Icon size={14} className="text-ink-3 flex-shrink-0" aria-hidden="true" />
}

/** A chip: the same shape the mockup uses for scan, hold and retention facts. */
function Chip({ children, tone = 'line', title }: { children: React.ReactNode; tone?: 'line' | 'warn' | 'quiet' | 'ok'; title?: string }) {
  const style = tone === 'warn'
    ? { borderColor: 'var(--warning)', color: 'var(--warning-ink)', background: 'color-mix(in srgb, var(--warning) 8%, var(--surface))' }
    : tone === 'ok'
      ? { borderColor: 'var(--success)', color: 'var(--success-ink)', background: 'transparent' }
      : tone === 'quiet'
        ? { borderColor: 'var(--line-str)', color: 'var(--ink-3)', background: 'transparent' }
        : { borderColor: 'var(--line)', color: 'var(--ink-2)', background: 'var(--surface-2)' }
  return (
    <span className="inline-flex items-center gap-1 whitespace-nowrap border px-1.5 py-[3px] font-label text-[9.5px] font-semibold uppercase leading-none tracking-[0.1em]"
      style={{ borderRadius: 'var(--radius)', ...style }} title={title}>
      {children}
    </span>
  )
}

/** Why a file cannot be downloaded right now, in the gate's own words. Empty when it can. */
function refusalText(file: RequestAttachmentFacts): string {
  if (file.state.ready) return ''
  const sentence = requestRefusalText(file.state.refusal)
  return sentence || 'This file is not available right now.'
}

/** One file: its name, its honest state, and the acts the caller actually has. */
function FileRow({ teamId, requestId, file, actions, onChanged, onError }: {
  teamId: number
  requestId: number
  file: RequestAttachmentFacts
  actions: AttachmentActions
  onChanged?: () => void
  onError: (message: string | null) => void
}) {
  const [busy, setBusy] = useState<'download' | 'keep' | 'delete' | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const alive = useRef(true)
  useEffect(() => () => { alive.current = false }, [])

  const scan = requestScanText(file.scan_state)
  const refusal = refusalText(file)
  const mine = actions.viewerUserId != null && file.uploaded_by === actions.viewerUserId
  const canManage = !!actions.canManage
  // A requester may withdraw their own file; a team member may remove any file on
  // the inbox's requests. The server re-checks both (RequestRetentionCapabilityFor).
  const canDelete = canManage || mine
  // Keeping is the team's retention promise, never the uploader's (D3).
  const canKeep = canManage

  const download = async () => {
    if (busy || !file.state.ready) return
    setBusy('download'); onError(null)
    try {
      const link = await requestApi.downloadLink(teamId, requestId, file.id)
      // A same-origin path the server minted. Navigating (rather than fetching)
      // keeps the bytes out of the SPA and lets the response's own
      // Content-Disposition name the file.
      if (alive.current) goToDownload(link.url)
    } catch (e) {
      if (alive.current) onError(e instanceof Error ? e.message : 'That download could not be started.')
    } finally {
      if (alive.current) setBusy(null)
    }
  }

  const keep = async (next: boolean) => {
    if (busy) return
    setBusy('keep'); onError(null)
    try {
      await requestApi.keepAttachment(teamId, requestId, file.id, next)
      if (alive.current) onChanged?.()
    } catch (e) {
      if (alive.current) onError(e instanceof Error ? e.message : 'That change to the file could not be saved.')
    } finally {
      if (alive.current) setBusy(null)
    }
  }

  const remove = async () => {
    if (busy) return
    setBusy('delete'); onError(null)
    try {
      await requestApi.deleteAttachment(teamId, requestId, file.id)
      if (alive.current) { setConfirmDelete(false); onChanged?.() }
    } catch (e) {
      if (alive.current) onError(e instanceof Error ? e.message : 'That file could not be removed.')
    } finally {
      if (alive.current) setBusy(null)
    }
  }

  return (
    <li className="flex flex-col gap-1.5 border border-line px-2.5 py-2 min-w-0" data-testid="attachment-row"
      data-file-id={file.id} data-ready={file.state.ready ? 'true' : 'false'}
      style={file.state.held ? { borderColor: 'color-mix(in srgb, var(--warning) 55%, var(--line))' } : undefined}>
      <div className="flex items-center gap-2 flex-wrap min-w-0">
        <FileIcon contentType={file.content_type} />
        <span className="text-[12.5px] font-medium text-ink min-w-0 truncate" title={file.file_name} data-testid="attachment-name">
          {file.file_name}
        </span>
        {file.held ? <Chip tone="warn" title="A restricted type or a code-like name. A team lead must release it before anyone can read it."><Lock size={9} aria-hidden="true" />Held</Chip> : null}
        <Chip tone={scan.bad ? 'warn' : file.scan_state === 'clean' ? 'ok' : 'quiet'} title={scan.title}>{scan.label}</Chip>
        {file.keep ? <Chip tone="ok" title="Pinned by the team: this file is never purged."><Pin size={9} aria-hidden="true" />Kept</Chip> : null}
        <span className="ml-auto text-[11px] text-ink-3 flex-shrink-0 tabular-nums font-mono" data-testid="attachment-meta">
          {requestFileSize(file.size_bytes)} · {requestFileType(file.content_type)}
        </span>
      </div>

      {refusal ? (
        <p className="m-0 text-[11.5px] text-ink-2 leading-snug" data-testid="attachment-refusal">{refusal}</p>
      ) : null}
      {file.expires_at && !file.keep ? (
        <p className="m-0 text-[11.5px] text-ink-3 leading-snug">
          Purged after {new Date(file.expires_at).toLocaleDateString()} unless the team keeps it.
        </p>
      ) : null}

      <div className="flex items-center gap-1.5 flex-wrap">
        <button type="button" className="amp-btn amp-btn-sm" disabled={!file.state.ready || busy !== null}
          onClick={() => void download()}
          title={file.state.ready ? 'Mint a short-lived download link' : refusal}>
          {busy === 'download' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Download size={11} aria-hidden="true" />}Download
        </button>
        {canKeep ? (
          <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" disabled={busy !== null}
            onClick={() => void keep(!file.keep)}
            title={file.keep ? 'Let the request’s own retention clock apply again' : 'Never purge this file'}>
            {busy === 'keep' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : file.keep ? <PinOff size={11} aria-hidden="true" /> : <Pin size={11} aria-hidden="true" />}
            {file.keep ? 'Unkeep' : 'Keep'}
          </button>
        ) : null}
        {canDelete ? (confirmDelete ? (
          <span className="inline-flex gap-1.5">
            {/* autoFocus: the two-step confirm replaces the Delete button in
                place, so focus moves to the irreversible step instead of
                dropping to the document body (which would lose a keyboard
                user's place and make Escape-free recovery impossible). */}
            <button type="button" className="amp-btn amp-btn-sm amp-btn-accent" autoFocus disabled={busy !== null} onClick={() => void remove()}>
              {busy === 'delete' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : null}Delete for good
            </button>
            <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" disabled={busy !== null} onClick={() => setConfirmDelete(false)}>Cancel</button>
          </span>
        ) : (
          <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" disabled={busy !== null} onClick={() => setConfirmDelete(true)}
            title="Removes the file and its bytes; a kept file's pin goes with it">
            <Trash2 size={11} aria-hidden="true" />Delete
          </button>
        )) : null}
      </div>
    </li>
  )
}

/** The bounds a listing carries, in one quiet sentence. */
function BoundsNote({ limits }: { limits: RequestAttachmentLimits }) {
  return (
    <p className="m-0 text-[11.5px] text-ink-3 leading-snug" data-testid="attachment-bounds">
      {limits.max_files} files, {requestFileSize(limits.max_file_bytes)} each and {requestFileSize(limits.max_total_bytes)} in
      total. Files are private to this request, never linked or served publicly, and are purged {limits.retention_days} days
      after it closes unless the team keeps one.
    </p>
  )
}

export function RequestAttachmentsPanel(props: RequestAttachmentsPanelProps) {
  const { teamId, panel, error, onRetry, onChanged, loading } = props
  const [actionError, setActionError] = useState<string | null>(null)
  // The write's own failure is cleared whenever the list is re-read: a stale
  // "could not delete" over a row that is now gone would be exactly the kind of
  // lie this panel exists to avoid. Adjusted during render rather than in an
  // effect, so the cleared list and the cleared message are the same paint.
  const [seenPanel, setSeenPanel] = useState(panel)
  if (seenPanel !== panel) {
    setSeenPanel(panel)
    setActionError(null)
  }
  const items = panel?.items ?? []

  return (
    <RequestPanelSection
      label="Attachments"
      note="Private to this request"
      actions={
        panel ? <span className="text-[11.5px] text-ink-3 tabular-nums">{panel.count} {panel.count === 1 ? 'file' : 'files'}</span> : null
      }
    >
      {error ? (
        <div className="flex items-center gap-2 flex-wrap">
          <RequestPanelError message={error} />
          {onRetry ? <button type="button" className="amp-btn amp-btn-sm" onClick={onRetry}>Retry</button> : null}
        </div>
      ) : null}

      {loading && !panel ? (
        <p className="m-0 text-[12.5px] text-ink-3" role="status">Loading the files…</p>
      ) : null}

      {actionError ? <RequestPanelError message={actionError} /> : null}

      {panel && items.length === 0 ? (
        <RequestPanelEmpty>
          No files on this request. Anything the requester attached, or the team uploaded, would be listed here — and never
          reachable outside it.
        </RequestPanelEmpty>
      ) : null}

      {items.length > 0 && panel ? (
        <ul className="list-none m-0 p-0 flex flex-col gap-1.5 min-w-0" aria-label="Request attachments">
          {items.map(file => (
            <FileRow key={file.id} teamId={teamId} requestId={panel.request_id} file={file}
              actions={props} onChanged={onChanged} onError={setActionError} />
          ))}
        </ul>
      ) : null}

      {panel ? <BoundsNote limits={panel.limits} /> : null}
    </RequestPanelSection>
  )
}
