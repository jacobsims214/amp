/// <reference types="vitest/config" />
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { cspFor } from './src/remote/csp'

/**
 * The pages' CSPs under `vite preview`, matching nginx in production: the strict
 * one on /sessions/, the baseline everywhere else, none on the proxied /api/.
 * Not under `vite dev`: its HMR client injects inline script and styles.
 */
function pageCsp(): Plugin {
  return {
    name: 'amp-page-csp',
    configurePreviewServer(server) {
      server.middlewares.use((req, res, next) => {
        const path = req.url ?? '/'
        if (!path.startsWith('/api/')) res.setHeader('Content-Security-Policy', cspFor(path))
        next()
      })
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss(), pageCsp()],
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
