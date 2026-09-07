import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // Dev loop: vite on :5173 proxies API calls to the Go binary on :8080.
      '/api': 'http://localhost:8080',
    },
  },
})
