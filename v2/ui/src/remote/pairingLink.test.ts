import { afterEach, describe, expect, it, vi } from 'vitest'
import { acceptPairingLink, capturePairingFragment, forgetPairingLink, pairingLinkFor } from './pairingLink'
import { loadVectors } from '../test/vectors'

const vec = loadVectors<{ link: string; inputs: { pairing_secret_b64u: string; harness_id: string; session_id: string } }>('pairing.json')
const AMP = 'https://amp.example.test'

function at(href: string) {
  const u = new URL(href)
  return { pathname: u.pathname, hash: u.hash, search: u.search }
}

afterEach(() => forgetPairingLink())

describe('pairing link fragment (protocol 7.1)', () => {
  it('reads p and h, then replaces the URL with the bare path', () => {
    const hist = { replaceState: vi.fn() }
    expect(capturePairingFragment(at(vec.link), hist)).toBe(true)
    expect(hist.replaceState).toHaveBeenCalledWith(null, '', `/sessions/${vec.inputs.session_id}`)
    const link = pairingLinkFor(vec.inputs.session_id)!
    expect(link.h).toBe(vec.inputs.harness_id)
    expect(link.secret).toHaveLength(32)
    expect(pairingLinkFor('rs_ZZZZZZZZZZZZZZZZZZZZZZ')).toBeNull() // only for its own session
  })

  it('scrubs any fragment or query on a session URL, usable or not', () => {
    for (const bad of ['#p=short&h=x', '#h=KcrmtQDCZstbc69koLwXJWQvwvQbZAufZBMxCffV6DY', '#p=AQ==&h=KcrmtQDCZstbc69koLwXJWQvwvQbZAufZBMxCffV6DY', '?next=https://evil.example#x']) {
      const hist = { replaceState: vi.fn() }
      expect(capturePairingFragment(at(`https://amp.example.test/sessions/${vec.inputs.session_id}${bad}`), hist)).toBe(false)
      expect(hist.replaceState).toHaveBeenCalledWith(null, '', `/sessions/${vec.inputs.session_id}`)
      expect(pairingLinkFor(vec.inputs.session_id)).toBeNull()
    }
  })

  it('leaves other pages alone', () => {
    const hist = { replaceState: vi.fn() }
    expect(capturePairingFragment(at('https://amp.example.test/projects/2#p=x&h=y'), hist)).toBe(false)
    expect(capturePairingFragment(at(`https://amp.example.test/sessions/${vec.inputs.session_id}`), hist)).toBe(false)
    expect(hist.replaceState).not.toHaveBeenCalled()
  })

  it('forgets (and zeroes) the secret', () => {
    capturePairingFragment(at(vec.link), { replaceState: () => {} })
    const link = pairingLinkFor(vec.inputs.session_id)!
    forgetPairingLink()
    expect(pairingLinkFor(vec.inputs.session_id)).toBeNull()
    expect(link.secret.every(b => b === 0)).toBe(true)
  })

  it('runs against the real window history in the browser', () => {
    window.history.replaceState(null, '', `/sessions/${vec.inputs.session_id}#p=${vec.inputs.pairing_secret_b64u}&h=${vec.inputs.harness_id}`)
    expect(capturePairingFragment()).toBe(true)
    expect(window.location.hash).toBe('')
    expect(window.location.href).not.toContain(vec.inputs.pairing_secret_b64u)
  })
})

/** #4902: the same link, entered by hand because an installed app is never handed it as a page load. */
describe('entering a pairing link by hand (#4902)', () => {
  it('accepts a pasted link with the fragment capture\'s own validation', () => {
    // The same vector link the page-load path accepts, so both entries agree byte for byte.
    expect(acceptPairingLink(vec.link, vec.inputs.session_id, AMP)).toEqual({ kind: 'link', link: expect.anything() })
    const link = pairingLinkFor(vec.inputs.session_id)!
    expect(link).not.toBeNull()
    expect(link.h).toBe(vec.inputs.harness_id)
    expect(link.secret).toHaveLength(32)
    expect(link.session_id).toBe(vec.inputs.session_id)
  })

  it('finds the link in text around it, and without the scheme, as a terminal hands it over', () => {
    for (const pasted of [
      `Open this on your phone: ${vec.link}`, // the harness prints words around it
      `  ${vec.link}\n`, // pasted with whitespace
      vec.link.replace('https://', ''), // a wrap that lost the scheme
    ]) {
      forgetPairingLink()
      expect(acceptPairingLink(pasted, vec.inputs.session_id, AMP).kind).toBe('link')
      expect(pairingLinkFor(vec.inputs.session_id)).not.toBeNull()
    }
  })

  it('refuses what the page-load path refuses, with words that say what to fix', () => {
    const cases: { pasted: string; says: RegExp }[] = [
      { pasted: 'hello', says: /doesn't look like a pairing link/ },
      { pasted: '', says: /doesn't look like a pairing link/ },
      { pasted: `${AMP}/projects/2#p=x&h=y`, says: /doesn't look like a pairing link/ },
      { pasted: `${AMP}/sessions/${vec.inputs.session_id}`, says: /has no pairing secret/ },
      { pasted: `${AMP}/sessions/${vec.inputs.session_id}#p=${vec.inputs.pairing_secret_b64u}`, says: /is missing the laptop key/ }, // no h
      { pasted: `${AMP}/sessions/${vec.inputs.session_id}#p=short&h=${vec.inputs.harness_id}`, says: /secret in that link isn't valid/ },
      // Right alphabet, wrong length; and padded, which the protocol refuses as non-canonical.
      { pasted: `${AMP}/sessions/${vec.inputs.session_id}#p=AQ==&h=${vec.inputs.harness_id}`, says: /secret in that link isn't valid/ },
      { pasted: `${AMP}/sessions/${vec.inputs.session_id}#p=${vec.inputs.pairing_secret_b64u}&h=x`, says: /is missing the laptop key/ }, // bad fp
    ]
    for (const c of cases) {
      const result = acceptPairingLink(c.pasted, vec.inputs.session_id, AMP)
      expect(result.kind).toBe('refused')
      expect(result.kind === 'refused' ? result.message : '').toMatch(c.says)
      expect(pairingLinkFor(vec.inputs.session_id)).toBeNull() // nothing held, nothing stored
    }
  })

  it('refuses a link for another AMP rather than following it off this server', () => {
    const other = vec.link.replace(AMP, 'https://evil.example')
    const result = acceptPairingLink(other, vec.inputs.session_id, AMP)
    expect(result).toEqual({ kind: 'refused', message: expect.stringContaining('another AMP server') })
    expect(pairingLinkFor(vec.inputs.session_id)).toBeNull()
  })

  it('accepts this AMP\'s own session path pasted without a host', () => {
    // A person can paste the path as the address bar shows it, or as the laptop printed it; both
    // must work on the page they are on.
    const result = acceptPairingLink(`${at(vec.link).pathname}${at(vec.link).hash}`, vec.inputs.session_id, AMP)
    expect(result.kind).toBe('link')
    expect(pairingLinkFor(vec.inputs.session_id)).not.toBeNull()
  })

  it('hands back another session\'s own URL on this AMP, for a page load to capture there', () => {
    const otherSession = 'rs_ZZZZZZZZZZZZZZZZZZZZZZ'
    const result = acceptPairingLink(vec.link.replace(vec.inputs.session_id, otherSession), vec.inputs.session_id, AMP)
    expect(result).toEqual({ kind: 'other_session', href: `/sessions/${otherSession}#p=${vec.inputs.pairing_secret_b64u}&h=${vec.inputs.harness_id}` })
    // Not held here: only a page load at that URL captures it, as a scan in a browser does.
    expect(pairingLinkFor(vec.inputs.session_id)).toBeNull()
    expect(pairingLinkFor(otherSession)).toBeNull()
  })

  it('replaces an earlier secret and zeroes it, like the page-load path', () => {
    expect(acceptPairingLink(vec.link, vec.inputs.session_id, AMP).kind).toBe('link')
    const first = pairingLinkFor(vec.inputs.session_id)!
    capturePairingFragment(at(vec.link), { replaceState: () => {} })
    expect(first.secret.every(b => b === 0)).toBe(true)
    expect(pairingLinkFor(vec.inputs.session_id)).not.toBeNull()
  })
})

