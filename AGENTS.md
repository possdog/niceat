# AGENTS.md — niceat

## Project overview

niceat lets communities self-service Bluesky handles under a shared domain:
a web form (not yet built) collects a desired handle label plus the user's
Bluesky account DID, and this API provisions the `_atproto` TXT record that
binds `<label>.<domain>` to the DID via the Porkbun DNS API.  Go 1.26,
standard library only, one package at `cmd/niceat/`.

## Build & run

```bash
go build -o niceat ./cmd/niceat
./niceat -domain example.com -addr :8080
```

`-domain` (apex domain served) is required; the binary refuses to start
without it.  `porkbun_api_key.txt` / `porkbun_secret_key.txt` in the working
directory hold the sandbox Porkbun credentials (gitignored; never commit,
never hard-code them into source or logs).

The DNS backend is currently `stubProvisioner`: it logs what it would create
and returns success, so requests "work" end-to-end but no DNS record exists.
The binary warns about this at startup.

## The API (what exists today)

Single endpoint:

- **POST /register** — JSON `{"handle": "<label>", "did": "did:plc:..."}`.
  Returns `201 {"status":"created","handle":"<label>.<domain>"}`.

Status codes: 201 created; 400 invalid handle/DID or malformed JSON; 405
non-POST (from the mux's `POST /register` pattern); 413 body over 4 KiB;
415 Content-Type not `application/json`; 500 provisioning failure.

Validation (in `validate.go`, all error messages user-displayable):

- **Handle** is a single DNS label — 1–63 chars `[a-z0-9-`, no leading/trailing
  hyphen, no dots or underscores — lowercased and whitespace-trimmed first.
  Dots are rejected explicitly so a caller cannot reach outside the served
  domain.  Labels in `reservedLabels` (app.go) are refused: infrastructure
  names (www, mx, ns1, ...) and AT Protocol names (_atproto, bsky, plc, ...).
  The list is meant to be over-inclusive; extending it is safe, recalling a
  claimed label is not.
- **DID** accepts only `did:plc` (24 chars `[a-z2-7]`) and `did:web` (pchar
  charset), max 200 bytes.  Tight per-method regexes guarantee the DID cannot
  inject extra content into the `did=<DID>` TXT record value.  DIDs are
  case-sensitive and are never normalized.

## Architecture

```
cmd/niceat/
├── main.go        — flags (-domain, -addr), mux, graceful shutdown
├── app.go         — app struct, regexes, reservedLabels, JSON types
├── handlers.go    — handleRegister + writeJSON
├── validate.go    — normalizeHandle, validateDID
├── provision.go   — provisioner interface, stubProvisioner, Porkbun notes
└── *_test.go      — tests alongside source, fakeProvisioner in helpers_test.go
```

The handler only sees the `provisioner` interface
(`Provision(ctx, domain, label, did)`), so `porkbun.go` can be added without
touching the HTTP layer; tests use `fakeProvisioner`.  Porkbun endpoint
details and credential handling are recorded as comments in `provision.go`.

## Planned defenses (designed for, deliberately NOT yet implemented)

Correctness comes first; none of these are wired up yet.  Hook points:

- **Per-IP rate limiting** — middleware slot in `main.go` marked
  `TODO(defense)`; copy the approach from nicelink's `ratelimit.go`
  (golang.org/x/time/rate, rightmost X-Forwarded-For behind trusted proxy).
- **Duplicate/conflicting claims** — nothing persists claims yet, so the same
  label can be claimed twice and the second overwrites the first.  Needs a
  store (bbolt) before public exposure.
- **DID ownership verification** — currently the form attests the DID; a
  stronger design verifies the requester controls the DID before
  provisioning.

Do not consider the service deployable until these are addressed.

## Testing / linting

```bash
go test -count=1 -v ./cmd/niceat/   # full run
go test -count=1 -run TestValidateDID ./cmd/niceat/   # subset
go vet ./cmd/niceat/
gofmt -l .   # must print nothing
```

Tests are fully self-contained (fake provisioner, `httptest`); no network, no
fixtures.  When changing validation rules, extend the table tests in
`validate_test.go` — they are the spec for handle/DID acceptance.

## Conventions

- Match the sibling projects (nicelink, cola): single package under `cmd/`,
  stdlib first, tests beside source, `{"detail": ...}` error bodies.
- Non-obvious logic (validation choices, security properties) carries a
  comment explaining the *why*, not the *what*.
- Keep the stub out of any public deployment; `stubProvisioner` reporting
  success while doing nothing is a foot-gun by design of this phase.
