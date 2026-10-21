import { svelte } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// The build must produce a single self-contained dist/index.html: the Go
// server embeds exactly that file and serves it on every managed domain,
// so any external asset reference would 404 in production.
export default defineConfig({
  plugins: [svelte(), viteSingleFile()],
  // The artifact lands inside the Go package so go:embed can pick it up
  // (embed patterns cannot reach parent directories).  emptyOutDir is set
  // explicitly because Vite refuses to empty a directory outside its
  // root unless told to.
  build: {
    outDir: '../cmd/niceat/web',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // Dev convenience: forward API posts to the Go server.  The Host
      // header is forced to a managed domain because the Go side selects
      // the registration domain by Host; vite's dev origin (localhost:5173)
      // would otherwise be rejected with 421.  The dev niceat.json must
      // list this same domain.
      '/register': {
        target: 'http://127.0.0.1:8080',
        headers: { Host: 'wagg.ing' },
      },
    },
  },
})
