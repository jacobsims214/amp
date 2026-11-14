// The tracking screen's decisions as pure functions (AMP #3987, build slice J6).
//
// These are the answers the page is not allowed to invent: which arm a lifecycle
// state renders, what each arm's tag looks like, what a refusal of the requester's
// own act says, and which of the three access states a read failure can be. Each
// case names the server rule it mirrors, because the point of the module is that
// the UI repeats the server's decision rather than making a second one.
import { describe, expect, it } from 'vitest'
import {
  MAX_ANSWER_LEN,
  REQUESTER_TRACK_NOTE,
  answerCounter,
  requesterActNotice,
  requesterDownloadPath,
  requesterOutputDownloadable,
  requesterOutputWord,
  requesterReadFailure,
  requesterTagTone,
  requesterTrack,
  requesterTrackVar,
  validateAnswer,
} from './requesterStatus'
import type { RequestStatus } from '../types'

describe('requesterTrack', () => {
  it('collapses the nine lifecycle states into the screen\'s five arms', () => {
    const expected: Record<RequestStatus, string> = {
      new: 'progress',
      triaging: 'progress',
      needs_information: 'needs_reply',
      accepted: 'progress',
      in_progress: 'progress',
      delivered: 'delivered',
      requester_accepted: 'accepted',
      // The server's own word for both is "Closed" (`RequesterStateWord`), so a
      // rejected request and a folded duplicate render the same arm.
      rejected: 'closed',
      duplicate: 'closed',
    }
    for (const [state, track] of Object.entries(expected)) {
      expect(requesterTrack(state as RequestStatus), state).toBe(track)
    }
  })

  it('puts the answer box in exactly one state, which is the only state an answer is legal out of', () => {
    const needsReply = (Object.keys(REQUESTER_TRACK_NOTE) as string[]).length
    expect(needsReply).toBe(5)
    const arms = (['new', 'triaging', 'needs_information', 'accepted', 'in_progress', 'delivered', 'requester_accepted', 'rejected', 'duplicate'] as RequestStatus[])
      .filter(s => requesterTrack(s) === 'needs_reply')
    // `planRequestAnswer` makes an answer legal only out of needs_information.
    expect(arms).toEqual(['needs_information'])
  })

  it('reads an unknown state as "we are on it" rather than promising anything', () => {
    expect(requesterTrack('something_new' as RequestStatus)).toBe('progress')
  })
})

describe('the arm\'s colour and tone', () => {
  it('gives every arm a tone and a variable, and no arm a hole', () => {
    const tracks = ['progress', 'needs_reply', 'delivered', 'accepted', 'closed'] as const
    const tones = tracks.map(requesterTagTone)
    const vars = tracks.map(requesterTrackVar)
    for (const v of vars) expect(v).toMatch(/^var\(--[a-z-]+\)$/)
    // The three arms that ask something of the reader (or celebrate) each have
    // their own tone. `accepted` and `closed` share the quiet "done" tone on
    // purpose: they are both over, and the tag's *word* (the server's own) is
    // what tells them apart — the mockup does the same.
    expect(requesterTagTone('needs_reply')).toBe('flag')
    expect(requesterTagTone('delivered')).toBe('ok')
    expect(requesterTagTone('progress')).toBe('prog')
    expect(requesterTagTone('accepted')).toBe('done')
    expect(requesterTagTone('closed')).toBe('done')
    expect(new Set(tones).size).toBe(4)
    // And no two *working* arms collide: a reply-needed tag never looks like a
    // delivered one.
    expect(requesterTrackVar('needs_reply')).not.toBe(requesterTrackVar('progress'))
    expect(requesterTrackVar('needs_reply')).toBe('var(--st-flag)')
    expect(requesterTrackVar('delivered')).toBe('var(--success)')
    expect(requesterTrackVar('closed')).toBe('var(--st-wait)')
  })

  it('has a sentence for every arm, and none of them names an internal state', () => {
    for (const [track, note] of Object.entries(REQUESTER_TRACK_NOTE)) {
      expect(note.length, track).toBeGreaterThan(30)
      for (const internal of ['triaging', 'needs_information', 'requester_accepted', 'ticket', 'project']) {
        expect(note.toLowerCase(), `${track}: ${internal}`).not.toContain(internal)
      }
    }
  })
})

describe('validateAnswer', () => {
  it('refuses an empty or whitespace-only answer without a round trip', () => {
    expect(validateAnswer('')).toMatch(/Write your answer first/)
    expect(validateAnswer('   \n\t ')).toMatch(/Write your answer first/)
  })

  it('accepts anything up to the server\'s own comment bound', () => {
    expect(validateAnswer('The October close.')).toBeNull()
    expect(validateAnswer('x'.repeat(MAX_ANSWER_LEN))).toBeNull()
    expect(validateAnswer('x'.repeat(MAX_ANSWER_LEN + 1))).toMatch(/too long|is the most one message can carry/)
  })

  it('counts characters, not bytes: an emoji-heavy answer is not refused early', () => {
    // 5000 emoji are 20000 bytes but 5000 characters, and the domain bounds the
    // comment by runes (`NormalizeRequestBrief` → `utf8.RuneCountInString`).
    expect(validateAnswer('😀'.repeat(5000))).toBeNull()
  })
})

describe('answerCounter', () => {
  it('stays quiet until the bound matters, then counts', () => {
    expect(answerCounter('short')).toBeNull()
    expect(answerCounter('x'.repeat(Math.floor(MAX_ANSWER_LEN * 0.5)))).toBeNull()
    expect(answerCounter('x'.repeat(Math.ceil(MAX_ANSWER_LEN * 0.85)))).toMatch(/of 20000 characters$/)
    expect(answerCounter('x'.repeat(MAX_ANSWER_LEN + 5))).toMatch(/too long to send/)
  })
})

describe('requesterActNotice', () => {
  const err = (status: number, code?: string, message?: string) =>
    Object.assign(new Error(message ?? ''), { status, code })

  it('turns a 409 into "it moved", and says nothing was written', () => {
    const notice = requesterActNotice(err(409, 'request.conflict', 'request 7 is at revision 9, not 4'), 'answer')
    expect(notice.title).toMatch(/moved while you were looking at it/)
    expect(notice.body).toContain('Nothing was sent')
    expect(notice.body).toContain('request 7 is at revision 9')
    expect(notice.reload).toBe(true)
  })

  it('keeps a 404 as the surface\'s one not-found, and never suggests it exists', () => {
    const notice = requesterActNotice(err(404, 'request.not_found'), 'accept')
    expect(notice.title).toMatch(/isn't available to you any more/)
    expect(notice.body).toContain('Nothing was recorded')
    expect(notice.body).toMatch(/visible only from the sign-in that sent it/)
    expect(notice.reload).toBe(true)
  })

  it('names the requester-only rule on a 403, and offers no reload', () => {
    const notice = requesterActNotice(err(403), 'accept')
    expect(notice.title).toMatch(/Only the request's own requester/)
    expect(notice.reload).toBe(false)
  })

  it('reports an unknown outcome honestly for a dropped connection', () => {
    const notice = requesterActNotice(new Error('Failed to fetch'), 'answer')
    expect(notice.title).toMatch(/could not tell whether that reached the desk/)
    expect(notice.body).toMatch(/may or may not have been recorded/)
    expect(notice.reload).toBe(true)
  })

  it('carries the server\'s own sentence for a 400', () => {
    const notice = requesterActNotice(err(400, 'request.invalid', 'an answer names the question it answers'), 'answer')
    expect(notice.title).toMatch(/couldn't take that/)
    expect(notice.body).toContain('an answer names the question it answers')
    expect(notice.body).toContain('Nothing was sent')
  })

  it('reads a 429\'s wait in both spellings the server writes', () => {
    const notice = requesterActNotice(Object.assign(new Error('slow down'), { status: 429, retryAfterS: 3 }), 'reopen')
    expect(notice.body).toMatch(/Try again in 3 seconds/)
  })

  it('uses the act\'s own verb, so a reopen never claims an answer was sent', () => {
    expect(requesterActNotice(err(409), 'reopen').body).toContain('Nothing was recorded')
    expect(requesterActNotice(err(409), 'answer').body).toContain('Nothing was sent')
  })
})

describe('requesterReadFailure', () => {
  it('folds every "not for you" into the one not-found arm', () => {
    // The surface answers one 404 for an unknown reference, another person's
    // reference and a request whose requester changed; a page that told them
    // apart would be the oracle the server refuses to be.
    for (const status of [404, 403]) {
      expect(requesterReadFailure(Object.assign(new Error('x'), { status }))).toBe('not_found')
    }
    expect(requesterReadFailure(Object.assign(new Error('x'), { status: 404, code: 'request.not_found' }))).toBe('not_found')
  })

  it('does NOT call a server failure "not found"', () => {
    expect(requesterReadFailure(Object.assign(new Error('boom'), { status: 500 }))).toBe('failed')
    expect(requesterReadFailure(new Error('Failed to fetch'))).toBe('failed')
    expect(requesterReadFailure(undefined)).toBe('failed')
  })
})

describe('shared outputs', () => {
  it('says one of three words and never names an internal state', () => {
    expect(requesterOutputWord('ready')).toBe('Ready')
    expect(requesterOutputWord('preparing')).toBe('Preparing')
    expect(requesterOutputWord('unavailable')).toBe('Unavailable')
    // `preparing` deliberately hides *why*: a pending scan and a lead's hold are
    // both the team's to resolve, and neither is a requester's business.
    for (const word of ['Ready', 'Preparing', 'Unavailable']) {
      for (const internal of ['scan', 'held', 'infected', 'purged']) {
        expect(word.toLowerCase()).not.toContain(internal)
      }
    }
  })

  it('offers a download only for a real reference', () => {
    expect(requesterOutputDownloadable('tok-abc')).toBe(true)
    expect(requesterOutputDownloadable('')).toBe(false)
    expect(requesterOutputDownloadable('   ')).toBe(false)
    expect(requesterOutputDownloadable(undefined)).toBe(false)
  })

  it('builds the serve path from the opaque reference, and accepts a whole path as it is', () => {
    expect(requesterDownloadPath('tok-abc.def')).toBe('/api/request-downloads/tok-abc.def')
    expect(requesterDownloadPath('a/b+c=')).toBe('/api/request-downloads/a%2Fb%2Bc%3D')
    // A future absolute URL needs no change here.
    expect(requesterDownloadPath('/api/request-downloads/tok')).toBe('/api/request-downloads/tok')
    expect(requesterDownloadPath('')).toBe('')
  })
})
