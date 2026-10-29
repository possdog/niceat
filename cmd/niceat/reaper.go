package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// The handle reaper frees labels whose accounts no longer use them.
//
// DNS is the claim store, but the ACCOUNT is the authority: changing a
// Bluesky handle requires both the DNS record and setting the username
// in-app, so a record whose account no longer lists the handle in its DID
// document is a squatted label — someone else could be using it.  The
// reaper walks every managed zone, resolves each record's DID, and deletes
// records whose account does not use the handle, after a grace period.
//
// Safety properties:
//
//   - Only affirmative evidence counts.  A transient resolution failure
//     (network error, SERVFAIL, 5xx, a refused dial) never marks a handle
//     unused; the reaper skips and retries next pass.  An account that no
//     longer resolves at all DOES count — a directory 404, or a did:web
//     host the DNS system affirmatively reports nonexistent (the host IS
//     the identity, the same evidence class as a 404) — the account is
//     gone.  The resolver performs the classification; this side sees
//     only verdicts.
//   - The grace period exists because of a chicken-and-egg: users set
//     their username in-app AFTER the DNS record exists.  A mismatch must
//     persist for the whole grace period before the record is deleted.
//   - Grace clocks are stored in the zone itself, as TXT marker records
//     (see porkbun.go's graceMarker), keyed by Porkbun RECORD ID.  A
//     re-registered label creates a fresh record (fresh id) and therefore
//     a fresh grace clock automatically; the reaper can never delete a
//     brand-new claim using a stale clock from a previous owner.
//   - The reaper is fully stateless: every scrap of timing state lives in
//     the same zone as the claims it times.  Losing any part of it (a
//     marker deleted, a zone listing failing, a machine recreated)
//     restarts clocks and delays reaps — it can never delete anything
//     early, and ownership is still decided solely by the claim records.
//   - Resolution work is bounded: a sweep resolves records through a
//     small fixed worker pool under a wall-clock budget, so records whose
//     DIDs hang — plantable by anyone who can register — can neither
//     serialize the sweep nor balloon it past the -reap-once deadline.
//     Records the budget does not cover are deferred to the next sweep
//     and the deferral is logged; like any other "not reached this
//     pass", a deferral only ever delays a reap.
//   - The plc.directory fetch volume is attacker-steerable (minting
//     DIDs is free), so it is also bounded by deduplication — one fetch
//     per distinct DID per sweep, the verdict shared by every record
//     referencing it — and by backoff: a directory 429 stops directory
//     fetches for the rest of the sweep, honoring Retry-After, with the
//     remaining did:plc records deferred inconclusive (a rate limit
//     must never start grace clocks).  did:web judging is untouched by
//     the directory's state.

const (
	// defaultReaperInterval is the default time between reaper passes.
	defaultReaperInterval = 6 * time.Hour
	// defaultReaperGrace is the default time a mismatch must persist
	// before a record is deleted: generous for users who register then
	// set the handle days later, short enough that a squatted label
	// frees up within a month.
	defaultReaperGrace = 30 * 24 * time.Hour
	// reaperOnceDeadline caps a single -reap-once sweep.  Every network
	// call has its own timeout, but the deadline is the backstop that
	// guarantees the scheduled reaper Machine's process exits even under
	// pathological conditions — that Machine's lifetime is exactly its
	// process's lifetime, so a stuck sweep would be a stuck bill.
	reaperOnceDeadline = 10 * time.Minute
	// reaperResolveWorkers is the size of the per-zone DID-resolution
	// pool.  Resolution used to be serial, so every hanging fetch
	// stalled the whole sweep for the per-fetch timeout (the HIGH-3
	// audit measured six hanging DIDs adding 60 s, with ~60 of them
	// enough to eat an entire -reap-once deadline).  Six workers bound
	// that damage to waves — N hanging DIDs now cost ceil(N/6) fetch
	// timeouts, not N — while keeping concurrent egress modest and polite
	// to plc.directory, and sustaining ~12-20 resolutions/s at the
	// directory's measured 0.3-0.5 s latency: a zone at Porkbun's
	// 2,500-record cap clears in 2-4 minutes, inside the budget below.
	reaperResolveWorkers = 6
	// sweepResolveBudget caps the wall time one zone's resolution phase
	// may spend before the sweep defers its remaining records to the
	// next pass (logged, operator-visible).  Four minutes sits above what
	// a healthy zone needs even at Porkbun's 2,500-record cap (2-4 min
	// across the pool), so a deferral signals a real anomaly — massed
	// hanging endpoints or a directory slowdown — rather than zone
	// size; and two zones' budgets plus the bounded in-flight drain
	// (workers x per-fetch timeout) still finish well inside
	// -reap-once's 10-minute backstop.
	sweepResolveBudget = 4 * time.Minute
	// plcBackoffDefault is how long did:plc judging stays deferred after
	// plc.directory answers 429 without a usable Retry-After.  Short
	// enough that the next sweep (6 h in-process, daily scheduled) is
	// effectively never inside it; long enough that a hammering sweep
	// does not immediately re-trigger the limit.
	plcBackoffDefault = time.Minute
)

// zoneCleaner is the DNS-backend surface the reaper needs: enumerate and
// delete the zone's _atproto records, and read and write the grace-marker
// records that time them.  porkbunProvisioner implements it; tests use
// fakes.
type zoneCleaner interface {
	listAtproto(ctx context.Context, domain string) ([]atprotoRecord, error)
	listGraceMarkers(ctx context.Context, domain string) ([]graceMarker, error)
	createGraceMarker(ctx context.Context, domain, label, recordID string, since time.Time) error
	deleteRecord(ctx context.Context, domain, id string) error
}

// reaper is the background task that frees unused handles.
type reaper struct {
	domains  []string
	cleaner  zoneCleaner
	resolver didResolver
	grace    time.Duration
	// now is the clock, injectable for tests.
	now func() time.Time
	// workers is the per-zone resolution pool size and budget the
	// resolution-phase wall-clock allowance; defaults come from the
	// constants above, and tests shrink them to keep timing assertions
	// fast and deterministic.
	workers int
	budget  time.Duration
	// resume maps each domain to the sort key of the first record the
	// previous sweep DEFERRED, so the next sweep resumes there instead of
	// re-judging the same prefix and starving the rest of the zone
	// forever.  This is a process-local scheduling hint, not reaper state
	// in the statelessness sense above: no record's fate depends on it,
	// and losing it — every -reap-once run starts with an empty map —
	// merely re-judges records from the top of the zone, which is
	// idempotent (clocks continue, reaping still needs fresh affirmative
	// evidence).
	resume map[string]string
	// plc.directory backoff (LOW-7): the reaper's directory fetch volume
	// is attacker-steerable, so when the directory answers 429 the sweep
	// stops fetching it and defers did:plc judging instead of amplifying
	// the refusal — a rate limit must not start grace clocks, and
	// hammering the directory deepens the limit and stops reaping
	// entirely.  mu guards both fields against the worker pool; sweeps
	// themselves run serially.  Like `resume`, this is a process-local
	// scheduling hint, not reaper state in the statelessness sense: a
	// deferral only ever delays a judgment.
	mu              sync.Mutex
	dirBackoffUntil time.Time // no directory fetch before this (Retry-After honored)
	plcStop         bool      // a 429 was seen this sweep: no more directory fetches until the next sweep
}

func newReaper(domains []string, cleaner zoneCleaner, resolver didResolver, grace time.Duration) *reaper {
	return &reaper{
		domains:  domains,
		cleaner:  cleaner,
		resolver: resolver,
		grace:    grace,
		now:      time.Now,
		workers:  reaperResolveWorkers,
		budget:   sweepResolveBudget,
		resume:   make(map[string]string),
	}
}

// run executes a pass immediately and then every interval, until ctx is
// done.  The immediate pass is deliberate: a server that was down for a
// while (or is freshly deployed) sweeps right away instead of waiting a
// full interval while stale records sit.
func (r *reaper) run(ctx context.Context, interval time.Duration) {
	r.sweep(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
		}
	}
}

// sweep runs one pass over every managed zone.  It returns an error when
// any zone could not be swept — run() ignores it (the failures are already
// logged and the next pass retries), while -reap-once uses it as its exit
// status so a scheduled Machine run surfaces the failure.
func (r *reaper) sweep(ctx context.Context) error {
	r.beginSweepPlc()
	// One verdict per distinct DID per sweep, shared across zones: the
	// same DID can back records in several zones (the per-DID cap is per
	// domain, not per instance) and duplicated records within a zone
	// (stacked pairs, zone restores), and every duplicate fetch is free
	// attacker leverage against plc.directory's rate limit (LOW-7).
	verdicts := &didVerdicts{seen: make(map[string]resolution)}
	var failed []string
	for _, domain := range r.domains {
		if err := r.sweepZone(ctx, domain, verdicts); err != nil {
			log.Printf("ERROR: reaper skipping %s: %v", domain, err)
			failed = append(failed, domain)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not sweep: %s", strings.Join(failed, ", "))
	}
	return nil
}

// beginSweepPlc arms this sweep's plc.directory policy: a sweep that
// starts inside a previous sweep's backoff window defers did:plc judging
// from the first record (no fetches at all), while a fresh window starts
// optimistic — a healthy directory costs nothing.
func (r *reaper) beginSweepPlc() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The zero dirBackoffUntil of a fresh reaper means "never limited";
	// otherwise a sweep that STARTS inside the window also defers.
	r.plcStop = !r.dirBackoffUntil.IsZero() && r.now().Before(r.dirBackoffUntil)
}

// stopPlc halts plc.directory fetches for the rest of the sweep and
// records the directory's requested window — its Retry-After, or a sane
// default when absent or unparseable — so subsequent sweeps inside the
// window also defer did:plc judging instead of re-triggering the limit
// on their first fetch.
func (r *reaper) stopPlc(retryAfter time.Duration) {
	if retryAfter <= 0 {
		retryAfter = plcBackoffDefault
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plcStop = true
	if until := r.now().Add(retryAfter); until.After(r.dirBackoffUntil) {
		r.dirBackoffUntil = until
	}
}

// plcStopped reports whether this sweep must not fetch plc.directory.
func (r *reaper) plcStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.plcStop
}

// errPlcDeferred is the verdict handed to did:plc records while the sweep
// is backed off plc.directory: no fetch was made, so nothing is known
// about the DID — inconclusive like any resolution error, never a basis
// for a clock or a reap.  A sentinel rather than a rateLimitedError,
// because the directory was not even asked.
var errPlcDeferred = errors.New("did:plc judging deferred: plc.directory backoff")

// isPlcDeferral reports whether err is a directory-backoff deferral:
// either the sentinel for records never fetched, or the 429 itself for
// the fetch that triggered the stop.  Both are inconclusive; the reaper
// logs them once per sweep instead of per record, because an attacker
// can plant thousands of them.
func isPlcDeferral(err error) bool {
	if errors.Is(err, errPlcDeferred) {
		return true
	}
	var rl *rateLimitedError
	return errors.As(err, &rl)
}

// didVerdicts is the per-sweep cache of resolved DID verdicts, shared
// across zones.  Zone sweeps run serially, but their worker pools run
// concurrently, so the map is mutex-guarded.  Deliberately per sweep, not
// per process: a verdict is seconds-old evidence, and a later sweep must
// never judge on a stale fetch — accounts move handles between sweeps.
type didVerdicts struct {
	mu   sync.Mutex
	seen map[string]resolution
}

func (v *didVerdicts) get(did string) (resolution, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	res, ok := v.seen[did]
	return res, ok
}

func (v *didVerdicts) put(did string, res resolution) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen[did] = res
}

// resolveDid resolves one distinct DID for this sweep.  The per-sweep
// verdict cache is consulted first (the same DID may back records in
// several zones); then, for did:plc only, the directory backoff: while
// the sweep is stopped, the record is deferred without any fetch — the
// directory asked us to stop, and attacker-minted DIDs must not turn
// its refusal into more request volume.  A 429 arms that stop.  did:web
// fetches are a different origin and are never affected by it.
func (r *reaper) resolveDid(ctx context.Context, did string, verdicts *didVerdicts) resolution {
	if res, ok := verdicts.get(did); ok {
		return res
	}
	if strings.HasPrefix(did, "did:plc:") && r.plcStopped() {
		return resolution{err: errPlcDeferred}
	}
	aka, registered, err := r.resolver.resolve(ctx, did)
	if err != nil {
		var rl *rateLimitedError
		if errors.As(err, &rl) {
			r.stopPlc(rl.retryAfter)
		}
	}
	res := resolution{aka: aka, registered: registered, err: err}
	verdicts.put(did, res)
	return res
}

// sweepZone judges every claim record in one zone and prunes its orphaned
// grace markers.
//
// Both listings (claims and markers) must succeed before anything is
// judged or pruned: a marker pruning decision needs the full record list
// (a marker is orphaned only if its record is absent from a COMPLETE
// listing), and a record cannot be judged without its clock.  Skipping the
// zone on failure leaves everything untouched — reaps delayed, never
// hastened.
//
// Resolution is bounded (see sweepResolve): records the budget does not
// cover are deferred to the next sweep.  Deferred records still count as
// live here — the listing was complete, so their markers must survive
// untouched.
func (r *reaper) sweepZone(ctx context.Context, domain string, verdicts *didVerdicts) error {
	records, err := r.cleaner.listAtproto(ctx, domain)
	if err != nil {
		return fmt.Errorf("listing records: %w", err)
	}
	markers, err := r.cleaner.listGraceMarkers(ctx, domain)
	if err != nil {
		return fmt.Errorf("listing grace markers: %w", err)
	}

	byRecord := make(map[string][]graceMarker, len(markers))
	for _, m := range markers {
		if m.Corrupt {
			// Matches no record id; pruning below removes it.
			continue
		}
		byRecord[m.RecordID] = append(byRecord[m.RecordID], m)
	}

	live := make(map[string]bool, len(records))
	for _, rec := range records {
		live[rec.ID] = true
	}

	if len(records) > 0 {
		r.sweepResolve(ctx, domain, records, byRecord, verdicts)
	}

	// Prune markers whose record no longer exists (reaped, manually
	// deleted) or whose content is corrupt.  Safe only because the
	// record listing above succeeded — an absent record was truly
	// observed absent, not merely unreachable.
	for _, m := range markers {
		if live[m.RecordID] {
			continue
		}
		if err := r.cleaner.deleteRecord(ctx, domain, m.ID); err != nil {
			log.Printf("ERROR: reaper could not delete grace marker for record %s: %v", m.RecordID, err)
		}
	}
	return nil
}

// resolution is one completed DID lookup: the account's alsoKnownAs list,
// whether it still exists, or the error that made the lookup
// inconclusive.  It is the pool-worker half of judging a record; enact is
// the decision half.
type resolution struct {
	aka        []string
	registered bool
	err        error
}

// sweepResolve runs one zone's resolution phase and applies the resulting
// judgments.  Records are resolved through a bounded worker pool in a
// deterministic order, starting where the previous sweep left off, and the
// feed stops once the sweep budget is spent — so hanging fetches cannot
// serialize or balloon a sweep past the -reap-once backstop.  Records
// beyond the budget are deferred (logged) rather than judged; the
// judgment half below still runs for everything that WAS resolved, so
// grace-marker and deletion writes are not lost to a deferral.
func (r *reaper) sweepResolve(ctx context.Context, domain string, records []atprotoRecord, byRecord map[string][]graceMarker, verdicts *didVerdicts) {
	// Deterministic order — sorted by label, then record id — so that a
	// budget-exhausted sweep defers a reproducible suffix and the resume
	// cursor advances through the zone instead of a different subset
	// each pass.
	ordered := slices.Clone(records)
	slices.SortFunc(ordered, func(a, b atprotoRecord) int {
		if c := strings.Compare(a.Label, b.Label); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	start := r.resumeIndex(domain, ordered)
	// This sweep's rotation is the contiguous segment from the resume
	// cursor to the end of the zone — a sweep never wraps, so each pass
	// judges a forward-moving segment and the next resumes where this one
	// stopped.  A pass that reaches the zone's end has covered everything
	// and the next starts from the top.
	limit := len(ordered) - start

	// LOW-7: identical DIDs within this pass's rotation are resolved
	// once.  The first record referencing a DID is its owner and does
	// the fetch; every other record reuses the owner's verdict.  The
	// owner is by definition the FIRST occurrence, and submission below
	// is a contiguous prefix of the segment, so a submitted record's
	// owner is always submitted too — its verdict is ready by the time
	// the judgment half runs.
	firstDid := make(map[string]int, limit)
	for i := 0; i < limit; i++ {
		did := ordered[start+i].DID
		if _, ok := firstDid[did]; !ok {
			firstDid[did] = start + i
		}
	}

	// Resolution phase: a fixed pool consumes records from the front of
	// the segment.  The feed blocks while every worker is busy, and the
	// budget is re-checked between sends, so at most `workers` fetches
	// are still in flight when the loop breaks — each bounded by the
	// resolver's per-fetch timeout.
	workers := r.workers
	if workers < 1 {
		workers = 1
	}
	results := make([]resolution, len(ordered))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers && w < limit; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if firstDid[ordered[i].DID] != i {
					// Duplicate DID: the owner's fetch covers it.
					continue
				}
				results[i] = r.resolveDid(ctx, ordered[i].DID, verdicts)
			}
		}()
	}
	phaseStart := time.Now()
	submitted := 0
	for ; submitted < limit; submitted++ {
		// The first record is always submitted, budget or not: every
		// sweep then makes at least one record of forward progress, so
		// a starved zone can never wedge entirely.
		if submitted > 0 && time.Since(phaseStart) > r.budget {
			break
		}
		jobs <- start + submitted
	}
	close(jobs)
	wg.Wait()

	// Judgment half: serial, in submission order.  Keeping every zone
	// write (grace markers, deletes) on the sweep goroutine means the
	// cleaner never sees concurrent calls, and judged records still get
	// their writes even when the rest of the zone was deferred.
	deferredPlc := 0
	for i := 0; i < submitted; i++ {
		idx := start + i
		res := results[firstDid[ordered[idx].DID]]
		if isPlcDeferral(res.err) {
			deferredPlc++
		}
		r.enact(ctx, domain, ordered[idx], byRecord[ordered[idx].ID], res)
	}
	if deferredPlc > 0 {
		// One line for the whole zone rather than one per record: an
		// attacker can plant thousands of deferrals, and the fail-safe
		// direction is identical for all of them (no fetch, no clock,
		// judged fresh next sweep).
		log.Printf("REAPER: %s: plc.directory is rate limiting this deployment; did:plc judging deferred for %d of %d records this pass (resumes after the Retry-After window)",
			domain, deferredPlc, submitted)
	}

	if submitted < limit {
		r.resume[domain] = recordSortKey(ordered[start+submitted])
		log.Printf("REAPER: %s: resolution budget (%s) exhausted after %d of %d records in this pass's rotation; deferring %d to the next sweep",
			domain, r.budget, submitted, limit, limit-submitted)
		return
	}
	// The rotation reached the end of the zone: the next sweep starts
	// from the top of the sorted order.
	delete(r.resume, domain)
}

// recordSortKey is the deterministic identity a record is ordered and
// resumed by within its zone.
func recordSortKey(rec atprotoRecord) string {
	return rec.Label + "\x00" + rec.ID
}

// resumeIndex returns the index in ordered (the zone's records in
// deterministic order) where this sweep should start: the record the
// previous sweep deferred first.  Losing the hint (a restart) restarts at
// the top of the zone, which is always safe — see the resume field.
func (r *reaper) resumeIndex(domain string, ordered []atprotoRecord) int {
	if len(ordered) == 0 {
		return 0
	}
	key, ok := r.resume[domain]
	if !ok {
		return 0
	}
	i := sort.Search(len(ordered), func(i int) bool { return recordSortKey(ordered[i]) >= key })
	if i == len(ordered) {
		// Every record sorts before the cursor (they changed): wrap to
		// the top rather than skipping the zone.
		return 0
	}
	return i
}

// enact applies one completed resolution to its record, deciding the
// record's fate using its grace markers.  It runs only on the sweep
// goroutine (the pool resolves; this serializes), so the cleaner never
// sees concurrent calls.
func (r *reaper) enact(ctx context.Context, domain string, rec atprotoRecord, markers []graceMarker, res resolution) {
	handle := rec.Label + "." + domain

	if res.err != nil {
		if isPlcDeferral(res.err) {
			// Inconclusive (no clock, no reap) and silent: the
			// sweep-level line already reported the backoff, and a
			// per-record line would only be thousands of copies of
			// it for attacker-minted DIDs.
			return
		}
		// Inconclusive: skip without touching the clock — a running
		// grace period keeps its original start, and reaping always
		// requires fresh affirmative evidence.
		log.Printf("REAPER: skipping %s: %v", handle, res.err)
		return
	}
	if res.registered && akaHasHandle(res.aka, handle) {
		// In use: the account has adopted the handle, so any pending
		// grace clock is cancelled.  A failed delete leaves the
		// marker behind for the next pass to retry.
		for _, m := range markers {
			if err := r.cleaner.deleteRecord(ctx, domain, m.ID); err != nil {
				log.Printf("ERROR: reaper could not delete grace marker for %s: %v", handle, err)
			}
		}
		return
	}

	// Affirmative mismatch: either the account is gone (unregistered) or
	// it exists and does not list this handle.
	since, ok := graceStart(markers)
	if !ok {
		if err := r.cleaner.createGraceMarker(ctx, domain, rec.Label, rec.ID, r.now()); err != nil {
			// No clock started: the record cannot be reaped this
			// pass, and the next pass retries — reaps are only
			// ever delayed by failures here.
			log.Printf("ERROR: reaper could not start the grace clock for %s: %v", handle, err)
			return
		}
		log.Printf("REAPER: %s -> %s is not in use; grace period starts", handle, rec.DID)
		return
	}
	if elapsed := r.now().Sub(since); elapsed < r.grace {
		return
	}
	if err := r.cleaner.deleteRecord(ctx, domain, rec.ID); err != nil {
		// The record stays, and so do its markers — the delete is
		// retried on the next pass rather than grace restarting.
		log.Printf("ERROR: reaper could not delete %s -> %s: %v", handle, rec.DID, err)
		return
	}
	for _, m := range markers {
		// Best effort: a marker left behind is orphaned (its record
		// is gone) and the pruning step removes it, this pass or the
		// next.
		_ = r.cleaner.deleteRecord(ctx, domain, m.ID)
	}
	// The same audit trail as registrations: the zone cannot answer
	// "who asked, and what did the reaper do", so this line is the
	// dispute-resolution record for reaps too.
	log.Printf("AUDIT: reaped %s -> %s (unused since %s)", handle, rec.DID, since.Format(time.RFC3339))
}

// graceStart returns the clock to judge a record by: the MOST RECENT start
// time among its markers.  Multiple markers for one record are a corruption
// case (a create race or a manual edit); taking the latest keeps the
// fail-safe direction, since a too-recent clock only delays a reap.
func graceStart(markers []graceMarker) (time.Time, bool) {
	var latest time.Time
	ok := false
	for _, m := range markers {
		if m.Since.After(latest) {
			latest = m.Since
			ok = true
		}
	}
	return latest, ok
}
