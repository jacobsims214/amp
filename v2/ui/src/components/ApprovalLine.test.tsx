import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { ApprovalLine } from './TaskDrawer'
import type { Task } from '../types'
import type { Person } from '../lib/people'

const people = new Map<number, Person>([
  [7, { id: 7, name: 'Lee Lead', email: 'lee@example.com' }],
  [9, { id: 9, name: 'Bob Builder', email: 'bob@example.com' }],
])
const task = (over: Partial<Task> = {}): Task => ({
  id: 5, project_id: 3, epic_id: 1, story_id: 1, name: 'T', description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_at: '2026-10-22T10:00:00Z', planned_at: '2026-10-22T10:00:00Z',
  updated_at: '', owner_user_id: 9, created_by: 9, ...over,
})
const text = (t: Task, meId?: number) => renderToStaticMarkup(<MemoryRouter><ApprovalLine task={t} people={people} meId={meId} /></MemoryRouter>)
  .replace(/<[^>]+>/g, '').replace(/&#x27;/g, "'")

describe('ApprovalLine (ticket drawer)', () => {
  it('shows who approved the ticket', () => {
    expect(text(task({ approved_at: '2026-10-22T11:00:00Z', approved_by: 9 }))).toMatch(/^Approved by Bob Builder · /)
  })
  it('says when it was approved on the owner\'s behalf', () => {
    expect(text(task({ approved_at: '2026-10-22T11:00:00Z', approved_by: 7 }), 7)).toContain('Approved by Lee Lead (you) on behalf of Bob Builder')
  })
  it('credits the upgrade when no person is recorded', () => {
    expect(text(task({ approved_at: '2026-10-22T11:00:00Z', approved_by: null }))).toContain('Approved by the AMP upgrade')
  })
  it('says an open ticket is not approved, or changed since, with a Flight plan link', () => {
    expect(text(task())).toContain('Plan not approved yet.Approve in Flight plan')
    expect(text(task({ approved_at: '2026-10-22T09:00:00Z', approved_by: 9 }))).toContain('Plan not approved yet (changed since it was approved).')
    expect(renderToStaticMarkup(<MemoryRouter><ApprovalLine task={task()} people={people} /></MemoryRouter>)).toContain('href="/project/3/plan#approve"')
  })
  it('says nothing for a done ticket nobody approved', () => {
    expect(text(task({ state: 'completed' }))).toBe('')
  })
})
