// The requester upload helpers (ticket AMP #3981, build slice J4): the bounds,
// the words and the one place bytes are read.
//
// The three things asserted here are the ticket's acceptance criteria at the
// level they can be asserted without a DOM:
//
//  1. **The bounds are applied before a round trip, and refused rows never
//     count.** 5 files, 25 MiB each, 60 MiB total — the domain's numbers
//     (v2/amp-api/internal/domain/request_attachment.go). The order the three
//     checks run in is asserted too, because "too many" is the true reason for a
//     sixth file whatever its size.
//  2. **`unsupported` is never called clean.** AMP ships no scanner in v1, so
//     every upload lands on `unsupported`; the wording may not say "scanned",
//     "clean" or "checked" about it.
//  3. **Bytes are read locally.** `fileToUploadInput` reads a File with a
//     FileReader and produces a name and base64 — no URL, no location.
import { describe, expect, it } from 'vitest'
import {
  REQUEST_UPLOAD_LIMITS, abortableUpload, fileNameForDisplay, fileToUploadInput, formatBytes,
  pickRefusalMessage, planPicks, scanWording,
} from './requesterUpload'
import type { RequestAttachmentFacts, RequestUploadAnswer } from '../types'

/** A File of `bytes` bytes, cheaply: a Blob's size is its content's length. */
const file = (name: string, bytes: number): File => {
  const f = new File(['x'], name)
  Object.defineProperty(f, 'size', { value: bytes })
  return f
}

describe('bounds are the server’s own numbers', () => {
  it('mirrors the domain constants (25 MiB / 5 / 60 MiB / 90 days)', () => {
    expect(REQUEST_UPLOAD_LIMITS).toEqual({
      max_file_bytes: 25 * 1024 * 1024,
      max_files: 5,
      max_total_bytes: 60 * 1024 * 1024,
      retention_days: 90,
    })
  })
})

describe('planPicks: the bounds apply before a round trip', () => {
  it('accepts files within every bound', () => {
    const plan = planPicks([], [file('a.csv', 182 * 1024), file('b.png', 1.2 * 1024 * 1024)])
    expect(plan.accepted.map(f => f.name)).toEqual(['a.csv', 'b.png'])
    expect(plan.refused).toEqual([])
  })

  it('refuses a sixth file by count, and names the count', () => {
    const kept = [1, 2, 3, 4, 5].map(i => file(`k${i}.csv`, 10))
    const plan = planPicks(kept, [file('sixth.csv', 10)])
    expect(plan.accepted).toEqual([])
    expect(plan.refused[0].reason).toBe('too_many')
    expect(pickRefusalMessage('too_many')).toBe('5 files max per request')
  })

  it('refuses a file over 25 MiB by size, and names the size', () => {
    const plan = planPicks([], [file('full-database-dump.zip', 96 * 1024 * 1024)])
    expect(plan.refused[0].reason).toBe('too_large')
    expect(pickRefusalMessage('too_large')).toBe('Too large — 25 MB max')
  })

  it('refuses what would push the request over 60 MiB in total', () => {
    // Three 20 MiB files are 60 MiB; a fourth 1 MB file is over the total but
    // under both other bounds, so this can only be the total check.
    const kept = [1, 2, 3].map(i => file(`k${i}.bin`, 20 * 1024 * 1024))
    const plan = planPicks(kept, [file('one-more.bin', 1024 * 1024)])
    expect(plan.refused[0].reason).toBe('total_too_large')
    expect(pickRefusalMessage('total_too_large')).toBe('60 MB total max')
  })

  it('checks the count before the size, so a sixth huge file says "too many"', () => {
    const kept = [1, 2, 3, 4, 5].map(i => file(`k${i}.csv`, 10))
    expect(planPicks(kept, [file('huge.zip', 96 * 1024 * 1024)]).refused[0].reason).toBe('too_many')
  })

  it('refuses an empty file rather than uploading zero bytes', () => {
    expect(planPicks([], [file('empty.log', 0)]).refused[0].reason).toBe('empty')
  })

  it('counts only the files already sent, not ones that failed or were refused', () => {
    // A refused row must not consume a slot: the person would be told to remove
    // a file that was never attached.
    const kept = [1, 2, 3, 4].map(i => file(`k${i}.csv`, 10))
    const plan = planPicks(kept, [file('fifth.csv', 10), file('sixth.csv', 10)])
    expect(plan.accepted.map(f => f.name)).toEqual(['fifth.csv'])
    expect(plan.refused.map(r => r.reason)).toEqual(['too_many'])
  })

  it('keeps the running total, so one batch cannot slip past 60 MiB together', () => {
    const plan = planPicks([], [file('a.bin', 25 * 1024 * 1024), file('b.bin', 25 * 1024 * 1024), file('c.bin', 25 * 1024 * 1024)])
    expect(plan.accepted.map(f => f.name)).toEqual(['a.bin', 'b.bin'])
    expect(plan.refused.map(r => r.reason)).toEqual(['total_too_large'])
  })
})

describe('scanWording: an unscanned file is never called clean', () => {
  const words = (s: Parameters<typeof scanWording>[0], held = false) => {
    const w = scanWording(s, held)
    return `${w.label} ${w.hint}`.toLowerCase()
  }

  it('says "Not scanned" for unsupported, and never claims a scan', () => {
    expect(scanWording('unsupported', false).label).toBe('Not scanned')
    for (const claim of ['clean', 'scanned and', 'checked', 'no threats', 'safe']) {
      expect(words('unsupported')).not.toContain(claim)
    }
    expect(words('unsupported')).toContain('unscanned')
  })

  it('only clean may claim a scan happened', () => {
    expect(scanWording('clean', false).label).toBe('Scanned')
    expect(scanWording('clean', false).hint).toContain('nothing was found')
  })

  it('reports pending as scanning and infected as blocked', () => {
    expect(scanWording('pending', false).label).toBe('Scanning')
    expect(scanWording('infected', false).label).toBe('Blocked')
  })

  it('reports the hold first, because a lead must release it', () => {
    const held = scanWording('unsupported', true)
    expect(held.label).toBe('Held for review')
    expect(held.hint).toContain('team lead')
    // A held file that is also unscanned says both facts, not just one.
    expect(held.hint).toContain('unscanned')
  })

  it('gives every state a tone, so colour is never the only signal', () => {
    for (const s of ['pending', 'clean', 'infected', 'unsupported'] as const) {
      expect(['ok', 'wait', 'flag']).toContain(scanWording(s, false).tone)
      expect(scanWording(s, false).label.length).toBeGreaterThan(0)
    }
  })
})

describe('formatBytes', () => {
  it('uses the mockup’s vocabulary', () => {
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(182 * 1024)).toBe('182 KB')
    expect(formatBytes(1.2 * 1024 * 1024)).toBe('1.2 MB')
    expect(formatBytes(25 * 1024 * 1024)).toBe('25 MB')
    expect(formatBytes(96 * 1024 * 1024)).toBe('96 MB')
    expect(formatBytes(60 * 1024 * 1024)).toBe('60 MB')
  })

  it('survives a nonsense size without rendering NaN', () => {
    expect(formatBytes(Number.NaN)).toBe('0 B')
    expect(formatBytes(-5)).toBe('0 B')
  })
})

describe('fileNameForDisplay', () => {
  it('leaves a short name alone', () => {
    expect(fileNameForDisplay('october-export-totals.csv')).toBe('october-export-totals.csv')
  })

  it('truncates a long name but keeps its extension', () => {
    const long = `${'a'.repeat(90)}.png`
    const shown = fileNameForDisplay(long, 40)
    expect(shown.length).toBeLessThanOrEqual(40)
    expect(shown.endsWith('.png')).toBe(true)
    expect(shown).toContain('…')
  })
})

describe('fileToUploadInput reads the bytes locally', () => {
  it('produces a name and base64, with no location and no browser-claimed type', async () => {
    const f = new File(['hello amp'], 'note.txt', { type: 'text/plain' })
    const input = await fileToUploadInput(f)
    expect(input.file_name).toBe('note.txt')
    expect(Object.keys(input).sort()).toEqual(['content_base64', 'file_name'])
    expect(Buffer.from(input.content_base64, 'base64').toString()).toBe('hello amp')
    // No data-URL prefix: the stored type is what AMP sniffs, never the
    // browser's guess that a data URL carries.
    expect(input.content_base64.startsWith('data:')).toBe(false)
    for (const forbidden of ['url', 'path', 'location', 'href']) expect(input).not.toHaveProperty(forbidden)
  })
})

describe('abortableUpload settles on cancel', () => {
  const answer = (name: string): RequestUploadAnswer => ({
    attachment: { file_name: name } as RequestAttachmentFacts,
    held: false,
    next: 'stored',
  })

  it('passes the name and base64 through and resolves the answer', async () => {
    const seen: unknown[] = []
    const upload = abortableUpload(async input => { seen.push(input); return answer(input.file_name) })
    const got = await upload(new File(['abc'], 'a.csv'), new AbortController().signal)
    expect(got.attachment.file_name).toBe('a.csv')
    expect(seen).toEqual([{ file_name: 'a.csv', content_base64: Buffer.from('abc').toString('base64') }])
  })

  it('rejects as an abort, so the picker cancels instead of failing', async () => {
    const controller = new AbortController()
    let finish: (a: RequestUploadAnswer) => void = () => {}
    const upload = abortableUpload(() => new Promise<RequestUploadAnswer>(resolve => { finish = resolve }))
    const pending = upload(new File(['abc'], 'a.csv'), controller.signal)
    controller.abort()
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' })
    // A late answer from the still-running request settles nothing.
    finish(answer('a.csv'))
  })

  it('does not start an upload that was already cancelled', async () => {
    const controller = new AbortController()
    controller.abort()
    const upload = abortableUpload(async () => { throw new Error('should not be called') })
    await expect(upload(new File(['abc'], 'a.csv'), controller.signal)).rejects.toMatchObject({ name: 'AbortError' })
  })
})
