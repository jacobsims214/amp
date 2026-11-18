import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../api/client'
import { useSSE } from './useSSE'
import type { ReceiptSummary, ReceiptSummaryRow } from '../types'

/** Live events that may change the summary arrive in bursts (a wave of agents
 *  completing at once, each completion making a receipt), and every open tab
 *  hears them all. A burst causes one reload, at most one per this long. The
 *  window opens at the first event and later ones don't extend it, so a lone
 *  event still reloads within it and a steady stream can't put reloads off. */
const RELOAD_COALESCE_MS = 2000

/**
 * A project's verification receipts summary (#126): its setting and each
 * ticket's latest receipt status. Loaded once, and again on receipt.updated
 * (and on task.completed, when a completion may have made one), coalesced to
 * one reload per RELOAD_COALESCE_MS. null until it answers, or when it fails:
 * callers then show no badges.
 */
export function useReceiptSummary(projectId: number | null) {
  const [summary, setSummary] = useState<{ pid: number; data: ReceiptSummary } | null>(null)
  const load = useCallback(() => {
    if (projectId == null) return
    const pid = projectId
    api.receiptSummary(pid).then(data => setSummary({ pid, data })).catch(() => {})
  }, [projectId])
  const pending = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const cancelPending = useCallback(() => { clearTimeout(pending.current); pending.current = undefined }, [])
  // Loading now covers any reload still scheduled.
  const reload = useCallback(() => { cancelPending(); load() }, [cancelPending, load])
  // A scheduled reload is for this project: drop it when the project changes
  // or the caller unmounts.
  useEffect(() => { reload(); return cancelPending }, [reload, cancelPending])
  useSSE(projectId, e => {
    if (e.type !== 'receipt.updated' && e.type !== 'task.completed') return
    if (pending.current !== undefined) return
    pending.current = setTimeout(() => { pending.current = undefined; load() }, RELOAD_COALESCE_MS)
  })
  const data = summary?.pid === projectId ? summary.data : null
  const byTask = useMemo(() => {
    const m = new Map<number, ReceiptSummaryRow>()
    for (const r of data?.tasks ?? []) m.set(r.task_id, r)
    return m
  }, [data])
  return { summary: data, byTask, reload }
}
