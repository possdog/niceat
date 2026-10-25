# AGENTS.md — niceat frontend

The registration form for niceat. Svelte 5 (runes) + Bulma v1 + Vite 8,
built with `vite-plugin-singlefile` into a **single `index.html`** with all
JavaScript and CSS inlined — the Go server embeds exactly that one file and
serves it on every managed domain. Do not add SvelteKit, SSR, multiple entry
points, or anything that emits an external asset reference: it would 404 in
production. No SvelteKit also means no server routes; the backend is the Go
server.

## Commands

- `pnpm dev` — Vite dev server with `/register` proxied to a locally running
  Go server (`http://127.0.0.1:8080`, Host forced to `wagg.ing`; the dev
  `niceat.json` must list that domain).
- `pnpm build` — produces the single-file build. The output lands at
  `../cmd/niceat/web/index.html` (not `dist/`) so the Go package's
  `//go:embed` can reach it; see vite.config.ts. The artifact is a build
  product, gitignored via `../cmd/niceat/.gitignore` — never commit it.
  Any `go build`/`go test` needs it present, so build before working on
  the Go side; the deploy image runs this same step in a Node stage.
- `pnpm check` — svelte-check + tsc.

Package manager: pnpm.

## Styling

**Always avoid writing custom CSS unless Bulma is simply not up to the
task.** When an exception is unavoidable, keep it narrowly scoped (one
component or one utility) and comment why.

## Architecture notes

- The domain parameterization is client-side: the app reads
  `location.hostname` to show the community domain and posts same-origin, so
  the request's Host header carries the domain selection. There is no
  server-side per-Host markup.
- API contract: `POST /register` with `{"handle": "<label>", "did": "..."}`;
  success `{status: "created"|"registered", handle}`; every error
  `{"detail": "..."}` — 4xx details are written for display to the user.
