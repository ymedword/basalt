// 管理台构建配置：资源基路径固定 /console/，产物写入 dist/，不引入任何 CDN 外链。
// Build config for the embedded console: base path is fixed to /console/ and the
// bundle is written to dist/, with no CDN references.
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  base: '/console/',
  plugins: [vue()],
  // 只额外开放净化的公开素材目录（含 ?raw 变体），不暴露仓库或实验控制目录。
  server: { fs: { allow: ['.', '../../examples/workflows/bid-readiness/defaults/materials'] } },
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
    // 并发上限固定为 4：`/mnt/d`（DrvFs）页缓存被打冷时（长时全量运行之后即是如此），
    // 一次性拉起约 19 个 fork 会让个别 worker 的启动握手超过 vitest 固定的 60s 超时
    // （START_TIMEOUT，`poolOptions` 不可调）。2026-10-30 两轮全量验收因此红，冷缓存
    // 对照实验复现；上限 4 在同样冷缓存条件下 29 文件全绿。断言、阈值与用例集零改动。
    // Cap workers at 4: when the DrvFs page cache is cold, spawning ~19 forks at once
    // pushes some worker start handshakes past vitest's fixed 60s timeout (START_TIMEOUT,
    // not configurable via poolOptions). Cap 4 stays green under the same cold cache.
    maxWorkers: 4,
  },
})
