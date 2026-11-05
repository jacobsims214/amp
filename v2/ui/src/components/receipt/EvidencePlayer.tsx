import { Download, FileText, Film, ImageIcon, TimerOff } from 'lucide-react'
import { api } from '../../api/client'
import type { ReceiptEvidence } from '../../types'

interface Props {
  evidence: ReceiptEvidence[]
}

const isVideo = (e: ReceiptEvidence) => e.content_type.startsWith('video/')
const isImage = (e: ReceiptEvidence) => e.content_type.startsWith('image/')

function sizeText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function dateText(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

/**
 * A receipt's evidence (#126): the video with its poster, the screenshots,
 * and a download link for each stored file. Files are served by amp-api
 * (GET /api/receipts/<key>, Range works, so the video seeks) under the
 * same safety headers as artifacts; the page only ever shows them through
 * <video> and <img>, never inline HTML. Expired evidence (unkept past
 * retention) says when it went; a file still uploading says so.
 */
export function EvidencePlayer({ evidence }: Props) {
  if (evidence.length === 0) return null
  const stored = evidence.filter(e => e.status === 'stored')
  const expired = evidence.filter(e => e.status === 'expired')
  const pending = evidence.filter(e => e.status === 'pending')
  const videos = stored.filter(isVideo)
  const poster = stored.find(e => e.name === 'poster.gif')
  // The poster belongs to the video; shown alone only when there's none.
  const images = stored.filter(e => isImage(e) && !(videos.length > 0 && e === poster))

  return (
    <div className="grid gap-2 mt-1" data-testid="evidence-player">
      <span className="amp-label text-ink-3">Evidence</span>
      {videos.map(v => (
        <figure key={v.id} className="m-0 grid gap-1">
          <video controls preload="metadata" className="w-full max-h-[320px] bg-black border border-line"
            src={api.receiptEvidenceUrl(v.key)} poster={poster ? api.receiptEvidenceUrl(poster.key) : undefined}
            aria-label={`Recording ${v.name}`} data-testid="evidence-video" />
          <figcaption className="flex items-center gap-1.5 text-ink-3">
            <Film size={11} aria-hidden="true" />{v.name}
            {v.duration_ms ? ` · ${(v.duration_ms / 1000).toFixed(1)} s` : ''}
          </figcaption>
        </figure>
      ))}
      {images.length > 0 && (
        <div className="flex flex-wrap gap-2" data-testid="evidence-images">
          {images.map(i => (
            <a key={i.id} href={api.receiptEvidenceUrl(i.key)} target="_blank" rel="noreferrer" className="block border border-line hover:border-ink-3"
              title={i.name}>
              <img src={api.receiptEvidenceUrl(i.key)} alt={i.name} loading="lazy" className="block max-h-[120px] max-w-[200px] object-contain" />
            </a>
          ))}
        </div>
      )}
      {stored.length > 0 && (
        <ul className="m-0 pl-0 list-none grid gap-0.5" data-testid="evidence-downloads">
          {stored.map(e => (
            <li key={e.id} className="flex items-center gap-1.5 text-ink-2">
              {isVideo(e) ? <Film size={11} aria-hidden="true" className="text-ink-3" />
                : isImage(e) ? <ImageIcon size={11} aria-hidden="true" className="text-ink-3" />
                  : <FileText size={11} aria-hidden="true" className="text-ink-3" />}
              <a href={api.receiptEvidenceUrl(e.key)} download={e.name} className="underline underline-offset-2 hover:text-ink">
                {e.name}
              </a>
              <span className="text-ink-3">{sizeText(e.bytes)}</span>
              <Download size={11} aria-hidden="true" className="text-ink-3" />
            </li>
          ))}
        </ul>
      )}
      {expired.length > 0 && (
        <div className="flex items-center gap-1.5 text-ink-3" data-testid="evidence-expired">
          <TimerOff size={11} aria-hidden="true" />
          {(() => {
            const on = dateText(expired[0].expires_at)
            const names = expired.map(e => e.name).join(', ')
            return on ? `Evidence expired on ${on}: ${names}` : `Evidence expired: ${names}`
          })()}
        </div>
      )}
      {pending.length > 0 && (
        <div className="text-ink-3" data-testid="evidence-pending">
          Not uploaded: {pending.map(e => e.name).join(', ')}
        </div>
      )}
    </div>
  )
}
