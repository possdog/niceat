package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeCleaner is the scriptable DNS backend for reaper tests: a fixed zone
// population plus stored grace markers and a log of delete calls.
type fakeCleaner struct {
	records []atprotoRecord
	// markers maps each marker record's own id to its contents, exactly
	// as it would live in the zone.
	markers    map[string]graceMarker
	markerSeq  int
	fails      bool // make listings error
	failDelete bool // make deletes error
	// deletes logs claim-record deletes; markerDeletes logs grace-marker
	// deletes (nearly every reap also cleans up its marker, and tests
	// assert on claim deletions).
	deletes       []string
	markerDeletes []string
}

func (f *fakeCleaner) listAtproto(_ context.Context, _ string) ([]atprotoRecord, error) {
	if f.fails {
		return nil, errors.New("zone unavailable")
	}
	return f.records, nil
}

func (f *fakeCleaner) listGraceMarkers(_ context.Context, _ string) ([]graceMarker, error) {
	if f.fails {
		return nil, errors.New("zone unavailable")
	}
	out := make([]graceMarker, 0, len(f.markers))
	for _, m := range f.markers {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeCleaner) createGraceMarker(_ context.Context, _, _, recordID string, since time.Time) error {
	f.markerSeq++
	if f.markers == nil {
		f.markers = make(map[string]graceMarker)
	}
	f.markers[fmt.Sprintf("marker-%d", f.markerSeq)] = graceMarker{
		ID:       fmt.Sprintf("marker-%d", f.markerSeq),
		RecordID: recordID,
		Since:    since,
	}
	return nil
}

func (f *fakeCleaner) deleteRecord(_ context.Context, domain, id string) error {
	// Marker and claim deletes are logged separately: nearly every reap
	// also cleans up its marker, and tests assert on claim deletions.
	if _, isMarker := f.markers[id]; isMarker {
		f.markerDeletes = append(f.markerDeletes, domain+"/"+id)
	} else {
		f.deletes = append(f.deletes, domain+"/"+id)
	}
	if f.failDelete {
		return errors.New("porkbun refused")
	}
	delete(f.markers, id)
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

// newTestReaper builds a reaper with a controllable clock.  The returned
// clock advances the reaper's notion of "now".
func newTestReaper(t *testing.T, cleaner *fakeCleaner, resolver *fakeReaperResolver, grace time.Duration) (*reaper, *time.Time) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	r := newReaper([]string{testDomain}, cleaner, resolver, grace)
	r.now = func() time.Time { return now }
	return r, &now
}

const reaperTestGrace = 30 * 24 * time.Hour

// claimDeletes returns the logged claim-record delete ids (domain prefix
// stripped) — marker deletes are logged separately in markerDeletes.
func claimDeletes(c *fakeCleaner) []string {
	out := make([]string, 0, len(c.deletes))
	for _, d := range c.deletes {
		out = append(out, strings.TrimPrefix(d, testDomain+"/"))
	}
	return out
}

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

	// Grace elapsed: the record is reaped and its marker cleaned up.
	*now = now.Add(2 * time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 1 || got[0] != "1" {
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
	if got := claimDeletes(cleaner); len(got) != 0 {
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
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("reaping on a stale clock after adoption must not happen, got %v", cleaner.deletes)
	}
	// Only after another full grace period may it go.
	*now = now.Add(reaperTestGrace)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 1 {
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
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("nothing may be reaped on inconclusive errors, got %v", cleaner.deletes)
	}

	// Once evidence returns, grace is measured from the ORIGINAL start.
	resolver.errDIDs = nil
	*now = start.Add(reaperTestGrace + time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 1 {
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
	if got := claimDeletes(cleaner); len(got) != 1 {
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
	// its own fresh grace window.  (Only the orphaned old marker is
	// pruned, which claimDeletes ignores.)
	*now = now.Add(reaperTestGrace * 2)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("a re-registered label must get a fresh grace clock, got deletes %v", cleaner.deletes)
	}
	*now = now.Add(reaperTestGrace + time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 1 || got[0] != "2" {
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
	if got := claimDeletes(cleaner); len(got) != 1 || got[0] != "2" {
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
	if got := claimDeletes(cleaner); len(got) != 1 {
		t.Fatalf("expected reap measured from the original clock, got %v", cleaner.deletes)
	}
}

func TestReaperGraceSurvivesRestart(t *testing.T) {
	// The clock lives in the zone, not in the process: a fresh reaper
	// instance (a redeploy, a recreated Machine) must continue an
	// existing grace clock rather than starting over.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	clock := func() time.Time { return now }
	r1 := newReaper([]string{testDomain}, cleaner, resolver, reaperTestGrace)
	r1.now = clock
	r1.sweep(context.Background()) // clock starts as a marker in the zone

	// "Restart": a brand-new reaper with no memory of the first one.
	now = base.Add(reaperTestGrace + time.Hour)
	r2 := newReaper([]string{testDomain}, cleaner, resolver, reaperTestGrace)
	r2.now = clock
	r2.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 1 {
		t.Fatalf("the grace clock must survive a restart, got %v", cleaner.deletes)
	}
}

func TestReaperCorruptMarkerFailsSafe(t *testing.T) {
	// A corrupt (unparseable) marker is treated like no clock: a fresh
	// clock starts and nothing is reaped early.  The corrupt marker is
	// pruned so it cannot accumulate.
	did := validPLC
	cleaner := &fakeCleaner{
		records: []atprotoRecord{{ID: "1", Label: "john", DID: did}},
		markers: map[string]graceMarker{
			"m1": {ID: "m1", Corrupt: true},
		},
	}
	resolver := &fakeReaperResolver{}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(reaperTestGrace * 2) // long past any pre-corrupt clock
	r := newReaper([]string{testDomain}, cleaner, resolver, reaperTestGrace)
	r.now = func() time.Time { return now }
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("a corrupt marker must restart grace, not reap, got %v", cleaner.deletes)
	}
	// The corrupt marker was pruned and a fresh, valid one written.
	if _, ok := cleaner.markers["m1"]; ok {
		t.Fatal("corrupt marker should have been pruned")
	}
	if len(cleaner.markers) != 1 {
		t.Fatalf("expected one fresh marker after the sweep, got %v", cleaner.markers)
	}
	for _, m := range cleaner.markers {
		if m.Corrupt || m.RecordID != "1" {
			t.Fatalf("fresh marker malformed: %+v", m)
		}
	}
}

func TestReaperDuplicateMarkersUseLatest(t *testing.T) {
	// Multiple markers for one record are a corruption case; the reaper
	// must judge by the MOST RECENT clock so corruption can only delay
	// a reap, never hasten one.
	did := validPLC
	cleaner := &fakeCleaner{
		records: []atprotoRecord{{ID: "1", Label: "john", DID: did}},
		markers: map[string]graceMarker{
			"m-old": {ID: "m-old", RecordID: "1", Since: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
			"m-new": {ID: "m-new", RecordID: "1", Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
	}
	resolver := &fakeReaperResolver{}
	r, now := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	// Past the old clock's grace but not the new one: no reap.
	*now = now.Add(reaperTestGrace - time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("the latest marker must win, got deletes %v", cleaner.deletes)
	}
	*now = now.Add(2 * time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 1 {
		t.Fatalf("expected reap once the latest marker's grace elapsed, got %v", cleaner.deletes)
	}
}

func TestReaperPrunesOrphanedMarkers(t *testing.T) {
	// Markers whose record vanished (reaped, manually deleted) must not
	// leave crumbs in the zone — a later re-registration of the same
	// label gets a fresh id and must never inherit an old clock.
	did := validPLC
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "john", DID: did}}}
	resolver := &fakeReaperResolver{}
	r, _ := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background())
	if len(cleaner.markers) != 1 {
		t.Fatalf("expected one grace marker after the first pass, got %v", cleaner.markers)
	}

	cleaner.records = nil // the record is gone by any means
	r.sweep(context.Background())
	if len(cleaner.markers) != 0 {
		t.Fatalf("markers for vanished records must be pruned, got %v", cleaner.markers)
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
	if got := claimDeletes(cleaner); len(got) != 1 {
		t.Fatalf("expected one failed delete attempt, got %v", cleaner.deletes)
	}
	if len(cleaner.markers) != 1 {
		t.Fatalf("the marker must survive a failed delete for the retry, got %v", cleaner.markers)
	}

	// A later pass — well short of another grace period — must retry
	// the delete, proving the clock was never restarted.
	cleaner.failDelete = false
	*now = now.Add(time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 2 {
		t.Fatalf("expected the delete to be retried on the next pass, got deletes %v", cleaner.deletes)
	}
}
