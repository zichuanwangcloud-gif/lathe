import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 构建产物由 Go 用 go:embed 打进二进制（见 internal/webui）。
// dev 模式下把 /api 代理到本地控制面，前端可独立热重载。
export default defineConfig({
  plugins: [vue()],
  build: { outDir: 'dist', emptyOutDir: true },
  server: {
    proxy: { '/api': 'http://127.0.0.1:8200' },
  },
})
