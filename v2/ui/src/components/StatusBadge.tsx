import type { TaskState } from '../types'
import { displayState } from '../lib/ticketState'
import { StateTag } from './ui/StateTag'

/** A ticket's state. Pass blockReason so a blocked ticket shows as waiting,
 *  flagged, or scheduled. */
export function StatusBadge({ state, blockReason }: { state: TaskState; blockReason?: string }) {
  return <StateTag state={displayState({ state, block_reason: blockReason })} />
}
