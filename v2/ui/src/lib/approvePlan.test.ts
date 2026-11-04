import { describe, expect, it } from 'vitest'
import { afterChips, buildModel, chipIds, defaultTicks, includeEffect, selectionEffect, computeSteps, criticalPath } from './approvePlan'
import { lineDiff, changedLines } from './textDiff'
import { suggestAnswers } from './answers'
import { actionKindOf, mergeStatusText } from './humanActions'
import { busyPlan, ticket } from '../test/planFixture'

describe('Approve view model', () => {
  it('numbers steps from dependencies inside the set and finds the critical path', () => {
    const plan = busyPlan()
    const step = computeSteps(plan.tickets)
    expect([1, 2, 3, 4, 5].map(id => step.get(id))).toEqual([1, 2, 1, 3, 4])
    expect([...criticalPath(plan.tickets, step)].sort()).toEqual([1, 2, 4, 5])
  })

  it('ignores dependencies outside the set and survives a cycle', () => {
    const step = computeSteps([ticket(1, { dependency_ids: [99] }), ticket(2, { dependency_ids: [3] }), ticket(3, { dependency_ids: [2] })])
    expect(step.get(1)).toBe(1)
    expect(step.get(2)).toBeGreaterThan(0)
  })

  it('orders stories by first step and tickets by step, then id', () => {
    const m = buildModel(busyPlan())
    expect(m.epics[0].stories.map(s => s.story.id)).toEqual([413, 414])
    expect(m.order).toEqual([1, 3, 2, 4, 5])
    expect(m.epics[0].stories[1]).toMatchObject({ firstStep: 3, lastStep: 4 })
  })

  it('ticks your own awaiting tickets by default', () => {
    const plan = busyPlan()
    plan.tickets[2] = { ...plan.tickets[2], yours: false, owner_name: 'Bob' }
    expect([...defaultTicks(plan)].sort()).toEqual([1, 2, 4, 5])
  })

  it('holds everything downstream of a left-out ticket and says why', () => {
    const plan = busyPlan()
    const m = buildModel(plan)
    const ticks = new Set([1, 3, 4, 5]) // 2 left out
    const e = selectionEffect(m, ticks, false)
    expect([...e.leftOut]).toEqual([2])
    expect(Object.fromEntries(e.held)).toEqual({ 4: [2], 5: [2] })
    expect(e.startsNow).toEqual([1, 3])
    expect(e.yourSteps).toEqual([5])
    expect(includeEffect(m.byId.get(5)!, ticks, e)).toBe("Approved, but it can't start until you approve #2.")
    expect(includeEffect(m.byId.get(2)!, ticks, e)).toBe('Left out: it stays awaiting your approval. 2 approved tickets after it would wait.')
    expect(includeEffect(m.byId.get(1)!, ticks, e)).toBe('Starts right away when you approve.')
  })

  it('draws an "after" chip only where a dependency leaves the story, amber when it points at a left-out ticket', () => {
    const plan = busyPlan()
    plan.tickets.push(ticket(6, { story_id: 414, dependency_ids: [1, 2, 3, 77], waits_on: [1, 2, 3, 77] }))
    plan.outside = [{ id: 77, project_id: 2, epic_id: 169, story_id: 1, name: 'Elsewhere', state: 'in_progress', performer: 'agent', merge_state: 'none', satisfied: false, epic_name: 'Other', relation: 'dependency' }]
    const m = buildModel(plan)
    expect(afterChips(m, 2, new Set())).toEqual([]) // 2 → 1 stays in story 413
    const chips = afterChips(m, 6, new Set([2]))
    expect(chips[0].ids).toEqual([1, 2, 3])
    expect(chips[0].held).toBe(true)
    expect(chipIds(chips[0].ids)).toBe('#1, #2 +1')
    expect(chips[1].outside?.id).toBe(77)
  })

  it('splits several epics and keeps one model', () => {
    const plan = busyPlan()
    plan.epics = [...plan.epics!, { id: 2, name: 'Dex', description: '' }]
    plan.stories.push({ id: 407, epic_id: 2, name: 'Dex freeze', description: '', acceptance_criteria: '', task_ids: [9], awaiting_ids: [9] })
    plan.tickets.push(ticket(9, { epic_id: 2, story_id: 407, starts_on_approval: true }))
    const m = buildModel(plan)
    expect(m.epics.map(e => e.epic.id)).toEqual([172, 2])
    expect(m.order.at(-1)).toBe(9)
  })
})

describe('textDiff', () => {
  it('marks removed and added lines', () => {
    expect(lineDiff('a\nb\nc', 'a\nB\nc')).toEqual([
      { kind: 'same', text: 'a' }, { kind: 'del', text: 'b' }, { kind: 'add', text: 'B' }, { kind: 'same', text: 'c' }])
    expect(changedLines('x', 'x\n\ny')).toEqual([{ kind: 'add', text: 'y' }])
  })
})

describe('suggestAnswers', () => {
  it('reads either-or options from the last question', () => {
    expect(suggestAnswers('The keychain is locked during QA. Should I fall back to a 0600 file under ~/.config/amp, or fail with a clear error?'))
      .toEqual(['Fall back to a 0600 file under ~/.config/amp', 'Fail with a clear error'])
    expect(suggestAnswers('Avalara or Stripe Tax?')).toEqual(['Avalara', 'Stripe Tax'])
  })
  it('keeps the question out of the options when its subject is a noun', () => {
    expect(suggestAnswers('Should widgets use UUID or bigint ids?')).toEqual(['UUID', 'Bigint ids'])
    expect(suggestAnswers('Should the API return JSON or XML?')).toEqual(['JSON', 'XML'])
    expect(suggestAnswers('Is the bug in the API or the UI?')).toEqual(['The API', 'The UI'])
    // A stem that doesn't come off cleanly gives no chips, not a chip with the question in it.
    expect(suggestAnswers('Should UUID or bigint be used?')).toEqual([])
    for (const q of ['Should widgets use UUID or bigint ids?', 'Should I fall back to a file, or fail?']) {
      expect(suggestAnswers(q).some(c => /^(should|shall|do|does|can|is|are)\b/i.test(c))).toBe(false)
    }
  })
  it('suggests nothing for an open question', () => {
    expect(suggestAnswers('Which provider do you want?')).toEqual([])
    expect(suggestAnswers('Ship it.')).toEqual([])
  })
})

describe('human actions helpers', () => {
  it('knows what a ticket asks of a person', () => {
    expect(actionKindOf({ state: 'blocked', block_reason: 'Which one?', performer: 'agent' })).toBe('answer')
    expect(actionKindOf({ state: 'blocked', block_reason: 'scheduled: 9am', performer: 'agent' })).toBe(null)
    expect(actionKindOf({ state: 'completed', merge_state: 'needed', performer: 'agent' })).toBe('merge')
    expect(actionKindOf({ state: 'completed', merge_state: 'merged', performer: 'agent' })).toBe(null)
    expect(actionKindOf({ state: 'backlog', performer: 'human' })).toBe('do')
  })
  it('says a done ticket\'s merge in one line', () => {
    expect(mergeStatusText({ state: 'completed', merge_state: 'needed' })).toBe('Done · Merge needed')
    expect(mergeStatusText({ state: 'completed', merge_state: 'merged', merge_source: 'github' })).toBe('Merged (recorded by GitHub)')
    expect(mergeStatusText({ state: 'in_progress', merge_state: 'needed' })).toBe(null)
  })
})

describe('?approve= route state', () => {
  it('parses epic ids and all', async () => {
    const { parseApprove, formatApprove } = await import('../hooks/useApproveParam')
    expect(parseApprove('172')).toEqual([172])
    expect(parseApprove('172,2')).toEqual([172, 2])
    expect(parseApprove('ALL')).toBe('all')
    expect(parseApprove('x')).toBe(null)
    expect(parseApprove('1,,2')).toBe(null)
    expect(formatApprove([172, 2])).toBe('172,2')
  })
})
