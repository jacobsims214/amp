/// <reference types="vitest/config" />
import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { cspFor } from './src/remote/csp'

/** The static home pages (public/home/**) and their asset URLs. */
const isHomePage = (path: string) => path === '/home' || path.startsWith('/home/')

/**
 * The pages' CSPs, matching nginx in production: the strict one on /sessions/, the
 * baseline everywhere else, none on the proxied /api/.
 *
 * Under `vite preview` it covers every page. Under `vite dev` it covers only the static
 * /home/ pages: they are plain HTML with no dev client, while the app's own index.html gets
 * Vite's HMR client, which injects inline script and styles. Without this the home pages
 * were the one thing dev never tested against the CSP they ship under (#2968).
 */
function pageCsp(): Plugin {
  const apply = (path: string, res: { setHeader(name: string, value: string): void }) => {
    if (!path.startsWith('/api/')) res.setHeader('Content-Security-Policy', cspFor(path))
  }
  return {
    name: 'amp-page-csp',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const path = req.url ?? '/'
        if (isHomePage(path.split('?')[0])) apply(path, res)
        next()
      })
    },
    configurePreviewServer(server) {
      server.middlewares.use((req, res, next) => {
        apply(req.url ?? '/', res)
        next()
      })
    },
  }
}

// The public home pages are static folders (public/home/, public/home/harness/). nginx serves a
// folder's index.html for /home/ and /home/harness/, and redirects /home/harness to the slash;
// the dev server would hand those URLs to the app instead, so do the same here.
function publicHomePages(): Plugin {
  return {
    name: 'amp-public-home-pages',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const [path, query = ''] = (req.url ?? '').split('?')
        if (!/^\/home(\/[a-z0-9-]+)*\/?$/.test(path)) return next()
        const dir = path.endsWith('/') ? path : `${path}/`
        if (!existsSync(join(server.config.publicDir, dir, 'index.html'))) return next()
        if (dir !== path) {
          res.statusCode = 301
          res.setHeader('Location', dir + (query ? `?${query}` : ''))
          return res.end()
        }
        req.url = `${dir}index.html${query ? `?${query}` : ''}`
        next()
      })
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss(), pageCsp(), publicHomePages()],
  server: {
    port: 5173,
    proxy: {
      // AMP_API_TARGET points the dev server at another AMP (default: one on :3001).
      '/api': { target: process.env.AMP_API_TARGET || 'http://localhost:3001', changeOrigin: true },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    restoreMocks: true,
  },
})
