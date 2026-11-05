/**
 * Projects by id, shared across the app (KB design/loading.md, "Shared data").
 *
 * The sidebar, the project header, the board and the ticket drawer all want
 * the same project (its name, code and settings). One module-level store
 * keeps the last answer per id, so a view that mounts later renders it on its
 * first frame, and concurrent loads share one request. A cached project is
 * shown at once and re-read in the background when it is older than
 * FRESH_MS (stale-while-revalidate).
 */
import { useEffect, useSyncExternalStore } from 'react'
import { api } from '../api/client'
import type { Project } from '../types'

const FRESH_MS = 30_000

const cache = new Map<number, { project: Project; at: number }>()
const inflight = new Map<number, Promise<Project | null>>()
const listeners = new Set<() => void>()
const emit = () => listeners.forEach(l => l())

/** Read project `id` again (shared with any read already in flight). Failures keep what's cached. */
export function loadProject(id: number): Promise<Project | null> {
  const running = inflight.get(id)
  if (running) return running
  const p = api.getProject(id)
    .then(project => {
      if (!project?.id) return cache.get(id)?.project ?? null
      cache.set(id, { project, at: Date.now() })
      emit()
      return project
    })
    .catch(() => cache.get(id)?.project ?? null)
    .finally(() => { inflight.delete(id) })
  inflight.set(id, p)
  return p
}

/** Put projects the app already has (the sidebar's list, a save's answer) in the store. */
export function primeProjects(list: Project[]) {
  const at = Date.now()
  for (const project of list) cache.set(project.id, { project, at })
  if (list.length) emit()
}

/** The cached project, or null before its first read. */
export function cachedProject(id: number): Project | null {
  return cache.get(id)?.project ?? null
}

/** For tests: forget every project. */
export function resetProjectCache() {
  cache.clear()
  inflight.clear()
  emit()
}

function subscribe(l: () => void) {
  listeners.add(l)
  return () => { listeners.delete(l) }
}

/**
 * Project `id` from the store, read on mount when it isn't cached or is
 * stale. Null until it's known (or when id is not a project id).
 */
export function useProject(id: number | null | undefined): Project | null {
  const valid = id != null && Number.isInteger(id) && id > 0
  const project = useSyncExternalStore(subscribe, () => (valid ? cachedProject(id) : null), () => null)
  useEffect(() => {
    if (!valid) return
    const hit = cache.get(id)
    if (!hit || Date.now() - hit.at > FRESH_MS) void loadProject(id)
  }, [id, valid])
  return project
}

// Test setup resets every shared store after each test (src/test/setup.ts).
;((globalThis as { __ampStoreResets?: Set<() => void> }).__ampStoreResets ??= new Set()).add(resetProjectCache)
