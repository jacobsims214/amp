// needsLine: what a My work item needs from the person, in one line, for
// every kind (the project tab's cards and the summary rows share it).
import { describe, expect, it } from 'vitest'
import { firstSentence, needsLine, reasonVerb } from './humanActions'
import type { Task, WaitItem } from '../types'

const item = (over: Partial<WaitItem> & { task?: Partial<Task> }): WaitItem & { task?: Task } => ({
  kind: 'do', key: 'task:1', project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2',
  task_id: 1, task_ids: [1], name: 'A ticket', since: new Date().toISOString(), ...over,
  task: over.task as Task | undefined,
})

describe('needsLine', () => {
  it('approve: how many tickets the plan asks you to approve', () => {
    expect(needsLine(item({ kind: 'approve', task_id: undefined, task_ids: [1, 2, 3], count: 3 }))).toBe('Review and approve 3 tickets')
    expect(needsLine(item({ kind: 'approve', task_id: undefined, task_ids: [9] }))).toBe('Review and approve 1 ticket')
  })

  it('answer: the first sentence of the question', () => {
    expect(needsLine(item({ kind: 'answer', question: 'Fall back to a 0600 file? Or fail with a clear error.' })))
      .toBe('Answer: Fall back to a 0600 file?')
    expect(needsLine(item({ kind: 'answer', task: { block_reason: 'Which snapshot\nmore detail' } }))).toBe('Answer: Which snapshot')
    expect(needsLine(item({ kind: 'answer' }))).toBe('Answer: open the ticket for the question')
  })

  it('merge: the PR and how many tickets wait on it', () => {
    expect(needsLine(item({ kind: 'merge', pr_url: 'https://github.com/o/r/pull/88', holding_up: [4, 5] }))).toBe('Merge PR #88, 2 tickets wait')
    expect(needsLine(item({ kind: 'merge', pr_url: 'https://github.com/o/r/pull/88', holding_up: [4] }))).toBe('Merge PR #88, 1 ticket waits')
    expect(needsLine(item({ kind: 'merge', branch: 'feat/x' }))).toBe('Merge branch feat/x')
    expect(needsLine(item({ kind: 'merge' }))).toBe('Merge its work')
    expect(needsLine(item({ kind: 'merge', merge_state: 'abandoned', pr_url: 'https://github.com/o/r/pull/9' })))
      .toBe('PR #9 closed without merging: reopen the ticket or mark it not needed')
  })

  it('start: launch the run or return the ticket', () => {
    expect(needsLine(item({ kind: 'start', task_id: 2803, task_ids: [2803] }))).toBe('Launch its run, or return #2803 to ready')
  })

  it('do: the reason verb and the first sentence of the brief', () => {
    expect(needsLine(item({ human_reason: 'judgment', task: { description: 'Pick the Q1 targets. Options are in the doc.' } })))
      .toBe('Decide: Pick the Q1 targets.')
    expect(needsLine(item({ task: { human_reason: 'approval', description: '## Sign the vendor contract\nDetails…' } })))
      .toBe('Sign off: Sign the vendor contract')
  })

  it('do: no description says so', () => {
    expect(needsLine(item({ human_reason: 'judgment', task: { description: '' } }))).toBe('No instructions: open the ticket')
    expect(needsLine(item({ human_reason: 'judgment', task: { description: '  \n ' } }))).toBe('No instructions: open the ticket')
    expect(needsLine(item({ human_reason: 'judgment', task: { description: '', acceptance_criteria: 'Three targets recorded' } })))
      .toBe('Decide: see the acceptance criteria in the brief')
  })

  it('do without the ticket (summary rows) points at the ticket for the brief', () => {
    expect(needsLine(item({ human_reason: 'access' }))).toBe('Do with your access: open the ticket for the brief')
  })
})

describe('reasonVerb and firstSentence', () => {
  it('maps every reason to a verb, and anything else to Do', () => {
    expect(['judgment', 'approval', 'access', 'review', 'external', 'physical', 'other', undefined, 'nope'].map(reasonVerb))
      .toEqual(['Decide', 'Sign off', 'Do with your access', 'Review', 'Get from someone outside', 'Do offline', 'Do', 'Do', 'Do'])
  })

  it('keeps decimals and versions inside a sentence, and caps long lines', () => {
    expect(firstSentence('Bump to v1.2.3 now. Then tag.')).toBe('Bump to v1.2.3 now.')
    expect(firstSentence('- Call the vendor')).toBe('Call the vendor')
    const long = firstSentence('x'.repeat(300))
    expect(long).toHaveLength(140)
    expect(long.endsWith('…')).toBe(true)
  })
})
