import type { HumanReason, Task } from '../types'

/** Why a person, not an agent, does a ticket. Mirrors domain.HumanReasons. */
export const HUMAN_REASONS: { value: HumanReason; label: string; hint: string; verb: string }[] = [
  // Merges are not human tickets (KB working/human-work.md): AMP tracks them as Merge needed.
  { value: 'review', label: 'Review', hint: "A person reviews something that isn't code going in (a document, a design, a vendor's work)", verb: 'Review' },
  { value: 'approval', label: 'Approval', hint: 'A sign-off only a person can give (release, spend, legal, security)', verb: 'Sign off' },
  { value: 'access', label: 'Access', hint: "Needs access or credentials agents don't have", verb: 'Do with your access' },
  { value: 'judgment', label: 'Judgment', hint: 'A decision or judgment call', verb: 'Decide' },
  { value: 'external', label: 'External', hint: 'Depends on someone outside the team', verb: 'Get from someone outside' },
  { value: 'physical', label: 'Physical', hint: 'Physical or offline work (hardware, a visit, a call)', verb: 'Do offline' },
  { value: 'other', label: 'Other', hint: 'Anything else a person has to do', verb: 'Do' },
]

export const reasonLabel = (r?: string) => HUMAN_REASONS.find(x => x.value === r)?.label ?? 'Person'
export const reasonHint = (r?: string) => HUMAN_REASONS.find(x => x.value === r)?.hint ?? ''
/** What the person does, as the human panel's heading: "Decide", "Sign off"… */
export const reasonVerb = (r?: string) => HUMAN_REASONS.find(x => x.value === r)?.verb ?? 'Do'

/** A ticket its owner does themselves, tracked in AMP. */
export const isHuman = (t: Pick<Task, 'performer'>) => t.performer === 'human'
