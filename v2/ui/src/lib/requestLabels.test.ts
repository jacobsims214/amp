// The words the request panels use (AMP #3979).
//
// These are small functions with one property each, and every one of them is a
// place a panel could quietly lie about a server decision — "clean" for an
// unscanned file, an invented refusal reason, a state word for a state the
// vocabulary does not have, an event sentence that claims a delivery. So each is
// pinned here, apart from the components that read them.
import { describe, expect, it } from 'vitest'
import {
  REQUEST_ACTION_LABEL,
  REQUEST_COMMENT_LABEL,
  REQUEST_COMMENT_SHARED,
  REQUEST_STATE_WORD,
  requestEditSentence,
  requestEventDetail,
  requestEventSentence,
  requestFileSize,
  requestFileType,
  requestLinkSentence,
  requestRefusalText,
  requestScanText,
  requestTransitionSentence,
} from './requestLabels'
import { REQUEST_COMMENT_KINDS, REQUEST_STATUSES } from '../types'
import { REQUEST_STATE_LABEL } from './requestState'

describe('the vocabularies are total', () => {
  it('has a word for all nine states and all four comment kinds', () => {
    for (const s of REQUEST_STATUSES) expect(REQUEST_STATE_WORD[s]).toBeTruthy()
    for (const k of REQUEST_COMMENT_KINDS) expect(REQUEST_COMMENT_LABEL[k]).toBeTruthy()
  })

  it('takes the state words from the queue\'s one table rather than copying them', () => {
    // The history line and the queue pill describe the same row: if these ever
    // diverge, one of them is wrong and the other is stale.
    expect(REQUEST_STATE_WORD).toBe(REQUEST_STATE_LABEL)
  })

  it('marks exactly the two directed kinds as shared with the requester', () => {
    expect(REQUEST_COMMENT_SHARED).toEqual({ comment: false, question: true, answer: true, system: false })
  })

  it('has a verb for every history action', () => {
    for (const a of Object.keys(REQUEST_ACTION_LABEL)) expect(REQUEST_ACTION_LABEL[a as keyof typeof REQUEST_ACTION_LABEL]).toBeTruthy()
  })
})

describe('scan verdicts', () => {
  it('never calls an unscanned file clean', () => {
    const unsupported = requestScanText('unsupported')
    expect(unsupported.label).toBe('Not scanned')
    expect(unsupported.title).toMatch(/never scanned/)
    expect(unsupported.bad).toBe(false)
  })

  it('calls a completed scan "Scanned" and a positive one "Blocked"', () => {
    expect(requestScanText('clean').label).toBe('Scanned')
    expect(requestScanText('infected')).toMatchObject({ label: 'Blocked', bad: true })
    expect(requestScanText('pending').label).toBe('Scanning')
  })
})

describe('refusals', () => {
  it('says nothing for the served case and something for each refusal', () => {
    expect(requestRefusalText('')).toBe('')
    expect(requestRefusalText(undefined)).toBe('')
    for (const r of ['scan_pending', 'scan_infected', 'held_for_release', 'purged'] as const) {
      expect(requestRefusalText(r)).not.toBe('')
    }
  })

  it('ignores a word it does not know rather than inventing a reason', () => {
    // A refusal the server adds later must not be described as a scan, a hold or
    // a purge: the panel says "not available", which is true of all of them.
    expect(requestRefusalText('quarantined_by_hand' as never)).toBe('This file is not available right now.')
  })
})

describe('file display', () => {
  it('sizes in the mockups\' units', () => {
    expect(requestFileSize(0)).toBe('0 B')
    expect(requestFileSize(41 * 1024)).toBe('41 KB')
    expect(requestFileSize(2.1 * 1048576)).toBe('2.1 MB')
    expect(requestFileSize(-1)).toBe('0 B')
  })

  it('names the sniffed type briefly, and does not guess an unknown one', () => {
    expect(requestFileType('text/xml')).toBe('XML')
    expect(requestFileType('application/pdf')).toBe('PDF')
    expect(requestFileType('image/png')).toBe('PNG')
    expect(requestFileType('application/x-msdownload')).toBe('EXE')
    expect(requestFileType('application/vnd.openxmlformats-officedocument.wordprocessingml.document')).toBe('OFFICE')
    expect(requestFileType('application/octet-stream')).toBe('file')
    expect(requestFileType('')).toBe('file')
    expect(requestFileType('application/x-thing-we-never-saw')).toBe('THING-WE-NEVER-SAW')
    expect(requestFileType('application/vnd.example.report')).toBe('EXAMPLE')
  })
})

describe('history sentences', () => {
  it('names the destination of a transition in words', () => {
    expect(requestTransitionSentence('triaging', 'needs_information')).toBe('asked the requester a question')
    expect(requestTransitionSentence('in_progress', 'delivered')).toBe('delivered the outcome')
    expect(requestTransitionSentence('duplicate', 'triaging')).toBe('reopened it out of Duplicate')
    // An unnamed destination falls back to the two state words rather than guessing.
    expect(requestTransitionSentence('accepted', 'mystery' as never)).toBe('moved the request')
  })

  it('lists the fields an edit changed, and stays generic without them', () => {
    expect(requestEditSentence(['subject'])).toBe('edited the subject')
    expect(requestEditSentence(['subject', 'input_brief'])).toBe('edited the subject and the brief')
    expect(requestEditSentence(['unknown_field'])).toBe('edited the request')
    expect(requestEditSentence(undefined)).toBe('edited the request')
  })

  it('reads a link row\'s own kinds and ids', () => {
    expect(requestLinkSentence({ link_kind: 'planned_as', target_kind: 'task', target_id: 3711 }, false))
      .toBe('linked a planned ticket #3711')
    expect(requestLinkSentence({ link_kind: 'delivered_by', target_kind: 'artifact', target_id: 41 }, false))
      .toBe('recorded a delivered artifact #41')
    expect(requestLinkSentence({ link_kind: 'duplicate_of' }, true)).toBe('detached it from its canonical request')
    expect(requestLinkSentence({ link_kind: 'routes_to' }, false)).toBe('routed it to a project')
    // An unknown kind still names something true.
    expect(requestLinkSentence({}, false)).toBe("linked a work")
  })

  it('parses a detail once and refuses to render a malformed one', () => {
    expect(requestEventDetail('{"link_id":1}')).toEqual({ link_id: 1 })
    expect(requestEventDetail('{"a":1')).toEqual({})
    expect(requestEventDetail('[1,2]')).toEqual({})
    expect(requestEventDetail('')).toEqual({})
    expect(requestEventDetail(undefined)).toEqual({})
  })

  it('builds a whole sentence from an event without adding a fact', () => {
    expect(requestEventSentence({ action: 'claim', from_status: undefined, to_status: undefined, detail: '{"claim_user_id":11}' }))
      .toBe('claimed the request')
    expect(requestEventSentence({ action: 'transition', from_status: 'needs_information', to_status: 'in_progress' }))
      .toBe('started work')
    // A claim's detail is never rendered into the sentence: ids are the row's, and
    // resolving one to a name is a lookup this module does not do.
    expect(requestEventSentence({ action: 'assign', detail: '{"previous_owner_user_id":11}' })).toBe('changed the assignee')
  })
})
