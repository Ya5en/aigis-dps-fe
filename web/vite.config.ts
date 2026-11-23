import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发期：/api 与 /events 反代到本地 Go 桥
export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:9832',
      '/events': {
        target: 'http://127.0.0.1:9832',
        // SSE 不缓冲
        configure: (proxy) => {
          proxy.on('proxyRes', (res) => {
            res.headers['x-accel-buffering'] = 'no'
          })
        },
      },
    },
  },
  build: {
    target: 'es2020',
    emptyOutDir: true,
  },
})
