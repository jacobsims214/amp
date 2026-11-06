import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { EvidencePlayer } from './EvidencePlayer'
import type { ReceiptEvidence } from '../../types'

const ev = (over: Partial<ReceiptEvidence>): ReceiptEvidence => ({
  id: 1, receipt_id: 9, key: '2/5/attempts/9/abcdef012345/video.mp4', name: 'video.mp4', content_type: 'video/mp4',
  bytes: 2_500_000, status: 'stored', created_at: '2026-11-01T10:00:00Z', ...over,
})

describe('EvidencePlayer', () => {
  it('plays the video with its poster, and shows screenshots and downloads', () => {
    render(<EvidencePlayer evidence={[
      ev({ duration_ms: 3000 }),
      ev({ id: 2, name: 'poster.gif', key: '2/5/attempts/9/abcdef012345/poster.gif', content_type: 'image/gif', bytes: 4000 }),
      ev({ id: 3, name: 'screenshot-1.png', key: '2/5/attempts/9/abcdef012345/screenshot-1.png', content_type: 'image/png', bytes: 900 }),
      ev({ id: 4, name: 'trace.zip', key: '2/5/attempts/9/abcdef012345/trace.zip', content_type: 'application/zip', bytes: 100 }),
    ]} />)
    const video = screen.getByTestId('evidence-video')
    expect(video.tagName).toBe('VIDEO')
    expect(video).toHaveAttribute('src', '/api/receipts/2/5/attempts/9/abcdef012345/video.mp4')
    expect(video).toHaveAttribute('poster', '/api/receipts/2/5/attempts/9/abcdef012345/poster.gif')
    expect(video).toHaveAttribute('preload', 'metadata')
    expect(video).toHaveAttribute('controls')
    expect(screen.getByText(/3\.0 s/)).toBeInTheDocument()
    // The poster isn't repeated as a screenshot.
    const imgs = screen.getAllByRole('img')
    expect(imgs.map(i => i.getAttribute('alt'))).toEqual(['screenshot-1.png'])
    const downloads = screen.getByTestId('evidence-downloads')
    expect(downloads).toHaveTextContent('video.mp4')
    expect(downloads).toHaveTextContent('2.4 MB')
    expect(screen.getByRole('link', { name: 'trace.zip' })).toHaveAttribute('download', 'trace.zip')
  })

  it('says when evidence expired, and what is still uploading', () => {
    render(<EvidencePlayer evidence={[
      ev({ status: 'expired', expires_at: '2027-01-30T10:00:00Z' }),
      ev({ id: 2, name: 'screenshot-2.png', content_type: 'image/png', status: 'pending', key: 'k/screenshot-2.png' }),
    ]} />)
    expect(screen.getByTestId('evidence-expired')).toHaveTextContent(/Evidence expired on .*2027: video\.mp4/)
    expect(screen.getByTestId('evidence-pending')).toHaveTextContent('Not uploaded: screenshot-2.png')
    expect(screen.queryByTestId('evidence-video')).toBeNull()
    expect(screen.queryByRole('img')).toBeNull()
    expect(screen.queryByTestId('evidence-downloads')).toBeNull()
  })

  it('shows a lone poster as an image, and nothing for no evidence', () => {
    const { container, rerender } = render(<EvidencePlayer evidence={[
      ev({ name: 'poster.gif', key: 'k/poster.gif', content_type: 'image/gif' }),
    ]} />)
    expect(screen.getByRole('img')).toHaveAttribute('src', '/api/receipts/k/poster.gif')
    rerender(<EvidencePlayer evidence={[]} />)
    expect(container).toBeEmptyDOMElement()
  })
})
