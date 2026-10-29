import { readFileSync } from 'node:fs'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { defineConfig, type Plugin } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// The favicon must reach the page as a data URI: vite-plugin-singlefile
// inlines JS and CSS but emits anything referenced by index.html (like a
// <link rel="icon">) as an external file, which the Go server cannot serve
// — it embeds and serves only index.html, so the file would 404 in
// production.  This plugin base64-injects the link instead; because it is
// a transformIndexHtml hook it also runs in `pnpm dev`, so the dev server
// shows the icon too.  (The PNG lives in src/assets — Vite's home for
// build-processed assets — not public/, which would mean "copy verbatim
// as an external file".)
function inlineFavicon(): Plugin {
  return {
    name: 'inline-favicon',
    transformIndexHtml() {
      const png = readFileSync(new URL('./src/assets/favicon.png', import.meta.url))
      return [
        {
          tag: 'link' as const,
          attrs: {
            rel: 'icon',
            type: 'image/png',
            href: `data:image/png;base64,${png.toString('base64')}`,
          },
          injectTo: 'head-prepend',
        },
      ]
    },
  }
}

// The build must produce a single self-contained dist/index.html: the Go
// server embeds exactly that file and serves it on every managed domain,
// so any external asset reference would 404 in production.
export default defineConfig({
  plugins: [inlineFavicon(), svelte(), viteSingleFile()],
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
