package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCleaner is the scriptable DNS backend for reaper tests: a fixed zone
// population plus a log of delete calls.
type fakeCleaner struct {
	records    []atprotoRecord
	fails      bool // make listings error
	failDelete bool // make deletes error
	deletes    []string
}

func (f *fakeCleaner) listAtproto(_ context.Context, _ string) ([]atprotoRecord, error) {
	if f.fails {
		return nil, errors.New("zone unavailable")
	}
	return f.records, nil
}

func (f *fakeCleaner) deleteRecord(_ context.Context, domain, id string) error {
	f.deletes = append(f.deletes, domain+"/"+id)
	if f.failDelete {
		return errors.New("porkbun refused")
	}
	return nil
}

// fakeReaperResolver returns scripted responses per DID; a missing entry
// means "registered and using the handle".
type fakeReaperResolver struct {
	aka      map[string][]string
	unavail  map[string]bool // DIDs that return 404 (unregistered)
	errDIDs  map[string]bool // DIDs whose lookup fails (inconclusive)
	lookedUp []string
}

func (f *fakeReaperResolver) resolve(_ context.Context, did string) ([]string, bool, error) {
	f.lookedUp = append(f.lookedUp, did)
	if f.errDIDs[did] {
		return nil, true, errors.New("directory unreachable")
	}
	if f.unavail[did] {
		return nil, false, nil
	}
	if aka, ok := f.aka[did]; ok {
		return aka, true, nil
	}
	// Default: the account uses no handles — the classic stale squatter.
	return nil, true, nil
}

// newTestReaper builds a reaper with a controllable clock and a temp state
// file.  The returned clock advances the reaper's notion of "now".
func newTestReaper(t *testing.T, cleaner *fakeCleaner, resolver *fakeReaperResolver, grace time.Duration) (*reaper, *time.Time) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	r := newReaper([]string{testDomain}, cleaner, resolver, filepath.Join(t.TempDir(), reaperStateFile), grace)
	r.now = func() time.Time { return now }
	return r, &now
}

const reaperTestGrace = 30 * 24 * time.Hour

func TestReaperReapsAfterGrace(t *testing.T) {
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	// First pass: mismatch observed, grace clock starts, no delete.
	r.sweep(context.Background())
	if len(cleaner.deletes) != 0 {
		t.Fatalf("nothing may be deleted on the first observation, got %v", cleaner.deletes)
	}

	// Still within grace: no delete.
	*now = now.Add(reaperTestGrace - time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 0 {
		t.Fatalf("no delete within the grace period, got %v", cleaner.deletes)
	}

	// Grace elapsed: the record is reaped and the state entry goes away.
	*now = now.Add(2 * time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 || cleaner.deletes[0] != testDomain+"/1" {
		t.Fatalf("expected exactly record 1 deleted, got %v", cleaner.deletes)
	}
}

func TestReaperKeepsInUseHandles(t *testing.T) {
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{aka: map[string][]string{did: {"at://john." + testDomain}}}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background())
	*now = now.Add(reaperTestGrace * 2)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 0 {
		t.Fatalf("an in-use handle must never be deleted, got %v", cleaner.deletes)
	}
}

func TestReaperAdoptionCancelsClock(t *testing.T) {
	// The chicken-and-egg flow: the record sits unused for a while (clock
	// ticking), the user then adopts the handle, and the clock must be
	// cancelled — even if the account drops it again much later.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background()) // clock starts
	*now = now.Add(time.Hour)
	resolver.aka = map[string][]string{did: {"at://john." + testDomain}}
	r.sweep(context.Background()) // adopted: clock cancelled

	*now = now.Add(reaperTestGrace * 2)
	resolver.aka = map[string][]string{} // dropped again: a NEW clock must start
	r.sweep(context.Background())
	if len(cleaner.deletes) != 0 {
		t.Fatalf("reaping on a stale clock after adoption must not happen, got %v", cleaner.deletes)
	}
	// Only after another full grace period may it go.
	*now = now.Add(reaperTestGrace)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 {
		t.Fatalf("expected the record to be reaped after the second grace period, got %v", cleaner.deletes)
	}
}

func TestReaperTransientErrorPreservesClock(t *testing.T) {
	// A directory outage mid-grace must neither reap nor restart the
	// clock: the original start time stays authoritative.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{errDIDs: map[string]bool{did: true}}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	// The clock can only start from an affirmative observation, so run
	// one working pass first.
	resolver.errDIDs = nil
	r.sweep(context.Background())
	start := *now

	// Outage passes: no conclusion, clock keeps running.
	resolver.errDIDs = map[string]bool{did: true}
	*now = now.Add(reaperTestGrace / 2)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 0 {
		t.Fatalf("nothing may be reaped on inconclusive errors, got %v", cleaner.deletes)
	}

	// Once evidence returns, grace is measured from the ORIGINAL start.
	resolver.errDIDs = nil
	*now = start.Add(reaperTestGrace + time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 {
		t.Fatalf("expected reap once affirmative evidence returns, got %v", cleaner.deletes)
	}
}

func TestReaperUnregisteredDIDCounts(t *testing.T) {
	// A DID that no longer resolves at all is affirmative evidence: the
	// account is gone and can never use the handle again.  Grace still
	// applies (the directory could be wrong), but reap it must.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{unavail: map[string]bool{did: true}}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background())
	*now = now.Add(reaperTestGrace + time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 {
		t.Fatalf("an unregistered DID's record must be reaped after grace, got %v", cleaner.deletes)
	}
}

func TestReaperFreshRecordIDResetsClock(t *testing.T) {
	// The record-ID keying: if a long-unused record is (somehow) deleted
	// and the label re-registered, the new record has a new ID and a
	// fresh grace clock — the reaper must not delete the new claim using
	// the old owner's clock.
	did := validPLC
	other := "did:plc:" + strings.Repeat("y", 24)
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background()) // old clock starts at T0
	*now = now.Add(time.Hour)
	// The old record disappears (manually deleted) and a new one — a
	// different DID — takes the label between passes.
	cleaner.records = []atprotoRecord{{ID: "2", Label: "john", DID: other}}

	// Far past the OLD clock's grace: the new record must still survive
	// its own fresh grace window.
	*now = now.Add(reaperTestGrace * 2)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 0 {
		t.Fatalf("a re-registered label must get a fresh grace clock, got deletes %v", cleaner.deletes)
	}
	*now = now.Add(reaperTestGrace + time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 || cleaner.deletes[0] != testDomain+"/2" {
		t.Fatalf("expected the new record reaped only after its own grace, got %v", cleaner.deletes)
	}
}

func TestReaperStackedRecordsJudgedIndividually(t *testing.T) {
	// A stacked (corrupt) name: one half's account uses the handle, the
	// other's does not.  The dead half is reaped and the live one kept —
	// the handle actually becomes healthy again.
	live := validPLC
	dead := "did:plc:" + strings.Repeat("y", 24)
	cleaner := &fakeCleaner{records: []atprotoRecord{
		{ID: "1", Label: "john", DID: live},
		{ID: "2", Label: "john", DID: dead},
	}}
	resolver := &fakeReaperResolver{aka: map[string][]string{live: {"at://john." + testDomain}}}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background())
	*now = now.Add(reaperTestGrace + time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 || cleaner.deletes[0] != testDomain+"/2" {
		t.Fatalf("expected only the dead half deleted, got %v", cleaner.deletes)
	}
}

func TestReaperFailedListingPreservesState(t *testing.T) {
	// If a zone listing fails, records were not observed at all — their
	// grace clocks must survive (erasing them would silently restart
	// grace for every record in the zone).
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background())
	cleaner.fails = true
	*now = now.Add(time.Hour)
	r.sweep(context.Background()) // listing fails, nothing judged

	cleaner.fails = false
	*now = now.Add(reaperTestGrace + time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 {
		t.Fatalf("expected reap measured from the original clock, got %v", cleaner.deletes)
	}
}

func TestReaperStatePersistsAcrossRestart(t *testing.T) {
	// The grace clock must survive a process restart via the state file.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	statePath := filepath.Join(t.TempDir(), reaperStateFile)
	grace := reaperTestGrace

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	clock := func() time.Time { return now }
	r1 := newReaper([]string{testDomain}, cleaner, resolver, statePath, grace)
	r1.now = clock
	r1.sweep(context.Background()) // clock starts, state file written

	// "Restart": a fresh reaper over the same state file.
	now = base.Add(reaperTestGrace + time.Hour)
	r2 := newReaper([]string{testDomain}, cleaner, resolver, statePath, grace)
	r2.now = clock
	r2.sweep(context.Background())
	if len(cleaner.deletes) != 1 {
		t.Fatalf("the grace clock must survive a restart, got %v", cleaner.deletes)
	}
}

func TestReaperCorruptStateFailsSafe(t *testing.T) {
	// A corrupt state file is treated like a lost one: clocks restart,
	// nothing is reaped early.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	statePath := filepath.Join(t.TempDir(), reaperStateFile)
	if err := os.WriteFile(statePath, []byte(`{"example.com/1": "not a timestamp"`), 0600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(reaperTestGrace * 2) // long past any pre-corrupt clock
	r := newReaper([]string{testDomain}, cleaner, resolver, statePath, reaperTestGrace)
	r.now = func() time.Time { return now }
	r.sweep(context.Background())
	if len(cleaner.deletes) != 0 {
		t.Fatalf("a corrupt state file must restart grace, not reap, got %v", cleaner.deletes)
	}
	// And the file is rewritten as valid JSON for the next pass.
	if _, err := loadReaperState(statePath); err != nil {
		t.Fatalf("state file should be valid after a sweep: %v", err)
	}
}

func TestReaperPrunesVanishedRecords(t *testing.T) {
	// Records that vanished (reaped, manually deleted) must not leave
	// state-file crumbs behind.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	r, _ := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background())
	state, err := loadReaperState(r.statePath)
	if err != nil {
		t.Fatalf("loadReaperState: %v", err)
	}
	if len(state) != 1 {
		t.Fatalf("expected one grace entry after the first pass, got %v", state)
	}

	cleaner.records = nil // the record is gone by any means
	r.sweep(context.Background())
	state, err = loadReaperState(r.statePath)
	if err != nil {
		t.Fatalf("loadReaperState: %v", err)
	}
	if len(state) != 0 {
		t.Fatalf("state for vanished records must be pruned, got %v", state)
	}
}

func TestReaperFailedDeleteKeepsClock(t *testing.T) {
	// If Porkbun refuses the delete, the record and its clock both stay,
	// so the next pass retries rather than restarting grace.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background()) // clock starts
	*now = now.Add(reaperTestGrace + time.Hour)
	cleaner.failDelete = true
	r.sweep(context.Background())
	if len(cleaner.deletes) != 1 {
		t.Fatalf("expected one failed delete attempt, got %v", cleaner.deletes)
	}

	// A later pass — well short of another grace period — must retry
	// the delete, proving the clock was never restarted.
	cleaner.failDelete = false
	*now = now.Add(time.Hour)
	r.sweep(context.Background())
	if len(cleaner.deletes) != 2 {
		t.Fatalf("expected the delete to be retried on the next pass, got %v", cleaner.deletes)
	}
}
