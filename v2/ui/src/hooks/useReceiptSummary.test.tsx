// useReceiptSummary reloads on receipt.updated and task.completed, coalesced:
// a burst is one reload, at most one per 2 s, and a lone event still reloads
// within 2 s (#2510).
import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ReceiptSummary, SSEEvent } from '../types'

let emit: (e: SSEEvent) => void = () => {}
vi.mock('./useSSE', () => ({
  useSSE: (_pid: number | null, onEvent: (e: SSEEvent) => void) => { emit = onEvent },
}))
const summaryOf = (ids: number[]) =>
  ({ mode: 'warn', tasks: ids.map(task_id => ({ task_id, status: 'verified', held: false, has_media: false })), would_hold_last_7_days: 0 }) as unknown as ReceiptSummary
let taskIds: number[] = []
const receiptSummary = vi.fn<(pid: number) => Promise<ReceiptSummary>>(() => Promise.resolve(summaryOf(taskIds)))
vi.mock('../api/client', () => ({ api: { receiptSummary: (pid: number) => receiptSummary(pid) } }))
const { useReceiptSummary } = await import('./useReceiptSummary')

const event = (type: SSEEvent['type'], project_id = 1) => ({ type, project_id, payload: {} }) as unknown as SSEEvent
const send = (type: SSEEvent['type'] = 'task.completed') => act(() => { emit(event(type)) })
// Advance fake time and let the loads it starts answer, inside act.
const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms) })
const reloads = () => receiptSummary.mock.calls.length - 1 // the first is the mount load

beforeEach(() => {
  vi.useFakeTimers()
  receiptSummary.mockClear()
  taskIds = []
})
afterEach(() => { vi.useRealTimers() })

async function mount(pid = 1) {
  const hook = renderHook(({ id }) => useReceiptSummary(id), { initialProps: { id: pid as number | null } })
  await tick(0)
  expect(receiptSummary).toHaveBeenCalledTimes(1)
  return hook
}

describe('useReceiptSummary', () => {
  it('turns five task.completed events within 2 s into one reload', async () => {
    const { result } = await mount()
    for (let i = 0; i < 5; i++) {
      send()
      await tick(400) // events at 0, 400, 800, 1200 and 1600 ms
    }
    expect(reloads()).toBe(1)
    await tick(10_000)
    expect(reloads()).toBe(1)
    expect(result.current.summary).not.toBeNull()
  })

  it('still reloads a single event within 2 s, with what the server has then', async () => {
    const { result } = await mount()
    taskIds = [7]
    send()
    await tick(1999)
    expect(reloads()).toBe(0)
    await tick(1)
    expect(reloads()).toBe(1)
    expect(result.current.byTask.get(7)?.status).toBe('verified')
  })

  it('reloads within 2 s of the first event after a quiet period', async () => {
    await mount()
    send()
    await tick(2000)
    expect(reloads()).toBe(1)
    await tick(30_000)
    send()
    await tick(2000)
    expect(reloads()).toBe(2)
  })

  it('never lets a steady stream of events put the reload off', async () => {
    await mount()
    // An event every 500 ms for 6 s: one reload per 2 s, the first at 2 s.
    for (let t = 0; t < 6000; t += 500) {
      send()
      await tick(500)
      if (t === 1500) expect(reloads()).toBe(1)
    }
    expect(reloads()).toBe(3)
  })

  it('coalesces receipt.updated with task.completed, and ignores other events', async () => {
    await mount()
    send('task.completed')
    send('receipt.updated')
    send('task.updated')
    await tick(2000)
    expect(reloads()).toBe(1)
    send('task.updated')
    send('comment.added')
    await tick(5000)
    expect(reloads()).toBe(1)
  })

  it('clears a scheduled reload on unmount', async () => {
    const { unmount } = await mount()
    send()
    unmount()
    expect(vi.getTimerCount()).toBe(0)
    await tick(5000)
    expect(reloads()).toBe(0)
  })

  it('drops a reload scheduled for the old project when the project changes', async () => {
    const { rerender } = await mount(1)
    send()
    rerender({ id: 2 })
    await tick(0)
    expect(receiptSummary).toHaveBeenLastCalledWith(2)
    await tick(5000)
    expect(receiptSummary.mock.calls.map(c => c[0])).toEqual([1, 2])
  })

  it('a manual reload covers a scheduled one', async () => {
    const { result } = await mount()
    send()
    await tick(500)
    act(() => { result.current.reload() })
    expect(reloads()).toBe(1)
    await tick(5000)
    expect(reloads()).toBe(1)
  })
})
