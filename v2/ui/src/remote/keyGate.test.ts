import { describe, expect, it } from 'vitest'
import { KeyGateError, onSessionPage, sessionPageOnly } from './keyGate'
import { memoryStore } from './deviceStore'
import { webRemoteCrypto } from './webCrypto'
import { buildCommand } from './envelope'

describe('device key gate (#2381 M6)', () => {
  it('allows only a document loaded in full at /sessions/… that is still there', () => {
    expect(onSessionPage('/sessions/rs_x', '/sessions/rs_x')).toBe(true)
    expect(onSessionPage('/sessions/rs_x', '/board')).toBe(false) // client-side navigation from another page
    expect(onSessionPage('/board', '/sessions/rs_x')).toBe(false) // navigated away
    expect(onSessionPage('/sessions', '/sessions')).toBe(false) // the list page
  })

  it('refuses the private-key operations off the session page, and allows them on it', async () => {
    let allowed = false
    const c = sessionPageOnly(webRemoteCrypto(memoryStore()), () => allowed)
    await expect(c.ensureDevice()).rejects.toBeInstanceOf(KeyGateError)
    await expect(c.signPairRequest({} as never)).rejects.toBeInstanceOf(KeyGateError)
    await expect(c.pinHarness({ harness_id: 'h', harness_pub: 'p', host: 'x', pinned_until_ms: 1 })).rejects.toBeInstanceOf(KeyGateError)
    expect(await c.deviceId()).toBeNull() // reads stay open
    expect(await c.harnessPin('h')).toBeNull()

    allowed = true
    const dev = await c.ensureDevice()
    const cmd = buildCommand({ session_id: 's', harness_id: 'h', device_id: dev.device_id, seq: 1, ts: 1, op: 'stop', body: {} })
    expect((await c.signCommand(cmd)).sig).toMatch(/^[A-Za-z0-9_-]{86}$/)

    allowed = false
    await expect(c.signCommand(cmd)).rejects.toBeInstanceOf(KeyGateError)
    expect(await c.deviceId()).toBe(dev.device_id)
  })
})
