/// <reference types="vitest/config" />
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { SESSION_CSP, SESSION_PATH_PREFIX } from './src/remote/csp'

/**
 * The remote session page's strict CSP under `vite preview`, matching nginx in
 * production. Not under `vite dev`: its HMR client injects inline styles.
 */
function sessionCsp(): Plugin {
  return {
    name: 'amp-session-csp',
    configurePreviewServer(server) {
      server.middlewares.use((req, res, next) => {
        if (req.url?.startsWith(SESSION_PATH_PREFIX)) res.setHeader('Content-Security-Policy', SESSION_CSP)
        next()
      })
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss(), sessionCsp()],
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
