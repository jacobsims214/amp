import { afterEach, describe, expect, it, vi } from 'vitest'
import { LOAD_TIMEOUT_MS, TimeoutError, withTimeout } from './timeout'

afterEach(() => vi.useRealTimers())

describe('withTimeout', () => {
  it('passes through a value that arrives in time', async () => {
    await expect(withTimeout(Promise.resolve(7), 'Loading')).resolves.toBe(7)
  })

  it('passes through a rejection that arrives in time', async () => {
    await expect(withTimeout(Promise.reject(new Error('nope')), 'Loading')).rejects.toThrow('nope')
  })

  it('rejects with a TimeoutError naming the load once the wait runs out', async () => {
    vi.useFakeTimers()
    const p = withTimeout(new Promise(() => {}), 'Loading the board')
    const check = expect(p).rejects.toSatisfy((e: unknown) =>
      e instanceof TimeoutError && /Loading the board took longer than 12 seconds/.test(e.message))
    await vi.advanceTimersByTimeAsync(LOAD_TIMEOUT_MS)
    await check
  })
})
