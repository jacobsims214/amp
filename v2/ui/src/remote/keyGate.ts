/**
 * Keeps the device key's private-key operations on the session page (#2381 M6, #2393).
 *
 * The device key signs prompts that run on the laptop, so only a document that the
 * strict SESSION_CSP protects should use it: one that was loaded in full at
 * /sessions/… (nginx sends SESSION_CSP only on a full load of that path) and is still
 * there. A document loaded at /board that later navigates client-side to /sessions/…
 * runs under the looser baseline CSP, so it is refused too; links to a session page
 * are plain `<a href>` (csp.test.ts forbids router links to it).
 *
 * Residual risk: this gates AMP's own code path, not the origin. The key is
 * non-extractable but lives in this origin's IndexedDB, so script that does get to run
 * on any AMP page could open the store and sign with it directly. The baseline CSP is
 * what keeps such script from running; the real fix is a step-up for prompts (R-S3,
 * WebAuthn user verification), tracked for a later milestone (protocol.md 15).
 */
import type { RemoteCrypto } from './crypto'
import { SESSION_PATH_PREFIX } from './csp'

/** Captured when the bundle first runs, before the router can change the URL. */
const pathAtStartup = typeof window !== 'undefined' ? window.location.pathname : ''

/** The path this document was loaded at (its last full page load). */
export function documentLoadPath(): string {
  try {
    const nav = performance.getEntriesByType?.('navigation')[0] as PerformanceNavigationTiming | undefined
    if (nav?.name) return new URL(nav.name).pathname
  } catch { /* fall back to the startup path */ }
  return pathAtStartup
}

/** True when this document was loaded in full at /sessions/… and is still on it. */
export function onSessionPage(current: string = window.location.pathname, loaded: string = documentLoadPath()): boolean {
  return loaded.startsWith(SESSION_PATH_PREFIX) && current.startsWith(SESSION_PATH_PREFIX)
}

export class KeyGateError extends Error {
  constructor() {
    super('The device key works only on a session page opened directly. Reload this page.')
    this.name = 'KeyGateError'
  }
}

/**
 * Wraps a RemoteCrypto so the operations that use or create the private key, or write
 * trust (signCommand, signPairRequest, ensureDevice, pinHarness), refuse unless
 * `allowed()`. Reading pins, the device id and verifying events stay open.
 */
export function sessionPageOnly(inner: RemoteCrypto, allowed: () => boolean = () => onSessionPage()): RemoteCrypto {
  const gate = <A extends unknown[], R>(fn: (...a: A) => Promise<R>) => async (...a: A): Promise<R> => {
    if (!allowed()) throw new KeyGateError()
    return fn(...a)
  }
  return {
    harnessPin: id => inner.harnessPin(id),
    deviceId: () => inner.deviceId(),
    verifyEvent: (env, pin) => inner.verifyEvent(env, pin),
    signCommand: gate(cmd => inner.signCommand(cmd)),
    ensureDevice: gate(() => inner.ensureDevice()),
    signPairRequest: gate(req => inner.signPairRequest(req)),
    pinHarness: gate(pin => inner.pinHarness(pin)),
  }
}
