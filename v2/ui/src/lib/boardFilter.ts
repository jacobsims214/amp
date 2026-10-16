import type { OwnerFilter } from './people'

export interface FilterState {
  query: string
  state: '' | 'backlog' | 'in_progress' | 'completed' | 'blocked'
  epicId: number | null
  /** whose tickets to show; defaults to 'mine' */
  owner: OwnerFilter
}

export const DEFAULT_FILTER: FilterState = { query: '', state: '', epicId: null, owner: 'mine' }
