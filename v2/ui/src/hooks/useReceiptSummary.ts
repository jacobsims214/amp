import { useCallback, useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import { useSSE } from './useSSE'
import type { ReceiptSummary, ReceiptSummaryRow } from '../types'

/**
 * A project's verification receipts summary (#126): its setting and each
 * ticket's latest receipt status. Loaded once, and again on receipt.updated
 * (and on task.completed, when a completion may have made one). null until
 * it answers, or when it fails: callers then show no badges.
 */
export function useReceiptSummary(projectId: number | null) {
  const [summary, setSummary] = useState<{ pid: number; data: ReceiptSummary } | null>(null)
  const load = useCallback(() => {
    if (projectId == null) return
    const pid = projectId
    api.receiptSummary(pid).then(data => setSummary({ pid, data })).catch(() => {})
  }, [projectId])
  useEffect(() => { load() }, [load])
  useSSE(projectId, e => { if (e.type === 'receipt.updated' || e.type === 'task.completed') load() })
  const data = summary?.pid === projectId ? summary.data : null
  const byTask = useMemo(() => {
    const m = new Map<number, ReceiptSummaryRow>()
    for (const r of data?.tasks ?? []) m.set(r.task_id, r)
    return m
  }, [data])
  return { summary: data, byTask, reload: load }
}
