import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 前端既可以单独运行（npm run dev），也可以由 Wails 内嵌加载（wails dev / wails build）
export default defineConfig({
  plugins: [vue()],
  // 桌面端通过本地资源服务器加载，相对路径在开发与内嵌两种模式下都可用
  base: './',
  server: {
    port: 5173,
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // WebView2（Windows）与 WKWebView（macOS）均为现代内核
    target: 'chrome105',
    sourcemap: false,
  },
})
