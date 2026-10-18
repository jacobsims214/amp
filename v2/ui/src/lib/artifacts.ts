import type { ArtifactKind, ArtifactStatus } from '../types'

export const STATUS_LABEL: Record<ArtifactStatus, string> = {
  in_review: 'In review',
  approved: 'Approved',
  changes_requested: 'Changes requested',
}

export const KIND_LABEL: Record<ArtifactKind, string> = {
  html: 'Page', svg: 'Drawing', image: 'Image', markdown: 'Document', pdf: 'PDF', text: 'Text',
}

/** "in 3 days", "tomorrow", "today" — when an artifact expires. */
export function expiresIn(iso: string | null): string {
  if (!iso) return ''
  const days = Math.ceil((new Date(iso).getTime() - Date.now()) / 86_400_000)
  if (days <= 0) return 'today'
  if (days === 1) return 'tomorrow'
  return `in ${days} days`
}
