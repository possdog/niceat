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

// Porkbun implementation notes (for the future porkbun.go), recorded here so
// the constraints are not lost:
//
//   - Credentials come from porkbun_api_key.txt / porkbun_secret_key.txt in
//     the working directory (both gitignored).  Current keys are sandbox:
//     requests are simulated, so tests can hit the real client safely.
//   - One key pair covers the whole Porkbun account, so the same client
//     serves every managed domain; the {domain} in the endpoint path comes
//     from the Host-resolved domain of the request, never from the client.
//   - Endpoint: POST https://api.porkbun.com/api/json/v3/dns/create/{domain}
//     with {"apikey", "secretapikey", "type":"TXT",
//     "name":"_atproto.<label>", "content":"did=<did>", "ttl":"300"}.
//   - Porkbun returns "SUCCESS" even when an identical record already
//     exists, so idempotent retries are safe.
//   - The handler must map transport failures to 5xx and keep the response
//     body generic (no Porkbun error details leaked to clients).
