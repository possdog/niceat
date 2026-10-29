package main

import (
	_ "embed"
	"log"
	"net/http"
)

// The frontend is a Svelte app (frontend/) that Vite builds with
// vite-plugin-singlefile into one self-contained page — all JavaScript and
// CSS inlined — with its outDir pointed here (see frontend/vite.config.ts)
// so go:embed, which cannot reach parent directories, can pick it up.  The
// artifact is a build product, deliberately not committed (see
// cmd/niceat/.gitignore): build it with `pnpm build` inside frontend/
// before `go build` / `go test` — go:embed fails loudly when it is
// missing — and the deploy image builds it in its own Node stage
// (see Dockerfile).

//go:embed web/index.html
var indexHTML []byte

// serveIndex writes the embedded registration form page.  It is served
// unchanged on every managed domain: the page is a client-side app that
// reads location.hostname to show the community domain and posts
// same-origin, so the server needs no per-Host markup parameterization.
//
// Like /register, the page only answers managed Hosts.  On an unmanaged
// host the form could never succeed (every post would 421), so serving it
// would only mislead — the same 421 keeps behavior uniform.
func (a *app) serveIndex(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.resolveDomain(r.Host); !ok {
		writeJSON(w, http.StatusMisdirectedRequest, errorResponse{Detail: "This host is not available for handle registration"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Defense in depth (audit INFO-8): the page has no XSS sink today, but a
	// CSP bounds the blast radius if one is ever introduced.  Directives are
	// derived from the page's actual needs: the single-file build inlines the
	// bundle's script and styles into this document, so script-src and
	// style-src must allow 'unsafe-inline' (the only alternative is a build
	// change emitting a nonce-able external file, which the embed setup rules
	// out); the form posts same-origin via fetch("/register"), so
	// connect-src 'self'.  The page loads no images, fonts, frames, or other
	// subresources, hence default-src 'none' for everything else.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; base-uri 'none'; form-action 'self'; "+
			"frame-ancestors 'none'; object-src 'none'; "+
			"script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	// Browsers must not MIME-sniff the page away from the declared
	// text/html (e.g. into an executable type on a crafted response).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Everything is inlined into this one file, so a cached copy can only
	// be refreshed by revalidating; no-cache makes deploys visible without
	// a separate asset-URL versioning scheme.
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := w.Write(indexHTML); err != nil {
		log.Printf("ERROR: writing index page: %v", err)
	}
}
