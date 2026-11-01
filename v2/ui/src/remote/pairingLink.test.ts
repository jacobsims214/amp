import { afterEach, describe, expect, it, vi } from 'vitest'
import { capturePairingFragment, forgetPairingLink, pairingLinkFor } from './pairingLink'
import { loadVectors } from '../test/vectors'

const vec = loadVectors<{ link: string; inputs: { pairing_secret_b64u: string; harness_id: string; session_id: string } }>('pairing.json')

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
