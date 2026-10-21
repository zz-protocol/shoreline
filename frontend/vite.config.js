import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: { proxy: { '/api': 'http://127.0.0.1:8080', '/healthz': 'http://127.0.0.1:8080', '/mcp': 'http://127.0.0.1:8080' } },
  preview: { proxy: { '/api': 'http://127.0.0.1:8080', '/healthz': 'http://127.0.0.1:8080', '/mcp': 'http://127.0.0.1:8080' } },
  test: { environment: 'jsdom', globals: true },
  build: { chunkSizeWarningLimit: 2000 },
  optimizeDeps: { include: ['swagger-ui-dist/swagger-ui-es-bundle.js'] },
})
