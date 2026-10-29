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
or zone restores.  (The reaper's grace clocks are also zone records —
see Defenses.)

## Build & run

Configuration lives in a JSON file (JSON because the standard library
parses it natively; a persistent service should not depend on CLI flags to
remember its setup).  The path defaults to `niceat.json` in the working
directory; the file is instance-specific and gitignored:

```json
{
  "domains": ["wagg.ing", "vore.rip"],
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
- `trusted_proxies` (optional): CIDR prefixes of reverse proxies whose
  `X-Forwarded-For` header may be trusted for client-IP attribution
  (rate-limit buckets and the audit log).  Default empty: NO proxy is
  trusted, not even loopback — the peer address is always the client IP.
  Entries are validated, deduplicated, and IPv4-mapping-normalized at
  startup; a non-CIDR entry is a startup error naming the value.
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

Three flags exist.  `-config` defaults to `niceat.json` relative to the
current working directory, so a service manager just needs to set the
working directory.  `-addr` overrides the config file's `addr` (useful when
a host runs several services and ports are handed out dynamically, e.g.
`-addr :0`); empty means "use the config".  Precedence:
`-addr` flag > config `addr` > `:8080` default.  `-reap-once` runs exactly
one reaper sweep and exits without starting the HTTP server — the scheduled
reaper Machine's mode (see Deployment notes); it overrides
`reaper.enabled`, since creating that Machine is the enablement, and fails
fast when no Porkbun credentials are configured.

Porkbun credentials are **per domain** (mirroring production key scoping)
and live in **one domain-keyed document**: `porkbun_keys.json` under the
working directory maps each managed domain to its own key pair
(gitignored; never commit, never hard-code them into source or logs):

```json
{
  "wagg.ing":   {"api_key": "pk1_…", "secret_key": "sk1_…"},
  "vore.rip": {"api_key": "pk1_…", "secret_key": "sk1_…"}
}
```

A template lives at `porkbun_keys.example.json` — copy it to
`porkbun_keys.json` and fill in the real keys.

Credentials for every configured domain select the real Porkbun backend;
with no credentials at all (no env value, no file), the service falls back
to `memoryProvisioner` (see provision.go) — same semantics, but nothing is
published and state is lost on restart, so it must never front real
traffic.  Startup logs this at ERROR severity, and `/healthz` then reports
`"provisioner":"memory"` — alert on that value.  A configured domain missing from the document, an empty key, an
entry for a domain this instance does not manage, or a document that fails
to parse (unknown fields are rejected, as in the main config) is a startup
error naming the domain: a domain whose records could never be published
must fail loudly, not silently eat registrations.  There is deliberately no
global fallback pair — a shared pair would defeat the per-domain scoping's
blast-radius containment.  The same document can instead come from the
environment variable `PORKBUN_CREDENTIALS` (the Fly.io path).  Env wins
over the file — that is Fly.io's secrets model (`fly secrets set`), and a
stale local-dev file must not shadow what the operator set.  The
sandbox pair (`pk1_sb_`/`sk1_sb_`, scoped to the sandbox account's one
probe domain, `niceat-probe-7q4k9x.com`) lives in
`porkbun_sandbox_keys.json` — the same domain-keyed document format as
`porkbun_keys.json`, so a sandbox session is just that file copied over
`porkbun_keys.json` (with `niceat.json` listing the probe domain).
Porkbun sandbox requests are simulated, so live smoke tests are safe.

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
`X-Forwarded-Host`.  Related: Go's server would otherwise let an
absolute-form request target (`POST http://host/register`) override the
Host header when setting `r.Host`; since niceat is origin-only (browsers
and the Fly edge send origin-form), absolute-form and authority-form
targets are rejected with 400 by a middleware in `newMux`.

Status codes: 201 created; 200 idempotent repeat (handle already bound to
this DID); 400 invalid handle/DID, malformed JSON, or an absolute-form
request target (niceat is origin-only — see below); 405 non-POST (from the
mux's `POST /register` pattern); 409 handle claimed by a different DID
(worded identically to the reserved-name refusal, so probers learn nothing)
or this DID already holds another handle under the served domain (one handle
per DID per domain);
413 body over 4 KiB; 415 Content-Type not `application/json`; 421 Host not a
managed domain; 429 per-IP rate limit exceeded; 500 provisioning failure
(generic detail; backend errors are logged, not leaked); 503 the DNS
backend is unavailable or rate-limiting (generic detail, `Retry-After`
set — retry shortly).

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
  are case-sensitive and are never normalized.  Path-segment `did:web` DIDs
  (e.g. `did:web:example.com:users:alice`) are accepted and resolved per the
  did:web spec (host, optionally with a percent-encoded port; colon-separated
  segments become URL path segments; `/.well-known/did.json` only for the
  root form), so the reaper can judge records bound to them; identifiers
  that would map to a traversal or non-spec URL are unverifiable (reaper
  skips, never reaps on an error).  One deliberate tolerance:
  a leading `did=` is stripped (bsky.app's Change Handle flow copies the
  full TXT value, and users paste it verbatim) — unambiguous because a
  real DID starts with `did:`, never `did=`.

## Architecture

```
cmd/niceat/
├── main.go        — flags, config/credential loading, mux, graceful shutdown
├── config.go      — JSON config type, strict loadConfig, managedSet
├── app.go         — app struct, striped label locks, regexes, reservedLabels, JSON types
├── handlers.go    — resolveDomain, handleRegister + writeJSON
├── healthz.go     — GET /healthz liveness probe + active-provisioner report
├── validate.go    — parseDomain, normalizeHandle, validateDID
├── ratelimit.go   — per-IP token bucket middleware (bounded table, trusted-proxy config)
├── provision.go   — provisioner interface + outcomes, memoryProvisioner
├── porkbun.go     — Porkbun client: zone listing, per-DID cap, create, reaper primitives
├── resolver.go    — DID document resolution (plc.directory / did:web spec mapping, SSRF-guarded)
├── reaper.go      — background task: budgeted concurrent sweeps, grace markers, delete unused
├── web.go         — go:embed of the built form page + serveIndex (CSP/nosniff)
├── web/           — frontend build artifact (index.html), gitignored build product
└── *_test.go      — tests alongside source, fakes in helpers_test.go / porkbun_test.go

frontend/           — Svelte 5 + Blades CSS + Vite source; pnpm build emits cmd/niceat/web/index.html
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
Porkbun calls (list + create), not one — though bursts share one listing
per zone per 5 s via the client's cache (see Defenses).  When Porkbun
itself is unavailable or rate-limiting (a non-JSON body, HTTP 429/5xx),
the client returns a typed backend-unavailable error the handler maps
to 503 with `Retry-After` — shedding load instead of 500-spamming while
the shared key's rate-limit window resets.  Tests use `fakeProvisioner`
(handler) and `fakePorkbunServer` (client).

Because registration is a check-then-create over two Porkbun calls, the
handler serializes it per (domain, label) with `labelLocks` (app.go): two
concurrent requests for the same label must not both pass the "no record"
check and stack records.  The lock is process-local, which is correct only
while the deployment is a single instance; its fixed 256-stripe table keeps
memory O(1) under distinct-label spray (a never-shrinking lock map was an
OOM vector) without weakening the same-label serialization.

## Frontend

`frontend/` is a Svelte 5 (runes) + Blades CSS + Vite 8 app built with
`vite-plugin-singlefile` into one self-contained `index.html` (all JS and
CSS inlined), emitted directly into `cmd/niceat/web/` for `go:embed`.  The
Go server serves it unchanged at `/` on every managed domain:
the domain parameterization is **client-side** — the page reads
`location.hostname` to show "*Get yourself a wagg.ing handle on
Bluesky*" (the equivalent on vore.rip) and posts the form same-origin, so the Host
header carries the domain selection.  The server does no per-Host markup.
`serveIndex` (web.go) sends a strict `Content-Security-Policy`
(`default-src 'none'` plus inline script/style allowances — the
single-file bundle inlines everything — `connect-src 'self'` for
the form POST, and `img-src data:` for the favicon, which the build
inlines as a data URI: `src/assets/favicon.png` in frontend/ is
injected by the `inlineFavicon` plugin in vite.config.ts since the
singlefile plugin inlines only JS/CSS and the embed serves no external
files) and
`X-Content-Type-Options: nosniff`; `web_test.go`
pins the exact value, so a new frontend subresource fails loudly until
the policy is widened deliberately.

The artifact is gitignored (see Build & run): `go build` and the tests need
it present, so run `pnpm build` in `frontend/` before working on the Go
side.  For frontend-specific conventions (pnpm, classless-Blades-over-custom-
CSS, no SvelteKit/SSR), see frontend/AGENTS.md.  In dev, `pnpm dev` proxies
`/register` to a locally running Go server with the Host forced to
`wagg.ing` (the dev niceat.json must list it).

## Defenses

- **DNS as the claim store** (`porkbun.go`) — availability and ownership are
  read from the `_atproto` TXT records in a single zone listing (cached
  per domain for 5 s on the HTTP path; every create/delete invalidates the
  entry, so this process's own writes are always visible, and the reaper
  bypasses the cache entirely), and the
  record is created only when the label is absent.  A same-DID repeat is an
  idempotent 200; a different DID is refused with 409.  A multi-record
  `_atproto` name (corrupt per the atproto spec) is treated as taken, never
  stacked onto.  The check-then-create flow runs under a per-label lock
  (`labelLocks`, a fixed 256-stripe lock table: O(1) memory under
  distinct-label spray while same-label requests still serialize) so
  concurrent requests cannot stack records.  No local
  ledger exists at all — even the reaper's grace clocks are zone records
  (see the reaper bullet below) — so nothing can desync with manual
  Porkbun dashboard edits or zone restores.
- **Per-IP rate limiting** (`ratelimit.go`) — token bucket per client IP
  (x/time/rate): burst 5, sustained 1 per 10 s; excess requests get 429.
  It wraps only `/register` — loading the form page is a public static
  read and must not consume a visitor's registration budget.
  Client IP comes from `RemoteAddr` normally; when the direct peer falls
  inside a `trusted_proxies` CIDR prefix (opt-in via config — by default
  NO proxy is trusted, not even loopback), the **rightmost**
  `X-Forwarded-For` entry is used — a client can't mint fresh buckets by
  spoofing left-side entries.  On Fly.io (detected via the `FLY_MACHINE_ID` env var Fly sets
  on every Machine), the peer is Fly's edge proxy and `Fly-Client-IP` —
  which Fly's edge overwrites, so clients cannot forge it there — is used
  instead; the env gate exists because outside Fly nothing strips that
  header, so honoring it unconditionally would let anyone mint fresh
  buckets.  A present-but-invalid `Fly-Client-IP` falls back to the peer
  address.  `X-Real-IP` is never trusted.  IP keys are canonicalized (an
  IPv4-mapped spelling and its plain form share one bucket), and the table
  is hard-capped at 10 000 entries: past the cap, idle entries go first,
  then the least-recently-used quarter in one batch — so a sustained
  IP-spray can never grow memory without bound (and 429'd sprayers do
  not get their entries kept fresh forever).
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
  accounts no longer use them.  Sweeps every managed zone (in-process:
  every 6h plus once at startup; on Fly: one sweep per day via the
  scheduled reaper Machine running `-reap-once` — see Deployment notes
  for why sweeps must not depend on registration traffic), resolves
  each record's
  DID (plc.directory for `did:plc`, the DID's own domain per the did:web
  spec for `did:web` — root form at `/.well-known/did.json`, path-segment
  forms at `<path>/did.json` with a percent-encoded port decoded) through
  a bounded pool of 6 workers with a 5 s per-fetch timeout, under a
  4-minute per-zone resolution budget: records the budget does not reach
  are deferred to the next sweep with an operator-visible log line (a
  process-local resume cursor carries the position — judgment is
  idempotent, so a lost cursor only re-judges), and identical DIDs are
  resolved once per sweep with the verdict shared across their records.
  `did:web` fetches refuse loopback/private/link-local/unique-local
  destinations at dial time (which also defeats DNS rebinding) and
  follow no redirects — such records are unverifiable, never reaped.  The
  reaper checks the DID document's `alsoKnownAs` for
  `at://<handle>` — the list of handles the account actually uses.  Only
  AFFIRMATIVE evidence counts: a directory 404 means the account is gone,
  and for `did:web` a host that no longer exists in DNS (NXDOMAIN) is the
  same class of evidence — the host IS the identifier per the did:web
  spec; a network error, DNS SERVFAIL, a 5xx, a wrong-`id` document, or
  an unverifiable DID means skip and retry next pass.  When plc.directory
  answers 429, the sweep stops directory fetches for the rest of the pass
  (Retry-After honored, 60 s default) and defers the remaining
  `did:plc` records — a third-party rate limit can never masquerade as
  affirmative evidence.
  A mismatch must persist for the whole grace period (default 30 days,
  the chicken-and-egg allowance for setting the username in-app after the
  record exists) before the record is deleted — every reap logs an
  `AUDIT: reaped ...` line for dispute resolution.  Stacked (corrupt)
  `_atproto` names are judged record by record, so a stacked pair loses
  its dead half and keeps its live one, healing the handle.  Grace
  clocks live in the zone itself as TXT marker records
  (`_niceat-grace.<label>.<domain>`, content
  `niceat-grace v1 <record ID> <RFC3339>`), keyed by **Porkbun record
  ID**: a re-registered label gets a fresh record ID and therefore a
  fresh grace clock, so the reaper can never delete a new claim on a
  stale clock.  Storing clocks in the zone (not in a local file) makes
  the reaper fully stateless — redeploys and recreated Machines cannot
  restart grace clocks, and losing or corrupting a marker only restarts
  that one clock (reaps are delayed, never accelerated).  The reaper
  only runs with real Porkbun credentials (there is nothing to sweep in
  the memoryProvisioner) and needs `porkbun.com` network egress in
  production.

## Deployment notes (not code)

- **Porkbun keys must be scoped per domain** in production (dns scope,
  single-domain allowlist) and delivered as one domain-keyed document —
  the `PORKBUN_CREDENTIALS` environment variable via `fly secrets set`
  (see Build & run) or the `porkbun_keys.json` file.  The client selects
  each request's key pair by the Host-resolved domain, so one community's
  key can never touch another's zone.
- **Hosting is one Fly.io app with two Machines: a serving Machine and a
  scheduled reaper Machine.**  TLS terminates at Fly's edge proxy, which
  forwards the original Host header (domain selection depends on it) and
  sets `Fly-Client-IP` (see the rate-limiting bullet in Defenses).  The
  serving Machine autostops (suspends) when idle — and runs with
  `reaper.enabled = false`, because the reaper must not depend on
  registration traffic: scale-to-zero means the serving Machine is asleep
  most of the time, and the handles worth reaping live in exactly the
  zones that have stopped generating it.  The reaper therefore runs on a
  second, service-less Machine: the Fly proxy manages no such Machine,
  so its lifetime is exactly the process's lifetime — `niceat
  -reap-once` sweeps once (the per-zone resolution budget normally
  finishes well inside the hard 10-minute wall-clock deadline backstop,
  deferring rather than aborting; every fetch has its own timeout) and
  exits,
  which is what puts the Machine back to sleep.  A crash just stops the
  Machine until the next schedule — no crash loop, retry next day.  The
  reaper is stateless (its grace clocks live in the DNS zones as marker
  records), so neither the scheduled stop/start cycle nor a Machine
  recreation on deploy touches any clock.  (The sweep's resume cursor
  is process-local and purely a scheduling hint; losing it just
  re-judges idempotently.)  Both Machines share the same
  secrets; the same config serves both (`-reap-once` ignores
  `reaper.enabled`).  Routine deploys
  — secrets sync, serving Machine, and a reaper Machine recreated on
  the latest released image — are one command: `./deploy.sh`.  The
  reaper Machine carries metadata `fly_process_group=reaper` (what
  flyctl displays as its process group) so deploy.sh can find and
  replace it.
- **Deployment artifacts** live at the repo root: `Dockerfile`
  (multi-stage: a `node:24-alpine` stage runs `pnpm build` on the frontend —
  the artifact is not committed, so the image builds it from source with
  `--frozen-lockfile` — then a `golang:1.26-alpine` build with
  `CGO_ENABLED=0`, then `distroless/static:nonroot` runtime — distroless
  ships the CA
  certificates the Porkbun client and the DID resolver need for TLS, and
  its `/app` workdir is pre-created nonroot-owned), `.dockerignore`
  (a whitelist that lets only
  `go.mod`/`go.sum`/`cmd/`/`frontend/` into the build context, minus the
  local `node_modules`/`dist`/`cmd/niceat/web` build products, so instance
  files and Porkbun keys can never reach the daemon or the image), and
  `fly.toml`
  (port 8080, `[http_service.checks]` on /healthz,
  `auto_stop_machines = "suspend"` for fast wakeups from idle (the
  reaper keeps no local state, so idle behavior has no state
  implications), and a `[[files]]` entry injecting the config),
  and `deploy.sh` (the routine-deploy script: secrets sync, `fly deploy`,
  and reaper-Machine recreation on the latest released image — see the
  Hosting bullet).
- **The config reaches the Machine as a secret, not a file.**
  `niceat.json` is gitignored and instance-specific, so `fly.toml` maps
  it from a base64-encoded secret (`NICEAT_CONFIG`) to `/app/niceat.json`
  via `[[files]]` — the file is re-injected on every deploy, unlike the
  rootfs.  Deploy fails loudly if the secret is absent (`domains` is
  required).  Operator sequence, in order: `fly apps create <name>` (then
  rename `app` in fly.toml if needed) →
  `fly secrets set NICEAT_CONFIG="$(base64 -i niceat.json)"` (Linux:
  `base64 -w0`) →
  `fly secrets set PORKBUN_CREDENTIALS="$(cat porkbun_keys.json)"` →
  `fly deploy` → `fly certs add <domain>` per
  managed domain plus the DNS records it prints.  `primary_region` in
  fly.toml is a placeholder to set before the first deploy.

## Testing / linting

```bash
go test -count=1 -v ./cmd/niceat/   # full run
go test -race -count=1 ./cmd/niceat/   # with the race detector; passes, use it after concurrency-touching changes
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
