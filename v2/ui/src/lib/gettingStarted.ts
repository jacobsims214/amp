import type { GettingStarted } from '../types'

/**
 * Every step of the Get started checklist is done. The fourth step, seeing a
 * receipt, arrived with verification receipts (#149), which landed after
 * this list.
 */
export function gettingStartedDone(g: GettingStarted): boolean {
  return g.agent.done && g.project.done && g.plan.done && g.receipt.done
}
