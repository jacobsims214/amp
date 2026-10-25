import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BASELINE_CSP, cspFor, SESSION_CSP } from './csp'

const parse = (csp: string) => Object.fromEntries(csp.split('; ').map(d => { const [k, ...v] = d.split(' '); return [k, v] }))

/** The body of one nginx `location <prefix> { … }` block (no nested braces in ours). */
function location(conf: string, prefix: string): string {
  const start = conf.indexOf(`location ${prefix} {`)
  expect(start, `nginx.conf has location ${prefix}`).toBeGreaterThanOrEqual(0)
  return conf.slice(start, conf.indexOf('\n  }', start))
}

describe('session page CSP (R-M10)', () => {
  const directives = parse(SESSION_CSP)

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
    const block = location(conf, '/sessions/')
    expect(block).toContain(`add_header Content-Security-Policy "${SESSION_CSP}" always;`)
    // Served inside the location, so the header isn't lost to an internal redirect.
    expect(block).toMatch(/try_files \/index\.html =404;/)
    expect(readFileSync(resolve(process.cwd(), 'Dockerfile'), 'utf8')).toContain('COPY ui/nginx.conf /etc/nginx/conf.d/default.conf')
  })
})

describe('baseline CSP for every other page (#2381 M6)', () => {
  const directives = parse(BASELINE_CSP)

  it('lets no injected script run', () => {
    expect(directives['script-src']).toEqual(["'self'"])
    expect(directives['object-src']).toEqual(["'none'"])
    expect(directives['base-uri']).toEqual(["'none'"])
    expect(directives['frame-ancestors']).toEqual(["'none'"])
    expect(directives['form-action']).toEqual(["'self'"])
    expect(directives['connect-src']).toEqual(["'self'"])
    expect(BASELINE_CSP).not.toMatch(/unsafe-(inline|eval|hashes)|\*|https?:\s|blob:/)
  })

  it('is what nginx serves on every page outside /sessions/, and not on /api/', () => {
    const conf = readFileSync(resolve(process.cwd(), 'nginx.conf'), 'utf8')
    const block = location(conf, '/')
    expect(block).toContain(`add_header Content-Security-Policy "${BASELINE_CSP}" always;`)
    // The SPA fallback stays in this location, so index.html served for /board keeps the header.
    expect(block).toMatch(/try_files \$uri \$uri\/ \/index\.html;/)
    // No server-level add_header: it would be dropped wherever a location adds its own.
    expect(conf.slice(0, conf.indexOf('location '))).not.toContain('add_header')
    // The API (artifact previews among it) sets its own headers.
    expect(location(conf, '/api/')).not.toContain('Content-Security-Policy')
  })

  it('picks the policy by path', () => {
    expect(cspFor('/sessions/rs_abc')).toBe(SESSION_CSP)
    expect(cspFor('/sessions')).toBe(BASELINE_CSP) // the list page signs nothing
    expect(cspFor('/board')).toBe(BASELINE_CSP)
    expect(cspFor('/')).toBe(BASELINE_CSP)
  })
})

/**
 * The strict CSP only covers a full page load of /sessions/…, so the app must never
 * navigate there client-side (a router <Link>/navigate keeps the baseline page's
 * policy). Links to a session page are plain <a href>.
 */
describe('no client-side navigation to a session page (#2381 M6)', () => {
  const files = (dir: string): string[] => readdirSync(dir).flatMap(n => {
    const p = join(dir, n)
    return statSync(p).isDirectory() ? files(p) : /\.(tsx?|jsx?)$/.test(n) && !/\.test\./.test(n) ? [p] : []
  })
  const src = resolve(process.cwd(), 'src')

  it('has no <Link>/<NavLink>/navigate()/redirect() to /sessions/<id>', () => {
    const bad = /(<(Nav)?Link\b[^>]*\bto=\{?\s*[`'"]\/sessions\/)|(\b(navigate|redirect|useNavigate\(\))\s*\(\s*[`'"]\/sessions\/)/
    const offenders = files(src).filter(f => bad.test(readFileSync(f, 'utf8')))
    expect(offenders).toEqual([])
  })

  it('catches one if added', () => {
    const bad = /(<(Nav)?Link\b[^>]*\bto=\{?\s*[`'"]\/sessions\/)|(\b(navigate|redirect|useNavigate\(\))\s*\(\s*[`'"]\/sessions\/)/
    expect(bad.test('<Link to={`/sessions/${id}`}>')).toBe(true)
    expect(bad.test("navigate('/sessions/' + id)")).toBe(true)
    expect(bad.test('<Link to="/sessions">')).toBe(false)
    expect(bad.test('<a href={`/sessions/${id}`}>')).toBe(false)
  })
})
