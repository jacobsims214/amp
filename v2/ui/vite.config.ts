import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

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
  plugins: [publicHomePages(), react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      // AMP_API_TARGET points the dev server at another AMP (default: one on :3001).
      '/api': { target: process.env.AMP_API_TARGET || 'http://localhost:3001', changeOrigin: true },
    },
  },
})
