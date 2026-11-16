// The requester's own-list logic (AMP #3986, build slice J5).
//
// The page is rendered in RequesterRequests.test.tsx; this file proves the
// decisions the page delegates, because each of them is a rule that would
// otherwise be repeated inline and drift:
//
//  1. **The pills are a total, exclusive mapping.** Every lifecycle state is in
//     exactly one pill, so "All" is the union of the others and no request is
//     reachable only by clearing the filter. The mockup draws five pills and
//     four sample rows, leaving `new` and `triaging` in no pill at all — the
//     test that would have caught that is `pillStatesMissing`.
//  2. **The server's state word is the tag, never a local table.** A view whose
//     `state_word` is empty says so rather than falling back to the raw enum,
//     and the word is what the server sent even when it does not match a label
//     this file would have chosen.
//  3. **The reference keeps its case.** `public_ref` is base64url and the server
//     resolves it with an equality, so a display transform would render a
//     reference nobody can paste back.
//  4. **"Nothing yet" and "nothing matches" are different answers**, and the
//     total is the server's, never a count of the rows on screen.
import { describe, expect, it } from 'vitest'
import type { RequesterRequestView, RequestStatus } from '../types'
import { REQUEST_STATUSES } from '../types'
import {
  REQUESTER_LIST_PILLS, REQUESTER_PAGE_SIZE, REQUESTER_STATE_TONE, allPillCount, matchesPill,
  pillCountKeys, pillStatesCovered, pillStatesMissing, requesterDate, requesterEmpty, requesterMetaLine,
  requesterMetaTitle, requesterNoMatchesHeading, requesterPill, requesterPillFilter, requesterRowLine,
  requesterStateHint, requesterStateVar, requesterStateWord, requesterTone, requesterTotalLabel,
} from './requesterRequests'

/** A requester view with only what a test needs; the rest is the server's defaults. */
function view(over: Partial<RequesterRequestView> = {}): RequesterRequestView {
  return {
    public_ref: '8f3k2xq7',
    state: 'in_progress',
    state_word: 'In progress',
    subject: 'October invoice export totals don\u2019t match the ledger',
    input_brief: 'The export is short by about \u20ac4,300 across 11 rows.',
    team_name: 'Northwind support desk',
    created_at: '2026-11-04T14:00:00Z',
    milestones: [],
    comments: [],
    outputs: [],
    ...over,
  }
}

/** A fixed clock, so a relative sentence is asserted against one moment. */
const NOW = Date.parse('2026-11-14T16:00:00Z')

describe('the state tag', () => {
  it('renders the server’s word, not a table of its own', () => {
    // The server names the six content states from the mockup; the tag must be
    // that word even where this file would have chosen a different one.
    expect(requesterStateWord(view({ state: 'delivered', state_word: 'Delivered — your call' }))).toBe('Delivered — your call')
    expect(requesterStateWord(view({ state: 'rejected', state_word: 'Closed' }))).toBe('Closed')
    expect(requesterStateWord(view({ state: 'needs_information', state_word: 'Needs your reply' }))).toBe('Needs your reply')
  })

  it('says the status is unavailable rather than naming the raw enum', () => {
    // An empty word is a bug made visible. Falling back to `needs_information`
    // would put a snake_case enum in front of a customer.
    expect(requesterStateWord(view({ state: 'needs_information', state_word: '' }))).toBe('Status unavailable')
    expect(requesterStateWord(view({ state: 'needs_information', state_word: '   ' }))).toBe('Status unavailable')
  })

  it('has a tone and a hint for all nine states, so no state is an unstyled hole', () => {
    for (const state of REQUEST_STATUSES) {
      expect(REQUESTER_STATE_TONE[state], state).toBeTruthy()
      expect(requesterStateHint(state).length, state).toBeGreaterThan(10)
      expect(requesterStateVar(state), state).toMatch(/^var\(--/)
    }
    // The mockup's four drawn states, in its own colours.
    expect(requesterTone('needs_information')).toBe('flag')
    expect(requesterTone('delivered')).toBe('ok')
    expect(requesterTone('in_progress')).toBe('prog')
    expect(requesterTone('requester_accepted')).toBe('done')
  })

  it('gives an unknown state the neutral tone rather than a warning colour', () => {
    expect(requesterTone('something_else' as RequestStatus)).toBe('wait')
  })
})

describe('the row’s two lines', () => {
  it('says what a needs-reply state means, with when the desk asked', () => {
    const line = requesterRowLine(view({
      state: 'needs_information',
      state_word: 'Needs your reply',
      needs_reply_since: new Date(NOW - 2 * 3_600_000).toISOString(),
    }), NOW)
    expect(line).toBe('The desk asked you a question 2h ago — your reply moves this forward.')
  })

  it('falls back to a timeless sentence when the timestamp is missing', () => {
    // A sentence with a dangling " ago" is worse than no relative phrase.
    expect(requesterRowLine(view({ state: 'needs_information', needs_reply_since: undefined }), NOW))
      .toBe('The desk asked you a question — your reply moves this forward.')
    expect(requesterRowLine(view({ state: 'delivered', delivered_at: undefined }), NOW))
      .toMatch(/^Delivered — check the outcome/)
  })

  it('is a sentence for every state, and promises no deadline anywhere', () => {
    for (const state of REQUEST_STATUSES) {
      const line = requesterRowLine(view({ state, state_word: 'x' }), NOW)
      expect(line.length, state).toBeGreaterThan(10)
      // No SLA exists in AMP (needs_info_since is the only clock), so no line
      // may name one — and none may leak an internal noun.
      for (const bad of ['ticket', 'epic', 'story', 'agent', 'branch', 'merge', 'within 24', 'SLA']) {
        expect(line.toLowerCase(), `${state}: ${bad}`).not.toContain(bad.toLowerCase())
      }
    }
  })

  it('names the reference exactly as the server minted it, and the dates in short form', () => {
    // base64url is case-sensitive: the server resolves public_ref with `=`, so
    // a case-folded display is a reference nobody can paste back.
    const mixed = view({ public_ref: 'AbC-123_xYz', created_at: '2026-11-08T09:42:00Z', closed_at: '2026-11-21T16:00:00Z' })
    expect(requesterMetaLine(mixed)).toBe('REQ-AbC-123_xYz · opened Nov 8 · closed Nov 21')
    // Local rendering, so the fixture is mid-day UTC: a midnight-UTC timestamp
    // is the previous day in the Americas and the assertion would be about the
    // test machine's zone rather than about the formatting.
    expect(requesterDate('2026-01-02T12:00:00Z')).toMatch(/Jan 2/)
    expect(requesterDate(undefined)).toBe('')
    expect(requesterDate('not a date')).toBe('')
    expect(requesterMetaTitle(mixed)).toContain('REQ-AbC-123_xYz')
  })

  it('does not print "opened" with no date behind it', () => {
    expect(requesterMetaLine(view({ created_at: '' }))).toBe('REQ-8f3k2xq7')
  })
})

describe('the filter pills', () => {
  it('covers every lifecycle state exactly once, so no request is unreachable', () => {
    // The map's own test: the mockup's four sample rows leave `new` and
    // `triaging` in no pill at all, which is the bug this asserts against.
    expect(pillStatesMissing()).toEqual([])
    const covered = pillStatesCovered()
    expect(new Set(covered).size).toBe(covered.length)
    expect([...covered].sort()).toEqual([...REQUEST_STATUSES].sort())
    // And each state matches exactly one pill.
    for (const state of REQUEST_STATUSES) {
      const matching = REQUESTER_LIST_PILLS.filter(p => p.key !== 'all' && matchesPill(state, p.key))
      expect(matching.map(p => p.key), state).toHaveLength(1)
    }
  })

  it('keeps the mockup’s five labels, verbatim', () => {
    expect(REQUESTER_LIST_PILLS.map(p => p.label)).toEqual([
      'All', 'Needs your reply', 'In progress', 'Delivered', 'Closed',
    ])
  })

  it('turns a pill into the server query the own list accepts', () => {
    // A server filter, not a view of the loaded page: the route takes `state`.
    expect(requesterPillFilter('all')).toEqual({})
    expect(requesterPillFilter('needs_reply')).toEqual({ state: ['needs_information'] })
    expect(requesterPillFilter('closed')).toEqual({ state: ['requester_accepted', 'rejected', 'duplicate'] })
  })

  it('treats an unknown pill as All rather than hiding every row', () => {
    expect(requesterPill('nonsense' as never).key).toBe('all')
    expect(requesterPillFilter('nonsense' as never)).toEqual({})
  })

  it('counts All from the other four, and never shows a partial sum', () => {
    // `All` is every state and the four pills partition the nine, so its count
    // is their sum — no fifth read.
    expect(pillCountKeys()).toEqual(['needs_reply', 'in_progress', 'delivered', 'closed'])
    expect(allPillCount({})).toBeUndefined()
    expect(allPillCount({ needs_reply: 1, in_progress: 2, delivered: 1 })).toBeUndefined()
    expect(allPillCount({ needs_reply: 1, in_progress: 2, delivered: 1, closed: 1 })).toBe(5)
  })
})

describe('the list’s own words', () => {
  it('counts with the server’s total and names the pill', () => {
    expect(requesterTotalLabel(1, 'all')).toBe('1 request')
    expect(requesterTotalLabel(4, 'all')).toBe('4 requests')
    expect(requesterTotalLabel(1, 'delivered')).toBe('1 request in delivered')
    expect(requesterTotalLabel(0, 'needs_reply')).toBe('0 requests in needs your reply')
  })

  it('tells "nothing yet" apart from "nothing matches this filter"', () => {
    expect(requesterEmpty('all', 0)).toBe('none-yet')
    // A filtered pill with nothing in it is not an empty account.
    expect(requesterEmpty('delivered', 0)).toBe('no-matches')
    // Nor is "All" with rows behind a page — but any answer with rows never asks.
    expect(requesterEmpty('all', 3)).toBe('no-matches')
  })

  it('names the pill in the no-matches heading, and never renders "are all"', () => {
    expect(requesterNoMatchesHeading('delivered')).toBe('None of your requests are delivered right now.')
    expect(requesterNoMatchesHeading('needs_reply')).toBe('None of your requests are needs your reply right now.')
    // `All` with an empty page (rows deleted between the count and the page) is
    // not a filter to clear: the template would have said "are all right now".
    expect(requesterNoMatchesHeading('all')).toBe('None of your requests came back.')
  })

  it('asks for a bounded page', () => {
    expect(REQUESTER_PAGE_SIZE).toBeGreaterThan(0)
    expect(REQUESTER_PAGE_SIZE).toBeLessThanOrEqual(50)
  })
})