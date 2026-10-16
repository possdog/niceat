package main

import (
	"context"
	"log"
	"sync"
)

// provisionOutcome classifies the result of a Provision call.  DNS is the
// claim store: whether a handle is available, and who it belongs to, is
// decided entirely by the existence and content of the _atproto TXT record.
type provisionOutcome int

const (
	// outcomeCreated: no record existed; the TXT record was published now.
	outcomeCreated provisionOutcome = iota
	// outcomeRegistered: the record already exists with this same DID, so
	// the handle is (still) bound to the requester.  No write was needed;
	// the request is an idempotent repeat.
	outcomeRegistered
	// outcomeConflict: the label is bound to a different DID, or the zone
	// holds multiple records for it (which the atproto spec treats as an
	// invalid handle).  The request must be refused.
	outcomeConflict
)

// provisioner is the DNS backend: it publishes the record that binds a handle
// label to a DID, reporting whether that created a new record, confirmed an
// existing one, or found the label taken.  The handler only ever sees this
// interface, so the Porkbun client (porkbun.go) and tests can be swapped in
// without touching the HTTP layer.
//
// For a label "john" on domain "example.com", a successful call results in
// the TXT record:
//
//	_atproto.john.example.com.  "did=<did>"
//
// which is what Bluesky's handle resolver looks up when it sees the handle
// john.example.com.
type provisioner interface {
	Provision(ctx context.Context, domain, label, did string) (provisionOutcome, error)
}

// memoryProvisioner is the fallback backend used when no Porkbun credentials
// are present: it keeps label→DID bindings in memory using the same
// created/registered/conflict semantics as the real client, so the service
// runs end-to-end without credentials.
//
// CAUTION: it provisions nothing.  If the binary ever answers real traffic
// with this implementation, users will be told they succeeded while no DNS
// record exists — main.go warns loudly at startup.  The state is also lost
// on restart.
type memoryProvisioner struct {
	mu      sync.Mutex
	records map[string]string // "domain\x00label" -> DID
}

func newMemoryProvisioner() *memoryProvisioner {
	return &memoryProvisioner{records: make(map[string]string)}
}

func (m *memoryProvisioner) Provision(_ context.Context, domain, label, did string) (provisionOutcome, error) {
	key := domain + "\x00" + label
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.records[key]; ok {
		if existing == did {
			return outcomeRegistered, nil
		}
		return outcomeConflict, nil
	}
	m.records[key] = did
	log.Printf("STUB: would create TXT _atproto.%s.%s -> \"did=%s\"", label, domain, did)
	return outcomeCreated, nil
}
