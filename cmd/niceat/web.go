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
// artifact is committed so a plain Go checkout builds without a Node
// toolchain; regenerate it with `pnpm build` inside frontend/.

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
	// Everything is inlined into this one file, so a cached copy can only
	// be refreshed by revalidating; no-cache makes deploys visible without
	// a separate asset-URL versioning scheme.
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := w.Write(indexHTML); err != nil {
		log.Printf("ERROR: writing index page: %v", err)
	}
}
