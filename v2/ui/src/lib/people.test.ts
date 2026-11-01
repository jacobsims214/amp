import { describe, expect, it } from 'vitest'
import { ownerFilterNarrows, parseOwnerFilter, taskMatchesOwner } from './people'
import type { Task } from '../types'

const task = (owner_user_id: number | null, created_by: number | null = null): Task => ({
  id: 1, project_id: 3, epic_id: 1, story_id: 1, name: 'T', description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_at: '', updated_at: '', owner_user_id, created_by,
})

describe('board owner filter', () => {
  it('Mine is tickets I own, nothing else (decisions/human-work-ux.md §1)', () => {
    expect(taskMatchesOwner(task(7), 'mine', 7)).toBe(true)
    expect(taskMatchesOwner(task(9, 7), 'mine', 7)).toBe(false)
    // unowned tickets I created, or with no creator, are no longer "mine"
    expect(taskMatchesOwner(task(null, 7), 'mine', 7)).toBe(false)
    expect(taskMatchesOwner(task(null, null), 'mine', 7)).toBe(false)
  })

  it('Mine without a signed-in user shows everything (local dev)', () => {
    expect(ownerFilterNarrows('mine', undefined)).toBe(false)
    expect(taskMatchesOwner(task(9), 'mine', undefined)).toBe(true)
  })

  it('Unowned finds the tickets nobody owns, whoever created them', () => {
    expect(taskMatchesOwner(task(null, 7), 'unowned', 7)).toBe(true)
    expect(taskMatchesOwner(task(null, null), 'unowned', 7)).toBe(true)
    expect(taskMatchesOwner(task(7, 7), 'unowned', 7)).toBe(false)
    expect(ownerFilterNarrows('unowned', 7)).toBe(true)
  })

  it('a member id and Everyone', () => {
    expect(taskMatchesOwner(task(9), 9, 7)).toBe(true)
    expect(taskMatchesOwner(task(7), 9, 7)).toBe(false)
    expect(taskMatchesOwner(task(null), 'everyone', 7)).toBe(true)
  })

  it('parses select values; the old "unassigned" reads as Unowned', () => {
    expect(parseOwnerFilter('mine')).toBe('mine')
    expect(parseOwnerFilter('unowned')).toBe('unowned')
    expect(parseOwnerFilter('unassigned')).toBe('unowned')
    expect(parseOwnerFilter('everyone')).toBe('everyone')
    expect(parseOwnerFilter('42')).toBe(42)
    expect(parseOwnerFilter('junk')).toBe('mine')
  })
})
