package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCleaner is the scriptable DNS backend for reaper tests: a fixed zone
// population plus stored grace markers and a log of delete calls.
type fakeCleaner struct {
	records []atprotoRecord
	// domainRecords, when set, overrides records for the zones it lists,
	// so a test can give several domains different populations (the
	// per-sweep DID dedup needs a cross-zone case).
	domainRecords map[string][]atprotoRecord
	// markers maps each marker record's own id to its contents, exactly
	// as it would live in the zone.  markerDomain scopes a marker to the
	// zone createGraceMarker put it in — production markers live in their
	// zone, so one zone's orphan pruning must never see (and delete)
	// another zone's markers; markers seeded directly without a domain
	// entry are listed for every zone (the single-domain tests).
	markers      map[string]graceMarker
	markerDomain map[string]string
	markerSeq    int
	fails        bool // make listings error
	failDelete   bool // make deletes error
	// deletes logs claim-record deletes; markerDeletes logs grace-marker
	// deletes (nearly every reap also cleans up its marker, and tests
	// assert on claim deletions).
	deletes       []string
	markerDeletes []string
}

func (f *fakeCleaner) listAtproto(_ context.Context, domain string) ([]atprotoRecord, error) {
	if f.fails {
		return nil, errors.New("zone unavailable")
	}
	if rs, ok := f.domainRecords[domain]; ok {
		return rs, nil
	}
	return f.records, nil
}

func (f *fakeCleaner) listGraceMarkers(_ context.Context, domain string) ([]graceMarker, error) {
	if f.fails {
		return nil, errors.New("zone unavailable")
	}
	out := make([]graceMarker, 0, len(f.markers))
	for id, m := range f.markers {
		if d, ok := f.markerDomain[id]; ok && d != domain {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeCleaner) createGraceMarker(_ context.Context, domain, _, recordID string, since time.Time) error {
	f.markerSeq++
	if f.markers == nil {
		f.markers = make(map[string]graceMarker)
	}
	if f.markerDomain == nil {
		f.markerDomain = make(map[string]string)
	}
	id := fmt.Sprintf("marker-%d", f.markerSeq)
	f.markers[id] = graceMarker{
		ID:       id,
		RecordID: recordID,
		Since:    since,
	}
	f.markerDomain[id] = domain
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
	delete(f.markerDomain, id)
	return nil
}

// fakeReaperResolver returns scripted responses per DID; a missing entry
// means "registered and using the handle".  delay simulates slow or
// hanging fetches.  The reaper's pool calls resolve from several
// goroutines at once, so every field is mutex-guarded and the lookup log
// is read through didLookups.
type fakeReaperResolver struct {
	mu       sync.Mutex
	aka      map[string][]string
	unavail  map[string]bool // DIDs that return 404 (unregistered)
	errDIDs  map[string]bool // DIDs whose lookup fails (inconclusive)
	delay    map[string]time.Duration
	lookedUp []string
}

func (f *fakeReaperResolver) resolve(_ context.Context, did string) ([]string, bool, error) {
	f.mu.Lock()
	d := f.delay[did]
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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

// didLookups returns the DIDs resolved so far, in lookup order.
func (f *fakeReaperResolver) didLookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lookedUp)
}

// newTestReaper builds a reaper with a controllable clock.  The returned
// clock advances the reaper's notion of "now".
func newTestReaper(t *testing.T, cleaner *fakeCleaner, resolver *fakeReaperResolver, grace time.Duration) (*reaper, *time.Time) {
	t.Helper()
	return clockedReaper(cleaner, resolver, grace)
}

// clockedReaper is newTestReaper for any didResolver: the MEDIUM-5 tests
// run the real plcDirectory verdict classification end to end, so they
// must be able to build the reaper around a resolver that is not a
// fakeReaperResolver.
func clockedReaper(cleaner *fakeCleaner, resolver didResolver, grace time.Duration) (*reaper, *time.Time) {
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

// captureLog redirects the process logger to a buffer for the test's
// duration, so a test can assert on operator-visible log lines.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

func TestReaperHangingFetchesDoNotSerialize(t *testing.T) {
	// HIGH-3: serial resolution let ~60 hanging DIDs eat the whole
	// -reap-once deadline (the audit measured 6 hanging records adding
	// exactly 6 fetch-timeouts).  With the pool, N hanging DIDs (N at
	// most the pool size) cost one fetch-timeout wave, not N of them.
	const (
		hangs = 4
		hang  = 250 * time.Millisecond
	)
	var records []atprotoRecord
	delay := make(map[string]time.Duration)
	for i := 0; i < hangs; i++ {
		did := fmt.Sprintf("did:plc:hang%03d", i)
		records = append(records, atprotoRecord{ID: strconv.Itoa(i), Label: fmt.Sprintf("hang%03d", i), DID: did})
		delay[did] = hang
	}
	cleaner := &fakeCleaner{records: records}
	resolver := &fakeReaperResolver{delay: delay}
	r, _ := newTestReaper(t, cleaner, resolver, reaperTestGrace)
	// Every hanging fetch gets its own worker, so the sweep takes one
	// hang, not hangs of them.
	r.workers = hangs

	begin := time.Now()
	r.sweep(context.Background())
	elapsed := time.Since(begin)

	if got := resolver.didLookups(); len(got) != hangs {
		t.Fatalf("every hanging record must still be judged, resolved %d of %d", len(got), hangs)
	}
	// Serial would be hangs*hang = 1 s; anything under ~0.7 s proves the
	// fetches overlapped.  The margin is deliberately generous so a
	// loaded test machine cannot flake the assertion.
	if elapsed > 700*time.Millisecond {
		t.Fatalf("hanging fetches serialized: sweep took %s, want well under %s", elapsed, hangs*hang)
	}
}

func TestReaperBudgetDefersPrefixAndResumes(t *testing.T) {
	// HIGH-3's other half: when the zone holds more slow resolutions
	// than the sweep budget covers, the sweep must judge a deterministic
	// prefix, defer the rest with an operator-visible line, resume where
	// it stopped on the NEXT sweep, and still perform the judged
	// records' writes.
	const (
		n          = 10
		perResolve = 15 * time.Millisecond
	)
	var records []atprotoRecord
	var dids []string
	delay := make(map[string]time.Duration)
	for i := 0; i < n; i++ {
		did := fmt.Sprintf("did:plc:budget%03d", i)
		records = append(records, atprotoRecord{ID: strconv.Itoa(100 + i), Label: fmt.Sprintf("u%02d", i), DID: did})
		dids = append(dids, did)
		delay[did] = perResolve
	}
	// The first record in sorted order sits far past an already-running
	// grace clock: it must be reaped on the first (budget-limited) sweep,
	// proving judged records keep their deletion writes while others are
	// deferred.
	pastGrace := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cleaner := &fakeCleaner{
		records: records,
		markers: map[string]graceMarker{
			"m0": {ID: "m0", RecordID: "100", Since: pastGrace},
		},
	}
	resolver := &fakeReaperResolver{delay: delay}
	r, _ := newTestReaper(t, cleaner, resolver, reaperTestGrace) // now = 2026-01-01, past the marker's grace
	r.workers = 1
	r.budget = 50 * time.Millisecond
	logBuf := captureLog(t)

	r.sweep(context.Background())

	// A deterministic, in-order prefix was judged — the count itself
	// may wobble with scheduler jitter, so assert the shape, not the
	// number.  At ~15 ms per resolve against a 50 ms budget, 3-5
	// records fit; all 10 never do.
	looked := resolver.didLookups()
	if len(looked) < 3 || len(looked) >= n {
		t.Fatalf("expected a partial prefix judged, got %d of %d", len(looked), n)
	}
	for i, did := range looked {
		if did != dids[i] {
			t.Fatalf("sweep must judge the sorted prefix in order: lookup %d = %s, want %s", i, did, dids[i])
		}
	}
	if !strings.Contains(logBuf.String(), "deferring") || !strings.Contains(logBuf.String(), fmt.Sprintf("of %d records", n)) {
		t.Fatalf("expected an operator-visible deferral line, got: %s", logBuf.String())
	}
	// The resume cursor points at the first deferred record.
	if got := r.resume[testDomain]; got != recordSortKey(records[len(looked)]) {
		t.Fatalf("resume cursor = %q, want the first deferred record %q", got, recordSortKey(records[len(looked)]))
	}
	// Judged records got their writes: the past-grace record was reaped…
	if got := claimDeletes(cleaner); len(got) != 1 || got[0] != "100" {
		t.Fatalf("a judged record past grace must be reaped despite deferrals, got %v", cleaner.deletes)
	}
	// …and each other judged record started its grace clock; no deferred
	// record did.
	clocked := make(map[string]bool, len(looked))
	for _, m := range cleaner.markers {
		clocked[m.RecordID] = true
	}
	for i := 1; i < len(looked); i++ { // record 100 was reaped, not clocked
		if !clocked[strconv.Itoa(100+i)] {
			t.Fatalf("judged record %d must have its grace marker, got %v", i, cleaner.markers)
		}
	}
	for i := len(looked); i < n; i++ {
		if clocked[strconv.Itoa(100+i)] {
			t.Fatalf("deferred record %d must not be judged this pass", i)
		}
	}

	// The next sweep resumes exactly where this one stopped and keeps
	// making forward progress until the whole zone is covered.
	logBuf.Reset()
	r.sweep(context.Background())
	resumed := resolver.didLookups()[len(looked):]
	if len(resumed) == 0 {
		t.Fatal("the second sweep must judge more records")
	}
	if resumed[0] != dids[len(looked)] {
		t.Fatalf("second sweep started at %s, want the first deferred record %s", resumed[0], dids[len(looked)])
	}
	for i, did := range resumed {
		if did != dids[(len(looked)+i)%n] {
			t.Fatalf("second sweep must continue in rotation order: lookup %d = %s, want %s", i, did, dids[(len(looked)+i)%n])
		}
	}
	for range n {
		if seen := len(unique(resolver.didLookups())); seen == n {
			break
		}
		r.sweep(context.Background())
	}
	if seen := len(unique(resolver.didLookups())); seen != n {
		t.Fatalf("successive sweeps must make forward progress and cover the zone, resolved %d of %d distinct DIDs", seen, n)
	}
	if _, still := r.resume[testDomain]; still {
		t.Fatal("the resume cursor must clear once the whole zone is covered")
	}
}

// unique returns the distinct entries of s, order-preserving.
func unique(s []string) []string {
	seen := make(map[string]bool, len(s))
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func TestReaperPoolJudgesAllRecordsMixedSpeeds(t *testing.T) {
	// The pool's concurrent path under the race detector (-race): fast,
	// slow, hanging, erroring, and unregistered resolutions mixed
	// together, with the default pool size.  Every record must be
	// resolved exactly once, and only the affirmative-404 third may
	// start grace clocks.
	const n = 36
	var records []atprotoRecord
	delay := make(map[string]time.Duration)
	aka := make(map[string][]string)
	errDIDs := make(map[string]bool)
	unavail := make(map[string]bool)
	for i := 0; i < n; i++ {
		did := fmt.Sprintf("did:plc:pool%03d", i)
		label := fmt.Sprintf("p%02d", i)
		records = append(records, atprotoRecord{ID: strconv.Itoa(i), Label: label, DID: did})
		switch i % 3 {
		case 0:
			// Slow fetch, but the account uses the handle: no clock.
			delay[did] = 20 * time.Millisecond
			aka[did] = []string{"at://" + label + "." + testDomain}
		case 1:
			errDIDs[did] = true // inconclusive: skip, no clock
		default:
			unavail[did] = true // affirmative 404: clock starts
		}
	}
	cleaner := &fakeCleaner{records: records}
	resolver := &fakeReaperResolver{delay: delay, aka: aka, errDIDs: errDIDs, unavail: unavail}
	r, _ := newTestReaper(t, cleaner, resolver, reaperTestGrace)

	r.sweep(context.Background())

	if got := resolver.didLookups(); len(got) != n {
		t.Fatalf("every record must be resolved, got %d of %d", len(got), n)
	}
	if len(cleaner.markers) != n/3 {
		t.Fatalf("only the unregistered third may start grace clocks, got %d markers for %d records", len(cleaner.markers), n)
	}
}

// webScript prescribes one did:web host's fetch outcome: a canned DID
// document (doc), a bare status (404, 5xx, ...), or a transport-level
// error shaped like the real dial errors Go's resolver and dialer
// produce.  It lets reaper tests run the resolver's REAL verdict
// classification — NXDOMAIN vs 404 vs every inconclusive failure — end
// to end, with no network at all.
type webScript struct {
	doc    string // 200 with this JSON body
	status int    // any other status: 404, 500, ...
	err    error  // transport-level failure (dial-class errors)
}

func (s webScript) roundTrip(req *http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	status, body := http.StatusOK, s.doc
	if s.status != 0 {
		status, body = s.status, ""
	}
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// scriptedWebTransport answers each fetch from a per-host script.  The map
// is read-only once the sweep starts, so the reaper's pool workers may
// share it concurrently.
type scriptedWebTransport struct{ byHost map[string]webScript }

func (s scriptedWebTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	script, ok := s.byHost[host]
	if !ok {
		return nil, fmt.Errorf("no script for host %q", host)
	}
	return script.roundTrip(req)
}

// newScriptedWebResolver returns a real plcDirectory resolving did:web
// DIDs against the given per-host scripts.
func newScriptedWebResolver(byHost map[string]webScript) *plcDirectory {
	d := newPlcDirectory("")
	d.hc = resolverHTTPClient(scriptedWebTransport{byHost: byHost})
	return d
}

func TestReaperNXDomainWebDIDReapedAfterGrace(t *testing.T) {
	// MEDIUM-5: a did:web host the DNS system affirmatively reports
	// nonexistent is the same class of evidence as a directory 404 — the
	// host IS the identity — so a record bound to it must start a grace
	// clock and be reaped after grace, instead of squatting its label
	// forever.  An in-use document beside it must still cancel its
	// existing clock (regression).
	cleaner := &fakeCleaner{
		records: []atprotoRecord{
			{ID: "1", Label: "inuse", DID: "did:web:inuse.example.com"},
			{ID: "2", Label: "gone", DID: "did:web:gone.example.com"},
			{ID: "3", Label: "nf", DID: "did:web:notfound.example.com"},
		},
		// A stale clock for the in-use record: adopting the handle
		// must cancel it.
		markers: map[string]graceMarker{
			"m0": {ID: "m0", RecordID: "1", Since: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		},
	}
	resolver := newScriptedWebResolver(map[string]webScript{
		"inuse.example.com":    {doc: `{"id":"did:web:inuse.example.com","alsoKnownAs":["at://inuse.` + testDomain + `"]}`},
		"gone.example.com":     {err: nxdomainDialError("gone.example.com")},
		"notfound.example.com": {status: http.StatusNotFound},
	})
	r, now := clockedReaper(cleaner, resolver, reaperTestGrace)

	// First pass: clocks start for the NXDOMAIN and 404 records; the
	// in-use record's stale clock is cancelled, not continued.
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("nothing may be deleted on the first observation, got %v", cleaner.deletes)
	}
	clocked := make(map[string]bool, len(cleaner.markers))
	for _, m := range cleaner.markers {
		clocked[m.RecordID] = true
	}
	if !clocked["2"] || !clocked["3"] {
		t.Fatalf("NXDOMAIN and 404 must both start grace clocks, got %v", cleaner.markers)
	}
	if clocked["1"] {
		t.Fatalf("an in-use handle must cancel its clock, got %v", cleaner.markers)
	}

	// Still within grace: no delete.
	*now = now.Add(reaperTestGrace - time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("no delete within the grace period, got %v", cleaner.deletes)
	}

	// Grace elapsed: both dead records are reaped, the in-use one kept.
	*now = now.Add(2 * time.Hour)
	r.sweep(context.Background())
	if got := claimDeletes(cleaner); !slices.Equal(got, []string{"2", "3"}) {
		t.Fatalf("expected records 2 and 3 reaped after grace, got %v", cleaner.deletes)
	}
}

func TestReaperUnverifiableWebDIDsNeverStartClock(t *testing.T) {
	// The fail-safe counterpart of the NXDOMAIN rule: SERVFAIL,
	// timeouts, refused connections, 5xx responses, wrong-id documents,
	// and the SSRF guard's refusal to dial a non-public host are all
	// inconclusive — no grace clock may EVER start on them, so they can
	// never turn into reaps.
	resolver := newScriptedWebResolver(map[string]webScript{
		"servfail.example.com": {err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
			Err: "server misbehaving", Name: "servfail.example.com", IsTemporary: true,
		}}},
		"stall.example.com": {err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
			Err: "i/o timeout", Name: "stall.example.com", IsTimeout: true,
		}}},
		"refused.example.com": {err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}},
		"broken.example.com":  {status: http.StatusInternalServerError},
		"wrong.example.com":   {doc: `{"id":"did:web:someone-else.example.com","alsoKnownAs":[]}`},
		// The SSRF guard refuses the loopback dial: the host exists in
		// DNS, we merely refuse to fetch it — inconclusive, never a
		// reap.  (resolver_test.go covers the guard end to end; here
		// the scripted error is the shape it produces.)
		"127.0.0.1": {err: &net.OpError{Op: "dial", Net: "tcp", Err: &unsafeDialError{network: "tcp", address: "127.0.0.1:443"}}},
	})
	records := []atprotoRecord{
		{ID: "1", Label: "serv", DID: "did:web:servfail.example.com"},
		{ID: "2", Label: "stall", DID: "did:web:stall.example.com"},
		{ID: "3", Label: "refuse", DID: "did:web:refused.example.com"},
		{ID: "4", Label: "broken", DID: "did:web:broken.example.com"},
		{ID: "5", Label: "wrongid", DID: "did:web:wrong.example.com"},
		{ID: "6", Label: "unsafe", DID: "did:web:127.0.0.1:9443"},
	}
	cleaner := &fakeCleaner{records: records}
	r, now := clockedReaper(cleaner, resolver, reaperTestGrace)

	// Two sweeps, the second far beyond any grace period: inconclusive
	// records are skipped every time — no clock ever starts, nothing is
	// ever reaped.
	for range 2 {
		r.sweep(context.Background())
		*now = now.Add(reaperTestGrace * 2)
	}
	if len(cleaner.markers) != 0 {
		t.Fatalf("inconclusive resolutions must never start a grace clock, got %v", cleaner.markers)
	}
	if got := claimDeletes(cleaner); len(got) != 0 {
		t.Fatalf("inconclusive resolutions must never reap, got %v", cleaner.deletes)
	}
}

// directoryScript is a plc.directory stand-in a test can flip between
// serving DID documents (from docs) and answering 429 with an optional
// Retry-After, while counting every request.  LOW-7 is about the reaper's
// request volume into the directory, so the tests assert on the number
// of fetches actually made.
type directoryScript struct {
	mu         sync.Mutex
	rateLimit  bool
	retryAfter string
	docs       map[string][]string // DID -> alsoKnownAs (missing = account using no handles)
	hits       atomic.Int32
}

func (s *directoryScript) setRateLimit(retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimit = true
	s.retryAfter = retryAfter
}

func (s *directoryScript) clearRateLimit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimit = false
}

func (s *directoryScript) handler(w http.ResponseWriter, r *http.Request) {
	s.hits.Add(1)
	s.mu.Lock()
	limit, ra, docs := s.rateLimit, s.retryAfter, s.docs
	s.mu.Unlock()
	if limit {
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	did := strings.TrimPrefix(r.URL.Path, "/")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"@context":    []string{"https://www.w3.org/ns/did/v1"},
		"id":          did,
		"alsoKnownAs": docs[did],
	})
}

// directoryTransport routes plc.directory fetches to the real stand-in
// server and scripts each did:web host's fetch per hostname, so one
// sweep can exercise both DID methods with their REAL verdict
// classification.
type directoryTransport struct {
	dir     *http.Transport
	dirHost string
	byHost  map[string]webScript
}

func (m directoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == m.dirHost {
		return m.dir.RoundTrip(req)
	}
	script, ok := m.byHost[req.URL.Hostname()]
	if !ok {
		return nil, fmt.Errorf("no script for host %q", req.URL.Hostname())
	}
	return script.roundTrip(req)
}

// resolver returns a real plcDirectory pointed at the stand-in (with
// optional per-host did:web scripts), so the reaper tests below run the
// REAL 429 detection and backoff end to end.
func (s *directoryScript) resolver(t *testing.T, byHost map[string]webScript) *plcDirectory {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	d := newPlcDirectory(srv.URL)
	d.hc = resolverHTTPClient(directoryTransport{
		dir:     &http.Transport{},
		dirHost: strings.TrimPrefix(srv.URL, "http://"),
		byHost:  byHost,
	})
	return d
}

func TestReaperPlcRateLimitDefersRestOfSweep(t *testing.T) {
	// LOW-7: the reaper's plc.directory fetch volume is attacker-steerable
	// (minting DIDs is free), so when the directory answers 429 the sweep
	// must stop fetching it: the remaining did:plc records are deferred
	// inconclusive — a rate limit must never start a grace clock — while
	// did:web judging is untouched.  Sweeps inside the Retry-After window
	// defer did:plc judging entirely; past it, fetching resumes.
	plc := func(s string) string { return "did:plc:" + s + strings.Repeat("a", 24-len(s)) }
	records := []atprotoRecord{
		{ID: "1", Label: "aa", DID: plc("b")},
		{ID: "2", Label: "ab", DID: plc("c")},
		{ID: "3", Label: "ac", DID: plc("d")},
		{ID: "4", Label: "web", DID: "did:web:unaffected.example.com"},
	}
	dir := &directoryScript{docs: map[string][]string{}} // healthy: accounts exist, use no handles
	resolver := dir.resolver(t, map[string]webScript{
		"unaffected.example.com": {status: http.StatusNotFound}, // affirmative: clock starts
	})
	cleaner := &fakeCleaner{records: records}
	r, now := clockedReaper(cleaner, resolver, reaperTestGrace)
	r.workers = 1 // serial fetches: after the first 429, no further directory request may go out

	dir.setRateLimit("60") // the directory's answer, with its backoff window
	logBuf := captureLog(t)
	r.sweep(context.Background())

	// Exactly one fetch: the first did:plc job got the 429; every other
	// did:plc record was deferred without fetching.
	if hits := dir.hits.Load(); hits != 1 {
		t.Fatalf("a 429 must stop the sweep's directory fetches, got %d fetches", hits)
	}
	if !strings.Contains(logBuf.String(), "rate limiting") {
		t.Fatalf("expected an operator-visible rate-limit line, got: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "REAPER: skipping") {
		t.Fatalf("deferrals are logged once per sweep, not per record, got: %s", logBuf.String())
	}
	// Deferred did:plc records start no grace clock; the did:web 404
	// (unaffected by the directory's state) does.
	clocked := make(map[string]bool, len(cleaner.markers))
	for _, m := range cleaner.markers {
		clocked[m.RecordID] = true
	}
	if clocked["1"] || clocked["2"] || clocked["3"] || !clocked["4"] {
		t.Fatalf("only the did:web record may start a clock during a directory rate limit, got %v", cleaner.markers)
	}

	// Still inside the Retry-After window: the next sweep defers did:plc
	// judging entirely — no fetch at all — while did:web judging
	// continues normally.
	*now = now.Add(10 * time.Second)
	r.sweep(context.Background())
	if hits := dir.hits.Load(); hits != 1 {
		t.Fatalf("sweeps inside the Retry-After window must not fetch plc.directory, got %d fetches", hits)
	}

	// Past the window the next sweep resumes normally: every did:plc
	// record is fetched and judged; the earlier deferrals started
	// nothing, so their clocks begin at THIS sweep's observation.
	dir.clearRateLimit()
	*now = now.Add(time.Hour)
	r.sweep(context.Background())
	if hits := dir.hits.Load(); hits != 4 {
		t.Fatalf("past the backoff window the sweep must resume directory fetches, got %d fetches", hits)
	}
	clocked = make(map[string]bool, len(cleaner.markers))
	for _, m := range cleaner.markers {
		clocked[m.RecordID] = true
		if m.RecordID != "4" && !m.Since.Equal(*now) {
			t.Fatalf("deferred records' clocks must start at the resuming sweep, got marker %v", m)
		}
	}
	for _, id := range []string{"1", "2", "3", "4"} {
		if !clocked[id] {
			t.Fatalf("all records must be clocked after the sweep resumes, got %v", cleaner.markers)
		}
	}
}

func TestReaperPlcRateLimitDefaultBackoff(t *testing.T) {
	// A 429 without a usable Retry-After falls back to a short default
	// window: the next sweep (still inside it) defers did:plc judging
	// with no fetch, and the sweep after it resumes.
	dir := &directoryScript{docs: map[string][]string{}}
	resolver := dir.resolver(t, nil)
	cleaner := &fakeCleaner{records: []atprotoRecord{{ID: "1", Label: "aa", DID: validPLC}}}
	r, now := clockedReaper(cleaner, resolver, reaperTestGrace)
	r.workers = 1

	dir.setRateLimit("") // 429, no Retry-After
	r.sweep(context.Background())
	if hits := dir.hits.Load(); hits != 1 {
		t.Fatalf("the sweep must stop at the first 429, got %d fetches", hits)
	}
	if len(cleaner.markers) != 0 {
		t.Fatalf("a rate limit must never start a grace clock, got %v", cleaner.markers)
	}

	*now = now.Add(30 * time.Second) // inside the default backoff window
	r.sweep(context.Background())
	if hits := dir.hits.Load(); hits != 1 {
		t.Fatalf("inside the default backoff window the sweep must not fetch plc.directory, got %d fetches", hits)
	}

	dir.clearRateLimit()
	*now = now.Add(time.Hour) // well past the window
	r.sweep(context.Background())
	if hits := dir.hits.Load(); hits != 2 {
		t.Fatalf("past the default window the sweep must resume directory fetches, got %d fetches", hits)
	}
	if len(cleaner.markers) != 1 {
		t.Fatalf("the stale account must be judged and clocked once fetching resumes, got %v", cleaner.markers)
	}
}

func TestReaperDedupesIdenticalDIDsPerSweep(t *testing.T) {
	// LOW-7's other half: the same DID may back several records — one
	// handle per DID per DOMAIN (not per instance), plus stacked pairs
	// and zone restores within a zone — and each duplicate fetch is free
	// attacker leverage against plc.directory's rate limit.  One sweep
	// resolves each distinct DID once and shares the verdict across
	// every record referencing it; per-record judgment still compares
	// the shared aka list against each record's own handle.
	did := validPLC
	dir := &directoryScript{docs: map[string][]string{
		// The account exists and uses exactly one of the three handles.
		did: {"at://dup." + testDomain},
	}}
	resolver := dir.resolver(t, nil)
	cleaner := &fakeCleaner{domainRecords: map[string][]atprotoRecord{
		testDomain: {
			{ID: "1", Label: "dup", DID: did},    // in use: no clock
			{ID: "2", Label: "shared", DID: did}, // duplicate within the zone: mismatch -> clock
		},
		"second.example": {
			{ID: "3", Label: "cross", DID: did}, // same DID in a second zone: mismatch -> clock
		},
	}}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	r := newReaper([]string{testDomain, "second.example"}, cleaner, resolver, reaperTestGrace)
	r.now = func() time.Time { return now }

	r.sweep(context.Background())

	// One distinct DID: exactly one directory fetch for the whole
	// sweep, across both zones.
	if hits := dir.hits.Load(); hits != 1 {
		t.Fatalf("one distinct DID must cost one directory fetch per sweep, got %d", hits)
	}
	// The shared verdict judged per record: only the handle actually in
	// use escapes a clock; the other two records share the same
	// resolution but mismatch on their own handles.
	clocked := make(map[string]bool, len(cleaner.markers))
	for _, m := range cleaner.markers {
		clocked[m.RecordID] = true
	}
	if clocked["1"] || !clocked["2"] || !clocked["3"] {
		t.Fatalf("all records share the DID's verdict; only the in-use handle may escape a clock, got %v", cleaner.markers)
	}
}
