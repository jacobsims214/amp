import type { Priority } from '../types'
import { PriorityMark } from './ui/PriorityMark'

/** High and critical priority as bars and a label; normal and low show nothing. */
export function PriorityBadge({ priority }: { priority: Priority }) {
  if (priority === '0' || priority === '1') return null
  return <PriorityMark priority={priority} showLabel />
}
