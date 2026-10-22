import { defineConfig } from 'vite'

// Output goes into the Go package so `go build` embeds it without Node.
export default defineConfig({
  esbuild: { jsx: 'automatic', jsxImportSource: 'preact' },
  build: {
    outDir: '../internal/ui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    target: 'es2022',
    modulePreload: { polyfill: false },
    reportCompressedSize: true,
    minify: process.env.NOMINIFY ? false : 'esbuild',
  },
  server: {
    port: 5173,
    proxy: { '/api': { target: 'http://127.0.0.1:7271', changeOrigin: false } },
  },
})
