# niceat

niceat is a small web service that lets a community hand out Bluesky
handles under a domain it owns. Visitors fill in a form with the handle
label they want and their account DID; the service publishes the
`_atproto` TXT record that binds `label.domain` to the DID through the
Porkbun DNS API, and Bluesky then accepts `someone.example.com` as that
account's username.

## Who it's for

Communities and organizations that own a domain and want to offer
self-service handles under it — without editing DNS records by hand for
each member, and without giving members access to the DNS provider at
all. One deployment can serve several domains at once; the web page and
API are the same on each, and the domain a visitor is browsing decides
where their handle is registered.

## How it works

- **DNS is the claim store.** Whether a handle is available, and who owns
  it, is decided entirely by the `_atproto` TXT records in the domain's
  Porkbun zone. There is no local database, so the service cannot drift
  out of sync with manual edits, zone restores, or the reaper.
- **A small Go API behind the form.** The form page is embedded in a
  single Go binary and posts same-origin to `POST /register`, which
  validates the request and creates the TXT record. The handler only
  knows the Porkbun backend through an interface, so other DNS providers
  can be swapped in without touching the HTTP layer.
- **Validation follows both specs.** A requested label must be valid as a
  Bluesky username *and* as a DNS subdomain, and the composed handle is
  capped at 30 characters to match what the official Bluesky app accepts
  for custom-domain handles. Infrastructure and protocol-reserved labels
  (`www`, `_atproto`, ...) are refused, and DIDs are restricted to
  `did:plc` and `did:web` forms.
- **Abuse is priced in.** Registration is rate limited per client IP, each
  DID holds at most one handle per domain, repeat submissions of an
  existing claim are idempotent, and every successful registration leaves
  an audit log line.
- **A reaper frees abandoned handles.** A background task periodically
  sweeps each zone, resolves each record's DID to its DID document, and
  checks whether the account actually uses that handle. If the mismatch
  persists for a grace period (30 days by default, to allow for the time
  it takes to switch a username in the app), the record is deleted and
  the label becomes available again.

See [AGENTS.md](AGENTS.md) for the full validation rules, status codes,
and security properties.

## Running an instance

Running niceat means provisioning two things:

1. **A JSON config file** listing the apex domains to serve, the listen
   address, and the reaper's interval and grace period. See
   `niceat.example.json` for the shape.
2. **A Porkbun API key pair per domain**, provided as files or
   environment variables. Keys are scoped per domain, so one community's
   key can never touch another's zone. With no credentials at all the
   service falls back to an in-memory backend with the same semantics
   but nothing published — useful for development, never for real
   traffic. Sandbox API keys (`pk1_sb_`/`sk1_sb_`) are simulated, so
   smoke tests are safe.

For production, the repo ships a `Dockerfile` and `fly.toml` targeting a
single Fly.io Machine for the service plus a scheduled Machine for the
reaper. `deploy.sh` is the day-to-day entry point: it syncs the config
and credentials as secrets, releases the serving Machine, and rebuilds
the reaper Machine on the new image. One-time setup (creating the app,
registering domains and their TLS certificates) and the "Build & run"
steps for running the binary locally are in [AGENTS.md](AGENTS.md).

## Development

The backend is Go 1.26, a single package under `cmd/niceat/`, standard
library plus `golang.org/x/time/rate`. Tests are fully self-contained
(httptest fakes, no network) and live beside the source. The frontend is
Svelte 5 + Bulma built by Vite into a single `index.html` that is embedded
with `go:embed`; the artifact is a build product, not committed, so run
`pnpm build` in `frontend/` before building or testing the Go side (the
deploy image does this itself).

```sh
go test -count=1 ./cmd/niceat/   # backend tests
go vet ./cmd/niceat/
gofmt -l .                       # must print nothing
cd frontend && pnpm check && pnpm build   # typecheck + rebuild the embedded page
```

## Further documentation

[AGENTS.md](AGENTS.md) carries the exhaustive detail: the API surface and
status codes, validation rules and why they exist, the security defenses,
Porkbun client semantics, the reaper's state model, and step-by-step
deployment notes.

