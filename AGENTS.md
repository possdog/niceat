# AGENTS.md — niceat

## Project overview

niceat lets communities self-service Bluesky handles under a shared domain:
a web form collects a desired handle label plus the user's Bluesky account
DID, and this API provisions the `_atproto` TXT record that binds
`<label>.<domain>` to the DID via the Porkbun DNS API.  Go 1.26, standard
library plus `golang.org/x/time`, one Go package at `cmd/niceat/`, plus a
Svelte frontend subproject at `frontend/` whose single-file build is embedded
into the Go binary.

**DNS is the claim store.**  Whether a handle is available — and who it
belongs to — is decided entirely by the existence and content of the
`_atproto` TXT record in Porkbun.  There is deliberately no local ledger
that could desync with the reaper, manual Porkbun dashboard edits,
or zone restores.  (The reaper's `reaper_state.json` is a timing cache, not
a claim ledger — see Defenses.)

## Build & run

Configuration lives in a JSON file (JSON because the standard library
parses it natively; a persistent service should not depend on CLI flags to
remember its setup).  The path defaults to `niceat.json` in the working
directory; the file is instance-specific and gitignored:

```json
{
  "domains": ["wagg.ing", "yeen.world"],
  "addr": ":8080",
  "reaper": {"enabled": true, "interval": "6h", "grace": "720h"}
}
```

- `domains` (required, non-empty): apex domains managed by this instance.
  Entries are validated and normalized at startup (lowercased, FQDN dot
  stripped, duplicates removed) — a bad entry is a startup error naming the
  offending value.
- `addr` (optional): HTTP listen address, defaults to `:8080`.
- `reaper` (optional): the background handle reaper (see Defenses).
  `enabled` defaults to true; `interval` (default `6h`) is the time
  between sweeps; `grace` (default `720h` = 30 days) is how long a
  handle-account mismatch must persist before the record is deleted.
  Durations are Go duration strings; invalid ones fail at startup.
- Unknown fields are rejected (`DisallowUnknownFields`), so a typo like
  `"domain"` fails loudly instead of silently yielding an empty domain set.
A template with every knob at its default value lives at
`niceat.example.json` — copy it to `niceat.json` and edit.  (JSON has no
comments, so the field reference is the surrounding text here, and
comment keys like `"_comment"` would fail `DisallowUnknownFields`.)

```bash
cd frontend && pnpm build   # generate cmd/niceat/web/index.html (see below)
go build -o niceat ./cmd/niceat
./niceat                       # reads ./niceat.json, listens on config "addr"
./niceat -addr :8123           # same config, listen address overridden
```

The frontend artifact `cmd/niceat/web/index.html` is a build product,
deliberately **not committed** (gitignored via `cmd/niceat/.gitignore`):
`go build`, `go test`, and `go vet` all need it present, because `go:embed`
in web.go fails loudly when it is missing.  Generate it with `pnpm build`
in `frontend/` before any Go build; the deploy image does the same in its
own Node stage (see Dockerfile), so deploys need no local artifact.
Never edit it by hand — it is a 700 kB minified bundle.  See
frontend/AGENTS.md for the frontend's own conventions.

Only two flags exist.  `-config` defaults to `niceat.json` relative to the
current working directory, so a service manager just needs to set the
working directory.  `-addr` overrides the config file's `addr` (useful when
a host runs several services and ports are handed out dynamically, e.g.
`-addr :0`); empty means "use the config".  Precedence:
`-addr` flag > config `addr` > `:8080` default.

Porkbun credentials are **per domain** (mirroring production key scoping):
`porkbun_keys/<domain>.api_key.txt` and `porkbun_keys/<domain>.secret_key.txt`
under the working directory hold each managed domain's own key pair
(gitignored; never commit, never hard-code them into source or logs).
Pairs for every configured domain select the real Porkbun backend; with no
pairs at all, the service falls back to `memoryProvisioner`
(see provision.go) — same semantics, but nothing is published and state is
lost on restart, so it must never front real traffic.  Pairs for some
configured domains but not others, a half-present pair, or an empty key is
a startup error naming the domain: a domain whose records could never be
published must fail loudly, not silently eat registrations.  There is
deliberately no global fallback pair — a shared pair would defeat the
per-domain scoping's blast-radius containment.  Credentials can also come
from environment variables, `PORKBUN_<DOMAIN>_API_KEY` /
`PORKBUN_<DOMAIN>_SECRET_KEY` (domain uppercased, `.` and `-` → `_`; two
domains mapping to one suffix is a startup error).  Env wins over files —
that is Fly.io's secrets model (`fly secrets set`), and a stale local-dev
file must not shadow what the operator set.  Current keys are
sandbox (`pk1_sb_`/`sk1_sb_`, scoped to the sandbox account's one probe
domain): Porkbun requests are simulated, so live smoke tests are safe.

## The API (what exists today)

Two routes (wired in `newMux`, main.go; tests exercise the same mux):

- **GET /** — the embedded registration form page (see Frontend below).
  Gated on the Host like /register: an unmanaged host gets 421 instead of a
  form whose every submission would fail.
- **POST /register** — JSON `{"handle": "<label>", "did": "did:plc:..."}`.
  Returns `201 {"status":"created","handle":"<label>.<domain>"}`.

**Domain selection is by Host header, not request body.**  One deployment
manages several domains; the request's Host (lowercased, port and FQDN dot
stripped) must be one of the configured `-domain` values, and the handle is
registered under that domain.  The request body never carries a domain — the
frontend posts same-origin, so the browser's Host is the domain the user is
browsing, and a spoofed Host can only select among domains we already
manage.  A reverse proxy in front must therefore forward the original Host
(or rewrite it to the correct domain); do not honor client-supplied
`X-Forwarded-Host`.

Status codes: 201 created; 200 idempotent repeat (handle already bound to
this DID); 400 invalid handle/DID or malformed JSON; 405 non-POST (from the
mux's `POST /register` pattern); 409 handle claimed by a different DID
(worded identically to the reserved-name refusal, so probers learn nothing)
or this DID already holds another handle under the served domain (one handle
per DID per domain);
413 body over 4 KiB; 415 Content-Type not `application/json`; 421 Host not a
managed domain; 429 per-IP rate limit exceeded; 500 provisioning failure
(generic detail; backend errors are logged, not leaked).

Validation (in `validate.go`, all error messages user-displayable).  A
requested handle must be a valid Bluesky username **and** a valid DNS
subdomain — the union of both specs, verified against the AT Proto handle
spec (atproto.com/specs/handle), the reference implementation
(`@atproto/syntax` `handle.ts`), and Bluesky's official support docs
("What is the suggested user handle format?"):

- **Label** (`normalizeHandle`): trimmed and lowercased; 3-63 chars
  `[a-z0-9-]`, no leading/trailing hyphen, no dots (explicitly, so a caller
  cannot reach outside the served domain), no underscores.  The 3-char
  minimum is a niceat policy, not an upstream rule: Bluesky enforces no
  minimum for custom domains (short handles like `s.wagg.ing` work), but
  short labels are scarce and perceived as valuable, so squatting them is
  discouraged.  63 and the character class are the shared DNS-label /
  AT Proto segment rules; punycode `xn--` labels are
  valid per the spec.  Labels in `reservedLabels` (app.go) are refused:
  infrastructure names (www, mx, ns1, ...) and AT Protocol names
  (_atproto, bsky, plc, ...).  The list is meant to be over-inclusive;
  extending it is safe, recalling a claimed label is not.
- **Composed handle** (`ensureHandleClaimable`): `label + "." + domain` must
  be at most 30 characters — bsky.app's limit for custom-domain handles
  ("Handle Too Long ... Custom domain users: up to 30 characters total").
  The budget is per-domain (wagg.ing leaves 21 label chars, example.com
  leaves 18).  This is deliberately stricter than the AT Proto limits (253
  total, 244 with the `_atproto.` DNS prefix): a longer handle would be
  protocol-valid but unclaimable through the official app's UI.
- **Configured domains** (`parseDomain`): in addition to DNS shape, the
  TLD must start with a letter (AT Proto rule) and must not be one of the
  reserved TLDs (`disallowedTLDs`: .local, .arpa, .invalid, .localhost,
  .internal, .example, .alt, .onion), since no handle under them could
  ever be valid.
- **DID** accepts only `did:plc` (24 chars `[a-z2-7]`) and `did:web` (pchar
  charset), max 200 bytes.  Tight per-method regexes guarantee the DID
  cannot inject extra content into the `did=<DID>` TXT record value.  DIDs
  are case-sensitive and are never normalized.

## Architecture

```
cmd/niceat/
├── main.go        — flags, config/credential loading, mux, graceful shutdown
├── config.go      — JSON config type, strict loadConfig, managedSet
├── app.go         — app struct, labelLocks, regexes, reservedLabels, JSON types
├── handlers.go    — resolveDomain, handleRegister + writeJSON
├── healthz.go     — GET /healthz liveness probe for deploy-time checks
├── validate.go    — parseDomain, normalizeHandle, validateDID
├── ratelimit.go   — per-IP token bucket middleware
├── provision.go   — provisioner interface + outcomes, memoryProvisioner
├── porkbun.go     — Porkbun client: zone listing, per-DID cap, create, reaper primitives
├── resolver.go    — DID document resolution (plc.directory / did:web well-known)
├── reaper.go      — background task: sweep zones, grace state file, delete unused
├── web.go         — go:embed of the built form page + serveIndex
├── web/           — frontend build artifact (index.html), gitignored build product
└── *_test.go      — tests alongside source, fakes in helpers_test.go / porkbun_test.go

frontend/           — Svelte 5 + Bulma + Vite source; pnpm build emits cmd/niceat/web/index.html
```

One third-party dependency exists alongside the standard library, matching
nicelink: `golang.org/x/time/rate` (token buckets).  (bbolt was used by the
claims store and was removed with it.)

The handler only sees the `provisioner` interface
(`Provision(ctx, domain, label, did) (provisionOutcome, error)`), so the DNS
backend is swappable without touching the HTTP layer; the domain argument is
the Host-resolved zone, so the Porkbun client automatically operates on the
matching domain.  The outcome (created / registered / conflict) maps to
201/200/409 respectively.  `porkbun.go` implements the real client — one
`/dns/retrieve/{domain}` zone listing serves both the existence check and
the per-DID cap count (see Defenses), create only when the label is absent
and the DID is under the cap, with sandbox-verified semantics (an
exact-duplicate create is 400 `DUPLICATE_RECORD` with `existingId`; a
same-name/different-content create would stack a second TXT record and
break the handle, hence the pre-check).  Registration is therefore two
Porkbun calls (list + create), not one.  Tests use `fakeProvisioner`
(handler) and `fakePorkbunServer` (client).

Because registration is a check-then-create over two Porkbun calls, the
handler serializes it per (domain, label) with `labelLocks` (app.go): two
concurrent requests for the same label must not both pass the "no record"
check and stack records.  The lock is process-local, which is correct only
while the deployment is a single instance.

## Frontend

`frontend/` is a Svelte 5 (runes) + Bulma v1 + Vite 8 app built with
`vite-plugin-singlefile` into one self-contained `index.html` (all JS and
CSS inlined), emitted directly into `cmd/niceat/web/` for `go:embed`.  The
Go server serves it unchanged at `/` on every managed domain:
the domain parameterization is **client-side** — the page reads
`location.hostname` to show "*a Bluesky handle under wagg.ing*" (the
equivalent on yeen.world) and posts the form same-origin, so the Host
header carries the domain selection.  The server does no per-Host markup.

The artifact is gitignored (see Build & run): `go build` and the tests need
it present, so run `pnpm build` in `frontend/` before working on the Go
side.  For frontend-specific conventions (pnpm, Bulma-over-custom-CSS, no
SvelteKit/SSR), see frontend/AGENTS.md.  In dev, `pnpm dev` proxies
`/register` to a locally running Go server with the Host forced to
`wagg.ing` (the dev niceat.json must list it).

## Defenses

- **DNS as the claim store** (`porkbun.go`) — availability and ownership are
  read from the `_atproto` TXT records in a single zone listing, and the
  record is created only when the label is absent.  A same-DID repeat is an
  idempotent 200; a different DID is refused with 409.  A multi-record
  `_atproto` name (corrupt per the atproto spec) is treated as taken, never
  stacked onto.  The check-then-create flow runs under a per-label lock
  (`labelLocks`) so concurrent requests cannot stack records.  No local
  ledger exists that could desync with the reaper (whose state file is a
  timing cache, not a claim ledger — see the reaper bullet below).
- **Per-IP rate limiting** (`ratelimit.go`) — token bucket per client IP
  (x/time/rate): burst 5, sustained 1 per 10 s; excess requests get 429.
  It wraps only `/register` — loading the form page is a public static
  read and must not consume a visitor's registration budget.
  Client IP comes from `RemoteAddr` normally; when the direct peer is a
  trusted proxy (localhost only), the **rightmost** `X-Forwarded-For`
  entry is used — a client can't mint fresh buckets by spoofing left-side
  entries.  On Fly.io (detected via the `FLY_MACHINE_ID` env var Fly sets
  on every Machine), the peer is Fly's edge proxy and `Fly-Client-IP` —
  which Fly's edge overwrites, so clients cannot forge it there — is used
  instead; the env gate exists because outside Fly nothing strips that
  header, so honoring it unconditionally would let anyone mint fresh
  buckets.  A present-but-invalid `Fly-Client-IP` falls back to the peer
  address.  `X-Real-IP` is never trusted.  Idle IPs are evicted after an
  hour (sweep above a 10 000-entry threshold) to bound memory under
  IP-spray abuse.
- **Per-DID handle cap** (`porkbun.go`, `provision.go`) — one handle per DID
  per domain: the same zone listing that checks label availability also
  counts the DID's existing `_atproto` records in that zone; a second label
  for the same DID is refused with 409 ("You already have a handle under
  <domain>").  The count runs after the label lookup, so re-submitting
  one's own handle stays an idempotent 200.  Rationale: the cap multiplies
  squatting cost (one DID can hold N labels) and keeps the natural
  one-handle-per-person shape; it is per-domain, so the same DID can claim
  one label under each managed domain.
- **Audit logging** (`handlers.go`) — every successful registration logs
  `AUDIT: <status> <handle> -> <did> (client <ip>)`.  There is no local
  ledger by design, so this line is the only record of who took what when —
  useful for dispute resolution and for debugging the reaper.
  Handle and DID are charset-validated before this point, so the log line
  cannot be forged via newline injection.
- **Server timeouts** (`main.go`) — ReadHeaderTimeout 10s, ReadTimeout 30s,
  WriteTimeout 30s, IdleTimeout 2m on the `http.Server`, so a slowloris
  client cannot pin connections open indefinitely.
- **Handle reaper** (`reaper.go`, `resolver.go`) — frees labels whose
  accounts no longer use them.  A background task sweeps every managed
  zone (default every 6h, plus once at startup), resolves each record's
  DID (plc.directory for `did:plc`, the domain's `/.well-known/did.json`
  for `did:web`), and checks the DID document's `alsoKnownAs` for
  `at://<handle>` — the list of handles the account actually uses.  Only
  AFFIRMATIVE evidence counts: a directory 404 means the account is gone;
  a network error or an unverifiable DID means skip and retry next pass.
  A mismatch must persist for the whole grace period (default 30 days,
  the chicken-and-egg allowance for setting the username in-app after the
  record exists) before the record is deleted — every reap logs an
  `AUDIT: reaped ...` line for dispute resolution.  Stacked (corrupt)
  `_atproto` names are judged record by record, so a stacked pair loses
  its dead half and keeps its live one, healing the handle.  Grace
  clocks live in `reaper_state.json` (working directory, gitignored),
  keyed by **Porkbun record ID**: a re-registered label gets a fresh
  record ID and therefore a fresh grace clock, so the reaper can never
  delete a new claim on a stale clock; and the file is a timing cache,
  not a claim ledger — losing or corrupting it only restarts clocks
  (reaps are delayed, never accelerated).  The reaper only runs with
  real Porkbun credentials (there is nothing to sweep in the
  memoryProvisioner) and needs `porkbun.com` network egress in
  production.

## Deployment notes (not code)

- **Porkbun keys must be scoped per domain** in production (dns scope,
  single-domain allowlist) and delivered as the per-domain pair —
  environment variables via `fly secrets set` (see Build & run) or the
  `porkbun_keys/` files.  The client selects each request's key pair by
  the Host-resolved domain, so one community's key can never touch
  another's zone.
- **Hosting is one Fly.io Machine per service.**  TLS terminates at Fly's
  edge proxy, which forwards the original Host header (domain selection
  depends on it) and sets `Fly-Client-IP` (see the rate-limiting bullet
  in Defenses).  Machines autostop when idle; the reaper's timers freeze
  while a Machine is suspended, which only delays sweeps — and delayed
  reaps are the safe direction by design.  `reaper_state.json` lives on
  the Machine rootfs and does not survive redeploys; losing it only
  restarts grace clocks.
- **Deployment artifacts** live at the repo root: `Dockerfile`
  (multi-stage: a `node:24-alpine` stage runs `pnpm build` on the frontend —
  the artifact is not committed, so the image builds it from source with
  `--frozen-lockfile` — then a `golang:1.26-alpine` build with
  `CGO_ENABLED=0`, then `distroless/static:nonroot` runtime — distroless
  ships the CA
  certificates the Porkbun client and the DID resolver need for TLS, and
  its `/app` workdir is pre-created nonroot-owned so the reaper can write
  its state file), `.dockerignore` (a whitelist that lets only
  `go.mod`/`go.sum`/`cmd/`/`frontend/` into the build context, minus the
  local `node_modules`/`dist`/`cmd/niceat/web` build products, so instance
  files and Porkbun keys can never reach the daemon or the image), and
  `fly.toml`
  (port 8080, `[http_service.checks]` on /healthz,
  `auto_stop_machines = "suspend"` so the rootfs — and the grace clocks —
  survive idle suspension, and a `[[files]]` entry injecting the config).
- **The config reaches the Machine as a secret, not a file.**
  `niceat.json` is gitignored and instance-specific, so `fly.toml` maps
  it from a base64-encoded secret (`NICEAT_CONFIG`) to `/app/niceat.json`
  via `[[files]]` — the file is re-injected on every deploy, unlike the
  rootfs.  Deploy fails loudly if the secret is absent (`domains` is
  required).  Operator sequence, in order: `fly apps create <name>` (then
  rename `app` in fly.toml if needed) →
  `fly secrets set NICEAT_CONFIG="$(base64 -i niceat.json)"` (Linux:
  `base64 -w0`) → `fly secrets set PORKBUN_<DOMAIN>_API_KEY=…/_SECRET_KEY=…`
  for every managed domain → `fly deploy` → `fly certs add <domain>` per
  managed domain plus the DNS records it prints.  `primary_region` in
  fly.toml is a placeholder to set before the first deploy.

## Testing / linting

```bash
go test -count=1 -v ./cmd/niceat/   # full run
go test -count=1 -run TestValidateDID ./cmd/niceat/   # subset
go vet ./cmd/niceat/
gofmt -l .   # must print nothing
cd frontend && pnpm check && pnpm build   # frontend typecheck + rebuild artifact
```

Tests are fully self-contained (`fakeProvisioner`, `fakePorkbunServer`,
`httptest`); no network, no fixtures.  When changing validation rules, extend
the table tests in `validate_test.go` — they are the spec for handle/DID
acceptance.  When changing Porkbun call semantics, extend `porkbun_test.go` —
its fake mirrors the sandbox-verified duplicate/stacking behavior.  When
changing routing, extend `web_test.go` / the mux tests — they run against
the same `newMux` main.go serves.

## Conventions

- Match the sibling projects (nicelink, cola): single package under `cmd/`,
  stdlib first, tests beside source, `{"detail": ...}` error bodies.
- Non-obvious logic (validation choices, security properties) carries a
  comment explaining the *why*, not the *what*.
- Keep the credential-less fallback out of any public deployment;
  `memoryProvisioner` reporting success while publishing nothing is a
  foot-gun by design of this phase.
