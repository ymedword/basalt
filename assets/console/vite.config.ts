// 管理台构建配置：资源基路径固定 /console/，产物写入 dist/，不引入任何 CDN 外链。
// Build config for the embedded console: base path is fixed to /console/ and the
// bundle is written to dist/, with no CDN references.
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  base: '/console/',
  plugins: [vue()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
    target: 'es2022',
  },
  test: {
    environment: 'jsdom',
    include: ['tests/**/*.test.ts'],
  },
})
