import type { GettingStarted } from '../types'

/** Every step of the Get started checklist is done. */
export function gettingStartedDone(g: GettingStarted): boolean {
  // The "See your first receipt" step is added by whichever of this change
  // (#128) and verification receipts (#149) lands second.
  return g.agent.done && g.project.done && g.plan.done
}
