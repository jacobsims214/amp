import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { SESSION_CSP } from './csp'

describe('session page CSP (R-M10)', () => {
  const directives = Object.fromEntries(SESSION_CSP.split('; ').map(d => { const [k, ...v] = d.split(' '); return [k, v] }))

  it('is strict', () => {
    expect(directives['default-src']).toEqual(["'none'"])
    expect(directives['script-src']).toEqual(["'self'"]) // no inline script, no eval
    expect(directives['connect-src']).toEqual(["'self'"]) // same-origin relay only (T-UI-8)
    expect(directives['img-src']).toEqual(["'self'", 'data:']) // no remote images
    expect(directives['frame-ancestors']).toEqual(["'none'"])
    expect(directives['require-trusted-types-for']).toEqual(["'script'"])
    expect(SESSION_CSP).not.toMatch(/unsafe-(inline|eval)/)
  })

  it('is what nginx serves on /sessions/', () => {
    const conf = readFileSync(resolve(process.cwd(), 'nginx.conf'), 'utf8')
    const block = conf.slice(conf.indexOf('location /sessions/'))
    expect(block).toContain(`add_header Content-Security-Policy "${SESSION_CSP}" always;`)
    // Served inside the location, so the header isn't lost to an internal redirect.
    expect(block).toMatch(/try_files \/index\.html =404;/)
    expect(readFileSync(resolve(process.cwd(), 'Dockerfile'), 'utf8')).toContain('COPY ui/nginx.conf /etc/nginx/conf.d/default.conf')
  })
})
