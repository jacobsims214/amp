// A page load that never answers used to leave its loader spinning forever.
// withTimeout gives the wait an end: past LOAD_TIMEOUT_MS it rejects with a
// TimeoutError, so the page can say so and offer Retry.

export const LOAD_TIMEOUT_MS = 12_000

export class TimeoutError extends Error {
  constructor(what: string, ms: number) {
    super(`${what} took longer than ${Math.round(ms / 1000)} seconds. Check that AMP is running, then retry.`)
    this.name = 'TimeoutError'
  }
}

/**
 * Settle with promise, or reject with a TimeoutError after ms. what names the
 * load in the message ("Loading the board"). The underlying request is not
 * cancelled; callers ignore a late answer (see useBoardData's load counter).
 */
export function withTimeout<T>(promise: Promise<T>, what: string, ms = LOAD_TIMEOUT_MS): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new TimeoutError(what, ms)), ms)
    promise.then(
      v => { clearTimeout(timer); resolve(v) },
      e => { clearTimeout(timer); reject(e) },
    )
  })
}
