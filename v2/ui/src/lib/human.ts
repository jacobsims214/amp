import type { HumanReason, Task } from '../types'

/** Why a person, not an agent, does a ticket. Mirrors domain.HumanReasons. */
export const HUMAN_REASONS: { value: HumanReason; label: string; hint: string }[] = [
  { value: 'review', label: 'Review', hint: 'A person reviews or merges (code review, PR merge)' },
  { value: 'approval', label: 'Approval', hint: 'A sign-off only a person can give (release, spend, legal, security)' },
  { value: 'access', label: 'Access', hint: "Needs access or credentials agents don't have" },
  { value: 'judgment', label: 'Judgment', hint: 'A decision or judgment call' },
  { value: 'external', label: 'External', hint: 'Depends on someone outside the team' },
  { value: 'physical', label: 'Physical', hint: 'Physical or offline work (hardware, a visit, a call)' },
  { value: 'other', label: 'Other', hint: 'Anything else a person has to do' },
]

export const reasonLabel = (r?: string) => HUMAN_REASONS.find(x => x.value === r)?.label ?? 'Person'
export const reasonHint = (r?: string) => HUMAN_REASONS.find(x => x.value === r)?.hint ?? ''

/** A ticket its owner does themselves, tracked in AMP. */
export const isHuman = (t: Pick<Task, 'performer'>) => t.performer === 'human'
