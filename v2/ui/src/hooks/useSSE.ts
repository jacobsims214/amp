import { useEffect, useRef } from 'react'
import type { SSEEvent } from '../types'

export function useSSE(
  projectId: number | null,
  onEvent: (event: SSEEvent) => void,
) {
  const onEventRef = useRef(onEvent)
  onEventRef.current = onEvent

  useEffect(() => {
    const es = new EventSource('/api/events')

    es.onmessage = (e) => {
      try {
        const event: SSEEvent = JSON.parse(e.data)
        // Filter to relevant project if specified
        if (projectId !== null && event.project_id !== undefined && event.project_id !== projectId) {
          return
        }
        onEventRef.current(event)
      } catch {
        // ignore malformed events
      }
    }

    es.onerror = () => {
      // EventSource auto-reconnects; we just let it
    }

    return () => es.close()
  }, [projectId])
}
