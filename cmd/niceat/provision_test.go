package main

import (
	"context"
	"testing"
)

func TestMemoryProvisionerOutcomes(t *testing.T) {
	m := newMemoryProvisioner()
	ctx := context.Background()
	other := "did:plc:" + repeatRunes('y', 24)

	if outcome, err := m.Provision(ctx, testDomain, "john", validPLC); err != nil || outcome != outcomeCreated {
		t.Fatalf("fresh provision: outcome=%d err=%v", outcome, err)
	}
	if outcome, err := m.Provision(ctx, testDomain, "john", validPLC); err != nil || outcome != outcomeRegistered {
		t.Fatalf("same-DID repeat: outcome=%d err=%v", outcome, err)
	}
	if outcome, err := m.Provision(ctx, testDomain, "john", other); err != nil || outcome != outcomeConflict {
		t.Fatalf("different DID: outcome=%d err=%v", outcome, err)
	}
}

func TestMemoryProvisionerDIDLimit(t *testing.T) {
	m := newMemoryProvisioner()
	ctx := context.Background()

	if _, err := m.Provision(ctx, testDomain, "john", validPLC); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	// One handle per DID per domain: a second label for the same DID must
	// be refused.
	outcome, err := m.Provision(ctx, testDomain, "mary", validPLC)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if outcome != outcomeDIDLimit {
		t.Fatalf("expected outcomeDIDLimit, got %d", outcome)
	}

	// The cap is per domain: the same DID may claim the same label shape
	// under another community.
	outcome, err = m.Provision(ctx, "vore.rip", "mary", validPLC)
	if err != nil {
		t.Fatalf("cross-domain provision: %v", err)
	}
	if outcome != outcomeCreated {
		t.Fatalf("expected outcomeCreated across domains, got %d", outcome)
	}
}

// repeatRunes returns n copies of c, for building distinct valid did:plc
// identifiers in tests.
func repeatRunes(c rune, n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
