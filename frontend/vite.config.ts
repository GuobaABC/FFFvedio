import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

import pkg from './package.json'

// 界面版本号直接取 package.json：发版只改一处，侧边栏不会再落后于实际版本
export default defineConfig({
  plugins: [vue()],
  define: {
    __APP_VERSION__: JSON.stringify(`v${pkg.version}`),
  },
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
