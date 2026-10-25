package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
//   - Grace clocks are keyed by Porkbun RECORD ID, not handle.  A
//     re-registered label creates a fresh record (fresh id) and therefore
//     a fresh grace clock automatically; the reaper can never delete a
//     brand-new claim using a stale clock from a previous owner.
//   - The clock state (reaper_state.json) is a timing cache, NOT part of
//     the claim store: losing or corrupting it only restarts grace clocks,
//     delaying reaps — it can never delete anything early, and ownership
//     is still decided solely by DNS.

const (
	// reaperStateFile stores first-mismatch timestamps in the working
	// directory (gitignored, instance-specific like the credentials).
	reaperStateFile = "reaper_state.json"
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

// zoneCleaner is the DNS-backend surface the reaper needs: enumerate the
// zone's _atproto records and delete one by id.  porkbunProvisioner
// implements it; tests use fakes.
type zoneCleaner interface {
	listAtproto(ctx context.Context, domain string) ([]atprotoRecord, error)
	deleteRecord(ctx context.Context, domain, id string) error
}

// reaperState maps "<domain>/<record id>" to the time the record's
// first mismatch (handle not used by its account) was observed.
type reaperState map[string]time.Time

// reaper is the background task that frees unused handles.
type reaper struct {
	domains   []string
	cleaner   zoneCleaner
	resolver  didResolver
	statePath string
	grace     time.Duration
	// now is the clock, injectable for tests.
	now func() time.Time
}

func newReaper(domains []string, cleaner zoneCleaner, resolver didResolver, statePath string, grace time.Duration) *reaper {
	return &reaper{
		domains:   domains,
		cleaner:   cleaner,
		resolver:  resolver,
		statePath: statePath,
		grace:     grace,
		now:       time.Now,
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

// sweep runs one pass over every managed zone and persists the grace state.
// It returns an error when any zone could not be listed or the state could
// not be persisted — run() ignores it (the failures are already logged and
// the next pass retries), while -reap-once uses it as its exit status so a
// scheduled Machine run surfaces the failure.
func (r *reaper) sweep(ctx context.Context) error {
	state, err := loadReaperState(r.statePath)
	if err != nil {
		// A missing file is the normal first run; a corrupt one is
		// treated like a lost one — clocks restart, reaps are only
		// delayed.  Either way, log it so the operator knows.
		log.Printf("WARN: reaper starting with fresh grace state (%v)", err)
		state = reaperState{}
	}

	// seen collects the state keys of records observed this pass; keys
	// for records that vanished (reaped, manually deleted) are pruned
	// so the state file does not grow without bound.  Pruning is
	// restricted to zones that listed successfully: a failed listing
	// means those records were not observed at all, and erasing their
	// clocks would silently restart grace for the whole zone.
	seen := make(map[string]bool)
	listed := make(map[string]bool, len(r.domains))
	var failed []string
	for _, domain := range r.domains {
		records, err := r.cleaner.listAtproto(ctx, domain)
		if err != nil {
			log.Printf("ERROR: reaper could not list %s: %v", domain, err)
			failed = append(failed, domain)
			continue
		}
		listed[domain] = true
		for _, rec := range records {
			seen[domain+"/"+rec.ID] = true
			r.judge(ctx, state, domain, rec)
		}
	}
	for key := range state {
		domain, _, _ := strings.Cut(key, "/")
		if listed[domain] && !seen[key] {
			delete(state, key)
		}
	}

	if err := saveReaperState(r.statePath, state); err != nil {
		log.Printf("ERROR: reaper could not persist grace state: %v", err)
		return fmt.Errorf("persist grace state: %w", err)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not list: %s", strings.Join(failed, ", "))
	}
	return nil
}

// judge decides one record's fate and updates the grace state in place.
func (r *reaper) judge(ctx context.Context, state reaperState, domain string, rec atprotoRecord) {
	handle := rec.Label + "." + domain
	key := domain + "/" + rec.ID

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
		// grace clock is cancelled.
		delete(state, key)
		return
	}

	// Affirmative mismatch: either the account is gone (unregistered) or
	// it exists and does not list this handle.
	firstSeen, ok := state[key]
	if !ok {
		state[key] = r.now()
		log.Printf("REAPER: %s -> %s is not in use; grace period starts", handle, rec.DID)
		return
	}
	if elapsed := r.now().Sub(firstSeen); elapsed < r.grace {
		return
	}
	if err := r.cleaner.deleteRecord(ctx, domain, rec.ID); err != nil {
		// The record stays; the clock stays too, so the delete is
		// retried on the next pass rather than restarted.
		log.Printf("ERROR: reaper could not delete %s -> %s: %v", handle, rec.DID, err)
		return
	}
	delete(state, key)
	// The same audit trail as registrations: the zone cannot answer
	// "who asked, and what did the reaper do", so this line is the
	// dispute-resolution record for reaps too.
	log.Printf("AUDIT: reaped %s -> %s (unused since %s)", handle, rec.DID, firstSeen.Format(time.RFC3339))
}

// loadReaperState reads the grace clock state; a missing file is an empty
// state and no error, a corrupt file is an error the caller logs before
// continuing with an empty state.
func loadReaperState(path string) (reaperState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reaperState{}, nil
		}
		return nil, err
	}
	state := reaperState{}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("state file corrupt: %w", err)
	}
	return state, nil
}

// saveReaperState writes the grace clock state atomically (temp file +
// rename), so a crash mid-write cannot corrupt the previous state.
func saveReaperState(path string, state reaperState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
