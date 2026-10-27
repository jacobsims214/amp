import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { TaskCard } from './TaskCard'
import { mergeBadge, mergeWait } from '../lib/merge'
import type { Task } from '../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 2471, project_id: 3, epic_id: 1, story_id: 1, name: 'Build it', description: '', acceptance_criteria: '',
  state: 'completed', priority: '1', dependency_ids: [], created_at: '', updated_at: '', ...over,
})
const card = (t: Task) => render(<TaskCard task={t} onClick={() => {}} />)

describe('merge signal helpers', () => {
  it('badges only done tickets that need a merge or whose PR closed', () => {
    expect(mergeBadge(task({ merge_state: 'needed' }))?.label).toBe('Merge needed')
    expect(mergeBadge(task({ merge_state: 'abandoned' }))?.label).toBe('PR closed')
    expect(mergeBadge(task({ merge_state: 'merged' }))).toBeNull()
    expect(mergeBadge(task({ merge_state: 'none' }))).toBeNull()
    expect(mergeBadge(task({}))).toBeNull() // an older server sends no merge_state
    expect(mergeBadge(task({ state: 'in_progress', merge_state: 'needed' }))).toBeNull()
  })
  it('reads waiting_detail on open tickets only', () => {
    expect(mergeWait(task({ state: 'blocked', waiting_detail: ' Waiting on merge of #2471 (PR #88) ' }))).toBe('Waiting on merge of #2471 (PR #88)')
    expect(mergeWait(task({ state: 'blocked' }))).toBeNull()
    expect(mergeWait(task({ state: 'completed', waiting_detail: 'x' }))).toBeNull()
  })
})

describe('TaskCard', () => {
  it('shows "Merge needed" on a done ticket with an unmerged PR', () => {
    card(task({ merge_state: 'needed' }))
    expect(screen.getByText('Merge needed')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Done · Merge needed/ })).toBeInTheDocument()
  })
  it('shows no merge badge once merged', () => {
    card(task({ merge_state: 'merged' }))
    expect(screen.queryByText('Merge needed')).toBeNull()
  })
  it('a waiting card says which merge it waits on', () => {
    card(task({ id: 2480, state: 'blocked', blocked_by_ids: [2471], waiting_detail: 'Waiting on merge of #2471 (PR #88)' }))
    expect(screen.getByText('Waiting on merge of #2471 (PR #88)')).toBeInTheDocument()
    expect(screen.queryByText('Merge needed')).toBeNull()
  })
  it('a question card never shows waiting_detail', () => {
    card(task({ state: 'blocked', block_reason: 'Which API?', waiting_detail: 'Waiting on merge of #1 (PR #2)' }))
    expect(screen.queryByText(/Waiting on merge/)).toBeNull()
  })
})
