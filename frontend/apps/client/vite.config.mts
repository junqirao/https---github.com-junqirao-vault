import { readFileSync } from 'node:fs'
import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

/**
 * 构建期把 package.json 的版本号注入为全局常量 `__APP_VERSION__`。
 *
 * 这样界面显示的版本、上报给服务端做兼容性判定的版本、以及发布包版本三者同源，
 * 不再需要手工同步（build.py 递增版本时只改 package.json 与 VERSION）。
 */
const pkg = JSON.parse(
  readFileSync(fileURLToPath(new URL('./package.json', import.meta.url)), 'utf8')
) as { version: string }

export default defineConfig({
  plugins: [react()],
  base: './',
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version)
  },
  resolve: {
    alias: {
      '@vault/ui': fileURLToPath(new URL('../../packages/ui/src/index.ts', import.meta.url)),
      '@vault/ui/': fileURLToPath(new URL('../../packages/ui/src/', import.meta.url))
    }
  },
  server: {
    port: 5173,
    strictPort: true
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 2000
  }
})
