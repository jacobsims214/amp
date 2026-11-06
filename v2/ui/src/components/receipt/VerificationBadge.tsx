import { Image, Shield, ShieldAlert, ShieldCheck } from 'lucide-react'
import { STATUS_TONE, verificationText } from '../../lib/receipt'
import type { ReceiptSummaryRow } from '../../types'

/**
 * A ticket's verification status on its card: a shield colored by status
 * (ShieldCheck once self-checked or better), an image icon when the receipt
 * has media, and an amber ShieldAlert when Require holds it for a person.
 */
export function VerificationBadge({ receipt }: { receipt: Pick<ReceiptSummaryRow, 'status' | 'held' | 'has_media'> }) {
  const text = verificationText(receipt)
  if (receipt.held) {
    return (
      <span data-testid="verification-badge" data-status="held" title={text}
        className="inline-flex items-center gap-1 font-label text-[10px] font-semibold uppercase tracking-[0.1em] text-warning-ink">
        <ShieldAlert size={12} aria-hidden="true" />Needs you
      </span>
    )
  }
  const Icon = receipt.status === 'unverified' ? Shield : ShieldCheck
  return (
    <span data-testid="verification-badge" data-status={receipt.status} title={text}
      className="inline-flex items-center gap-1" style={{ color: STATUS_TONE[receipt.status] }}>
      <Icon size={12} aria-hidden="true" />
      {receipt.status === 'human_accepted' && <span className="font-label text-[10px] font-semibold uppercase tracking-[0.1em]">Accepted</span>}
      {receipt.has_media && <Image size={12} aria-hidden="true" data-testid="verification-media" />}
    </span>
  )
}
