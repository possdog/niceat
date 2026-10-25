package main

import (
	"context"
	"fmt"
	"log"
	"strings"
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
//     (network error, 5xx) never marks a handle unused; the reaper skips
//     and retries next pass.  An account that no longer resolves at all
//     (directory 404) does count — the account is gone.
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
}

func newReaper(domains []string, cleaner zoneCleaner, resolver didResolver, grace time.Duration) *reaper {
	return &reaper{
		domains:  domains,
		cleaner:  cleaner,
		resolver: resolver,
		grace:    grace,
		now:      time.Now,
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
	var failed []string
	for _, domain := range r.domains {
		if err := r.sweepZone(ctx, domain); err != nil {
			log.Printf("ERROR: reaper skipping %s: %v", domain, err)
			failed = append(failed, domain)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not sweep: %s", strings.Join(failed, ", "))
	}
	return nil
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
func (r *reaper) sweepZone(ctx context.Context, domain string) error {
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
		r.judge(ctx, domain, rec, byRecord[rec.ID])
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

// judge decides one record's fate using its grace markers.
func (r *reaper) judge(ctx context.Context, domain string, rec atprotoRecord, markers []graceMarker) {
	handle := rec.Label + "." + domain

	aka, registered, err := r.resolver.resolve(ctx, rec.DID)
	if err != nil {
		// Inconclusive: skip without touching the clock — a running
		// grace period keeps its original start, and reaping always
		// requires fresh affirmative evidence.
		log.Printf("REAPER: skipping %s: %v", handle, err)
		return
	}
	if registered && akaHasHandle(aka, handle) {
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
