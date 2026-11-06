import { beforeEach, describe, expect, it } from 'vitest'
import { EMPTY_PREFS, SIDEBAR_KEY, loadPrefs, moveInOrder, savePrefs, sortByOrder } from './sidebarPrefs'

beforeEach(() => localStorage.clear())

describe('sidebarPrefs', () => {
  it('round-trips and survives junk', () => {
    savePrefs({ pinned: [2], order: [3, 1], collapsedOrgs: ['org:7'], openProjects: [1] })
    expect(loadPrefs()).toEqual({ pinned: [2], order: [3, 1], collapsedOrgs: ['org:7'], openProjects: [1] })
    localStorage.setItem(SIDEBAR_KEY, '{not json')
    expect(loadPrefs()).toEqual(EMPTY_PREFS)
    localStorage.setItem(SIDEBAR_KEY, JSON.stringify({ pinned: 'x', order: [1, 'a'] }))
    expect(loadPrefs()).toEqual({ pinned: [], order: [1], collapsedOrgs: [], openProjects: [] })
  })
  it('sorts known ids by order and keeps unknown ones after, in API order', () => {
    const items = [{ id: 1 }, { id: 2 }, { id: 3 }, { id: 4 }]
    expect(sortByOrder(items, [3, 1]).map(x => x.id)).toEqual([3, 1, 2, 4])
  })
  it('moves within bounds only', () => {
    expect(moveInOrder([1, 2, 3], 2, -1)).toEqual([2, 1, 3])
    expect(moveInOrder([1, 2, 3], 1, -1)).toEqual([1, 2, 3])
  })
})
