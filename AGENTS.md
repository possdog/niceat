# AGENTS.md — niceat

## Project overview

niceat lets communities self-service Bluesky handles under a shared domain:
a web form (not yet built) collects a desired handle label plus the user's
Bluesky account DID, and this API provisions the `_atproto` TXT record that
binds `<label>.<domain>` to the DID via the Porkbun DNS API.  Go 1.26,
standard library plus `golang.org/x/time`, one package at `cmd/niceat/`.

**DNS is the claim store.**  Whether a handle is available — and who it
belongs to — is decided entirely by the existence and content of the
`_atproto` TXT record in Porkbun.  There is deliberately no local ledger
that could desync with the planned reaper, manual Porkbun dashboard edits,
or zone restores.

## Build & run

Configuration lives in a JSON file (JSON because the standard library
parses it natively; a persistent service should not depend on CLI flags to
remember its setup).  The path defaults to `niceat.json` in the working
directory; the file is instance-specific and gitignored:

```json
{
  "domains": ["wagg.ing", "yeen.world"],
  "addr": ":8080"
}
```

- `domains` (required, non-empty): apex domains managed by this instance.
  Entries are validated and normalized at startup (lowercased, FQDN dot
  stripped, duplicates removed) — a bad entry is a startup error naming the
  offending value.
- `addr` (optional): HTTP listen address, defaults to `:8080`.
- Unknown fields are rejected (`DisallowUnknownFields`), so a typo like
  `"domain"` fails loudly instead of silently yielding an empty domain set.

```bash
go build -o niceat ./cmd/niceat
./niceat                       # reads ./niceat.json, listens on config "addr"
./niceat -addr :8123           # same config, listen address overridden
```

Only two flags exist.  `-config` defaults to `niceat.json` relative to the
current working directory, so a service manager just needs to set the
working directory.  `-addr` overrides the config file's `addr` (useful when
a host runs several services and ports are handed out dynamically, e.g.
`-addr :0`); empty means "use the config".  Precedence:
`-addr` flag > config `addr` > `:8080` default.

`porkbun_api_key.txt` / `porkbun_secret_key.txt` in the working directory
hold the Porkbun credentials (gitignored; never commit, never hard-code
them into source or logs).  Their presence selects the real Porkbun
backend; without the pair, the service falls back to `memoryProvisioner`
(see provision.go) — same semantics, but nothing is published and state is
lost on restart, so it must never front real traffic.  Current keys are
sandbox (`pk1_sb_`/`sk1_sb_`): Porkbun requests are simulated, so live
smoke tests are safe.

## The API (what exists today)

Single endpoint:

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
(worded identically to the reserved-name refusal, so probers learn nothing);
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
├── validate.go    — parseDomain, normalizeHandle, validateDID
├── ratelimit.go   — per-IP token bucket middleware
├── provision.go   — provisioner interface + outcomes, memoryProvisioner
├── porkbun.go     — Porkbun client: retrieveByNameType + create
└── *_test.go      — tests alongside source, fakes in helpers_test.go / porkbun_test.go
```

One third-party dependency exists alongside the standard library, matching
nicelink: `golang.org/x/time/rate` (token buckets).  (bbolt was used by the
claims store and was removed with it.)

The handler only sees the `provisioner` interface
(`Provision(ctx, domain, label, did) (provisionOutcome, error)`), so the DNS
backend is swappable without touching the HTTP layer; the domain argument is
the Host-resolved zone, so the Porkbun client automatically operates on the
matching domain.  The outcome (created / registered / conflict) maps to
201/200/409 respectively.  `porkbun.go` implements the real client —
retrieveByNameType for the existence check, create only when absent, with
sandbox-verified semantics (an exact-duplicate create is 400
`DUPLICATE_RECORD` with `existingId`; a same-name/different-content create
would stack a second TXT record and break the handle, hence the pre-check).
Tests use `fakeProvisioner` (handler) and `fakePorkbunServer` (client).

Because registration is a check-then-create over two Porkbun calls, the
handler serializes it per (domain, label) with `labelLocks` (app.go): two
concurrent requests for the same label must not both pass the "no record"
check and stack records.  The lock is process-local, which is correct only
while the deployment is a single instance.

## Planned frontend (not yet built)

The form will be served from the same mux on every managed domain, and its
markup must be parameterized by the request Host — "get your handle under
*@wagg.ing*" on wagg.ing, the equivalent on yeen.world — posting the form
same-origin so the Host header carries the domain selection.  See the
TODO(frontend) note in main.go.

## Defenses

- **DNS as the claim store** (`porkbun.go`) — availability and ownership are
  read from the `_atproto` TXT record itself (retrieveByNameType), and the
  record is created only when absent.  A same-DID repeat is an idempotent
  200; a different DID is refused with 409.  A multi-record `_atproto` name
  (corrupt per the atproto spec) is treated as taken, never stacked onto.
  The check-then-create flow runs under a per-label lock (`labelLocks`) so
  concurrent requests cannot stack records.  No local ledger exists that
  could desync with the reaper below.
- **Per-IP rate limiting** (`ratelimit.go`) — token bucket per client IP
  (x/time/rate): burst 5, sustained 1 per 10 s; excess requests get 429.
  Client IP comes from `RemoteAddr` normally; when the direct peer is a
  trusted proxy (localhost only), the **rightmost** `X-Forwarded-For`
  entry is used — a client can't mint fresh buckets by spoofing left-side
  entries.  `X-Real-IP` is never trusted.  Idle IPs are evicted after an
  hour (sweep above a 10 000-entry threshold) to bound memory under
  IP-spray abuse.
- **Handle reaper** — deliberately NOT yet implemented.  The account is the
  authority, not DNS, so DID ownership is not verified at registration;
  instead a slow background task will iterate the DNS records, resolve
  each DID (plc.directory for did:plc; the DID document's `aka` field
  lists the handles an account actually uses), and delete records whose
  account does not use the handle — freeing the label for someone else.
  It needs a grace period: users necessarily set their username in-app
  *after* the DNS record exists, or the bidirectional link could never be
  established.  This is the main remaining gap before public exposure.

## Testing / linting

```bash
go test -count=1 -v ./cmd/niceat/   # full run
go test -count=1 -run TestValidateDID ./cmd/niceat/   # subset
go vet ./cmd/niceat/
gofmt -l .   # must print nothing
```

Tests are fully self-contained (`fakeProvisioner`, `fakePorkbunServer`,
`httptest`); no network, no fixtures.  When changing validation rules, extend
the table tests in `validate_test.go` — they are the spec for handle/DID
acceptance.  When changing Porkbun call semantics, extend `porkbun_test.go` —
its fake mirrors the sandbox-verified duplicate/stacking behavior.

## Conventions

- Match the sibling projects (nicelink, cola): single package under `cmd/`,
  stdlib first, tests beside source, `{"detail": ...}` error bodies.
- Non-obvious logic (validation choices, security properties) carries a
  comment explaining the *why*, not the *what*.
- Keep the credential-less fallback out of any public deployment;
  `memoryProvisioner` reporting success while publishing nothing is a
  foot-gun by design of this phase.
