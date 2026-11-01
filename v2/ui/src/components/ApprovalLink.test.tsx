import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { ApprovalLink, type ApprovalLinkProps } from './ApprovalLink'
import type { Person } from '../lib/people'
import type { Project, Task } from '../types'

const people = new Map<number, Person>([[7, { id: 7, name: 'Jo Person', email: 'jo@example.com' }]])
const project: Project = {
  id: 3, name: 'P', code: 'P', description: '', state: 'active', created_at: '', updated_at: '',
  require_plan_approval: true, plan_approved_at: '2026-10-21T15:30:00Z', plan_approved_by: 7,
}
const task = (id: number, owner: number): Task => ({
  id, project_id: 3, epic_id: 1, story_id: 1, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], owner_user_id: owner, created_by: owner, created_at: '', updated_at: '',
})
const show = (p: Partial<ApprovalLinkProps>) => render(
  <MemoryRouter><ApprovalLink projectId={3} project={project} awaiting={[]} meId={7} people={people} {...p} /></MemoryRouter>,
)

describe('ApprovalLink', () => {
  it('one ticket: singular, linked to My work focused on it', () => {
    show({ awaiting: [task(11, 7)] })
    expect(screen.getByRole('link', { name: /1 ticket awaits your approval/ })).toHaveAttribute('href', '/project/3/my-work?focus=11')
    expect(screen.queryByRole('button')).toBeNull()
  })
  it("names other people's waiting tickets as text, not a link", () => {
    show({ awaiting: [task(12, 9)] })
    expect(screen.getByText('None of your tickets await approval.')).toBeInTheDocument()
    expect(screen.getByText(/1 ticket awaits its owner/)).toBeInTheDocument()
    expect(screen.queryByRole('link')).toBeNull()
  })
  it('nothing waits: nothing on the Board, a quiet "Plan approved" on Flight plan', () => {
    const { container } = show({})
    expect(container).toBeEmptyDOMElement()
    show({ showApproved: true })
    expect(screen.getByText('Plan approved.')).toBeInTheDocument()
  })
})
