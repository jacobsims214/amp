import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'
// Shared module stores (lib/boardStore, lib/projectCache) live for the whole
// test file: reset them so no test sees another's data. They register their
// reset on globalThis; importing them here would load the real api client
// before a test file's vi.mock could replace it.
afterEach(() => {
  cleanup()
  ;(globalThis as { __ampStoreResets?: Set<() => void> }).__ampStoreResets?.forEach(reset => reset())
})
