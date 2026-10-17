import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      // AMP_API_TARGET points the dev server at another AMP (default: one on :3001).
      '/api': { target: process.env.AMP_API_TARGET || 'http://localhost:3001', changeOrigin: true },
    },
  },
})
