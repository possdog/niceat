package main

import (
	"path/filepath"
	"testing"
)

// storeFixture opens a claims store at a fixed path so persistence across
// close/reopen can be exercised, unlike newTestClaims which hides the path.
func storeFixture(t *testing.T) (*claims, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claims.bolt")
	store, err := openClaims(path)
	if err != nil {
		t.Fatalf("open claims store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("close claims store: %v", err)
		}
	})
	return store, path
}

func TestTryClaimFresh(t *testing.T) {
	store, _ := storeFixture(t)

	created, existing, err := store.tryClaim("wagg.ing", "john", validPLC)
	if err != nil {
		t.Fatalf("tryClaim: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for unclaimed label")
	}
	if existing != "" {
		t.Fatalf("expected empty existing DID, got %q", existing)
	}
}

func TestTryClaimSameDIDIsIdempotent(t *testing.T) {
	store, _ := storeFixture(t)
	if _, _, err := store.tryClaim("wagg.ing", "john", validPLC); err != nil {
		t.Fatalf("first tryClaim: %v", err)
	}

	created, existing, err := store.tryClaim("wagg.ing", "john", validPLC)
	if err != nil {
		t.Fatalf("repeat tryClaim: %v", err)
	}
	if created {
		t.Fatalf("expected created=false for repeat by same DID")
	}
	if existing != validPLC {
		t.Fatalf("expected existing DID %q, got %q", validPLC, existing)
	}
}

func TestTryClaimDifferentDIDConflicts(t *testing.T) {
	store, _ := storeFixture(t)
	if _, _, err := store.tryClaim("wagg.ing", "john", validPLC); err != nil {
		t.Fatalf("first tryClaim: %v", err)
	}
	other := "did:plc:" + repeatChar('y', 24)

	created, existing, err := store.tryClaim("wagg.ing", "john", other)
	if err != nil {
		t.Fatalf("conflicting tryClaim: %v", err)
	}
	if created {
		t.Fatalf("expected created=false for conflicting DID")
	}
	if existing != validPLC {
		t.Fatalf("expected existing DID %q, got %q", validPLC, existing)
	}
}

func TestTryClaimPerDomainIsolation(t *testing.T) {
	store, _ := storeFixture(t)
	// The same label under two different domains must be independent:
	// "john" under wagg.ing says nothing about "john" under yeen.world.
	if _, _, err := store.tryClaim("wagg.ing", "john", validPLC); err != nil {
		t.Fatalf("tryClaim wagg.ing: %v", err)
	}
	other := "did:plc:" + repeatChar('y', 24)

	created, existing, err := store.tryClaim("yeen.world", "john", other)
	if err != nil {
		t.Fatalf("tryClaim yeen.world: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for same label under a different domain (existing=%q)", existing)
	}
}

func TestRelease(t *testing.T) {
	store, _ := storeFixture(t)
	if _, _, err := store.tryClaim("wagg.ing", "john", validPLC); err != nil {
		t.Fatalf("tryClaim: %v", err)
	}

	if err := store.release("wagg.ing", "john"); err != nil {
		t.Fatalf("release: %v", err)
	}
	created, existing, err := store.tryClaim("wagg.ing", "john", "did:plc:"+repeatChar('z', 24))
	if err != nil {
		t.Fatalf("tryClaim after release: %v", err)
	}
	if !created {
		t.Fatalf("expected label to be reclaimable after release (existing=%q)", existing)
	}

	// Releasing an absent claim must not error: release is a rollback
	// path and must tolerate the claim already being gone.
	if err := store.release("wagg.ing", "nobody"); err != nil {
		t.Fatalf("release of absent claim: %v", err)
	}
}

func TestClaimsPersistAcrossReopen(t *testing.T) {
	// Close the fixture's cleanup-managed handle is awkward; drive the
	// lifecycle manually here so the same file can be reopened.
	path := filepath.Join(t.TempDir(), "claims.bolt")
	store, err := openClaims(path)
	if err != nil {
		t.Fatalf("open claims store: %v", err)
	}
	if _, _, err := store.tryClaim("wagg.ing", "john", validPLC); err != nil {
		t.Fatalf("tryClaim: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("close claims store: %v", err)
	}

	reopened, err := openClaims(path)
	if err != nil {
		t.Fatalf("reopen claims store: %v", err)
	}
	defer func() {
		if err := reopened.close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	}()

	created, existing, err := reopened.tryClaim("wagg.ing", "john", "did:plc:"+repeatChar('y', 24))
	if err != nil {
		t.Fatalf("tryClaim after reopen: %v", err)
	}
	if created {
		t.Fatalf("claim did not survive reopen")
	}
	if existing != validPLC {
		t.Fatalf("expected persisted DID %q, got %q", validPLC, existing)
	}
}

// repeatChar returns a string of n copies of c, for building distinct valid
// did:plc identifiers in tests.
func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
