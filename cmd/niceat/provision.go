package main

import (
	"context"
	"log"
)

// provisioner is the DNS backend: it publishes the record that binds a handle
// label to a DID.  The handler only ever sees this interface, so the Porkbun
// client (future porkbun.go) can be swapped in later without touching the
// HTTP layer, and tests run without any network.
//
// For a label "john" on domain "example.com", a successful call results in
// the TXT record:
//
//	_atproto.john.example.com.  "did=<did>"
//
// which is what Bluesky's handle resolver looks up when it sees the handle
// john.example.com.
type provisioner interface {
	Provision(ctx context.Context, domain, label, did string) error
}

// stubProvisioner is the placeholder DNS backend used until the Porkbun
// client exists.  It logs what it would have created and reports success, so
// the HTTP layer can be developed and tested end-to-end today.
//
// CAUTION: it provisions nothing.  If the binary ever answers real traffic
// with this implementation, users will be told they succeeded while no DNS
// record exists — main.go warns loudly at startup.
type stubProvisioner struct{}

func (stubProvisioner) Provision(ctx context.Context, domain, label, did string) error {
	log.Printf("STUB: would create TXT _atproto.%s.%s -> \"did=%s\"", label, domain, did)
	// Deliberately returns nil: the stub simulates a successful Porkbun
	// call so the full request path can be exercised before the API is
	// wired up.  ctx is unused until the real client makes HTTP calls.
	_ = ctx
	return nil
}

// Porkbun implementation notes (for the future porkbun.go), verified against
// the sandbox API on 2026-10-15 (docs v3.63; OpenAPI spec at
// https://porkbun.com/api/json/v3/spec):
//
//   - Credentials come from porkbun_api_key.txt / porkbun_secret_key.txt in
//     the working directory (both gitignored).  Current keys are sandbox
//     (pk1_sb_/sk1_sb_): requests are simulated, so tests can hit the real
//     client safely.  Auth is apikey/secretapikey in the JSON body, or
//     X-API-Key/X-Secret-API-Key headers.
//   - One key pair covers the whole Porkbun account, so the same client
//     serves every managed domain; the {domain} in the endpoint path comes
//     from the Host-resolved domain of the request, never from the client.
//   - Endpoint: POST https://api.porkbun.com/api/json/v3/dns/create/{domain}
//     with {"apikey", "secretapikey", "type":"TXT",
//     "name":"_atproto.<label>", "content":"did=<did>", "ttl":"300"}.
//   - Create semantics (sandbox-verified): an exact repeat (same
//     name/type/content) returns HTTP 400 DUPLICATE_RECORD with the existing
//     record's id in existingId — NOT a second SUCCESS.  A create with the
//     same name but different content, however, succeeds and STACKS a second
//     TXT record on the same name.  Per the atproto handle spec ("only a
//     single valid record should exist... resolution should fail" if there
//     are multiple), stacked records break the handle for both parties — so
//     registration must check for an existing record before creating; create
//     alone is not put-if-absent.
//   - Existence check: GET/POST /dns/retrieveByNameType/{domain}/TXT/_atproto.<label>
//     returns the records for exactly that name and type.
//   - Every error carries a machine-readable code plus a next_action object
//     with a retryable flag; branch on code, never on message text.
//   - Writes accept an Idempotency-Key header (24h replay) — useful for safe
//     retries of a transport failure, but it only dedupes identical bodies,
//     so it cannot guard the same-label/different-DID race.
//   - The handler must map transport failures to 5xx and keep the response
//     body generic (no Porkbun error details leaked to clients).
