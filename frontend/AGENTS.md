# AGENTS.md — niceat frontend

The registration form for niceat. Svelte 5 (runes) + Blades CSS + Vite 8,
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

**Blades CSS (`@anyblades/blades`) is classless**, so express intent with
the right element, role, and ARIA state — `<button type="submit">`,
`aria-busy="true"` for a loading button, `aria-invalid` for a rejected
field — instead of adding classes. Its vocabulary is Pico CSS's (Blades is
Pico's maintained successor), so Pico's docs apply as-is. Light and dark
mode come from Pico's `light-dark()` palette: never write a
`prefers-color-scheme` block, override `--pico-*` variables at most. The
minified build is imported once, in `src/main.ts`; the package's exports map
resolves `.` to a stylesheet, which TypeScript cannot type, hence the
explicit `css/blades.min.css` path.

**Always avoid writing custom CSS unless Blades is simply not up to the
task.** When an exception is unavoidable, keep it narrowly scoped (one
component or one utility) and comment why.  Custom CSS lives in
`src/custom.css`, imported in `src/main.ts` right after Blades so overrides
win at equal specificity; `vite-plugin-singlefile` inlines it into the
single `index.html` like any other CSS import. It holds exactly three
exceptions today: `.narrow` (Blades' `.container` is document-sized, up to
1200px, and `blades.css` omits Pico's landmark padding), the
`.result-ok`/`.result-error` accent (Blades ships no alert component), and
the `.handle-field` rules that let the handle field display the composed
handle while keeping the domain uneditable (see below).

The handle field's suffix is a special case worth knowing about: the field
must *display* the composed handle (`label.domain`) exactly as Bluesky will
show it, but only the label may be editable, so the input holds the label
alone with transparent glyphs and `.handle-fill` draws `label.domain` beneath
it — the caret, selection and typing stay inside the label and the domain is
unreachable with the cursor.  When the field is empty the overlay draws a
sample name (`yourname`) in Pico's placeholder color instead, so an untouched
field still reads as a whole handle; like a native placeholder it yields on
focus, leaving the caret directly before the suffix.  Two invariants keep
that honest: the overlay
and the input must share text metrics exactly (hence the shared font/height
block in `custom.css`), and the input's `maxlength` is the label budget left
by Bluesky's 30-character cap, so the composed string always fits the field
instead of scrolling away from the overlay.

## Architecture notes

- The domain parameterization is client-side: the app reads
  `location.hostname` to show the community domain and posts same-origin, so
  the request's Host header carries the domain selection. There is no
  server-side per-Host markup.
- API contract: `POST /register` with `{"handle": "<label>", "did": "..."}`;
  success `{status: "created"|"registered", handle}`; every error
  `{"detail": "..."}` — 4xx details are written for display to the user.
