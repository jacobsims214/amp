import { useEffect, useEffectEvent } from 'react'
import { subscribeSSE } from './useSSE'

/**
 * Calls `onReconnect` when AMP's live stream (/api/events) reconnects after the
 * view has loaded, so the view can re-read what it missed while the stream was
 * down: a dropped connection, or a tab hidden long enough to let its stream go
 * (HIDDEN_CLOSE_MS in useSSE).
 *
 * Each {"type":"connected"} frame that arrives while `ready` is true calls
 * `onReconnect` once. Frames that arrive while `ready` is false are dropped, not
 * saved for later. Pass `ready` as "the view's first load has settled", so the
 * opening frame of a new stream, which arrives during that load, doesn't read
 * the data a second time.
 *
 * There is deliberately no "skip the first connected" rule. A view that mounts
 * on a stream another view already holds never sees an opening frame, so that
 * rule would skip its first real reconnect.
 *
 * This works the same way in the tab that holds the stream and in the tabs it
 * relays to. A tab that joins, or comes back after being hidden, gets exactly
 * one connected. The latest `onReconnect` and `ready` are always the ones used,
 * so passing a new function each render does not resubscribe.
 */
export function useStreamReconnect(onReconnect: () => void, ready: boolean): void {
  const onConnected = useEffectEvent(() => {
    if (ready) onReconnect()
  })

  useEffect(() => subscribeSSE('/api/events', (event) => {
    if (event.type === 'connected') onConnected()
  }), [])
}
