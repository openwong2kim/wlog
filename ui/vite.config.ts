import { defineConfig } from 'vite';

// Embedded static UI build. Output goes straight into the Go embed dir.
// base: './' so asset URLs are relative (served from go:embed FileServer root).
export default defineConfig({
  base: './',
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    sourcemap: false,
    minify: 'esbuild',
    target: 'es2020',
  },
});
