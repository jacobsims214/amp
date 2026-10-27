/**
 * The "waiting on you" badges (KB architecture/waiting-on-you.md): the
 * sidebar's total and per-project counts and the project's My work tab count,
 * all from GET /api/my-work/counts, so every badge shows the same number.
 *
 * One shared store: AppShell refreshes it on navigation, My work refreshes it
 * after every action, and any component reads it with useWaitCounts().
 */
import { useSyncExternalStore } from 'react'
import { api } from '../api/client'
import type { MyWorkCounts, WaitCounts } from '../types'

export const ZERO_COUNTS: WaitCounts = { waiting: 0, approve: 0, answer: 0, merge: 0, do: 0, coming_up: 0 }

let current: MyWorkCounts | null = null
let inflight: Promise<MyWorkCounts | null> | null = null
const listeners = new Set<() => void>()

function emit() { listeners.forEach(l => l()) }

/** Fetch the counts again. Concurrent calls share one request; failures keep the last counts. */
export function refreshWaitCounts(): Promise<MyWorkCounts | null> {
  if (inflight) return inflight
  inflight = api.myWorkCounts()
    .then(c => {
      current = { counts: { ...ZERO_COUNTS, ...c?.counts }, projects: c?.projects ?? {} }
      emit()
      return current
    })
    .catch(() => current)
    .finally(() => { inflight = null })
  return inflight
}

/** For tests: forget the counts. */
export function resetWaitCounts(value: MyWorkCounts | null = null) {
  current = value
  inflight = null
  emit()
}

function subscribe(l: () => void) {
  listeners.add(l)
  return () => { listeners.delete(l) }
}

/** The latest counts, or null before the first fetch. */
export function useWaitCounts(): MyWorkCounts | null {
  return useSyncExternalStore(subscribe, () => current, () => current)
}

/** One project's counts (zeros when nothing waits there). */
export function projectCounts(c: MyWorkCounts | null, projectId: number | string): WaitCounts | null {
  if (!c) return null
  return { ...ZERO_COUNTS, ...c.projects[String(projectId)] }
}
