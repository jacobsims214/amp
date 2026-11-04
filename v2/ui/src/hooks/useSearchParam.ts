import { useCallback } from 'react'
import { useSearchParams } from 'react-router-dom'

/**
 * One query param as state, e.g. the ticket drawer's `?tab=`.
 *
 * - Reads `?<name>=`, falling back to `defaultValue` when it is absent.
 * - Setting the default (or null) removes the param, so URLs only carry
 *   what differs from what the page would show anyway.
 * - Other query params are left alone.
 * - Writes replace the current history entry unless `push` is asked for:
 *   a tab or a filter isn't a place to go back to.
 */
export function useSearchParam(name: string, defaultValue: string | null = null): [string | null, (value: string | null, opts?: { push?: boolean }) => void] {
  const [params, setParams] = useSearchParams()
  const value = params.get(name) ?? defaultValue

  const setValue = useCallback((v: string | null, opts?: { push?: boolean }) => {
    setParams(prev => {
      const next = new URLSearchParams(prev)
      if (v == null || v === defaultValue) next.delete(name)
      else next.set(name, v)
      return next
    }, { replace: !opts?.push })
  }, [name, defaultValue, setParams])

  return [value, setValue]
}
