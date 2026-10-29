package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Porkbun API constants.  Base URL, endpoints and credential layout per
// the v3 docs (https://porkbun.com/api/json/v3/documentation); create
// semantics verified against the sandbox API on 2026-10-15.
const (
	porkbunBaseURL = "https://api.porkbun.com/api/json/v3"
	// Credentials are strictly per domain: production Porkbun keys are
	// scoped (dns scope, single-domain allowlist), so the credentials
	// document maps each managed domain to its own key pair, in one
	// place (porkbun_keys.json, or the PORKBUN_CREDENTIALS environment
	// variable on Fly).  There is deliberately no global fallback pair —
	// a shared pair would defeat the scoping's blast-radius containment.
	porkbunCredsFile = "porkbun_keys.json"
	porkbunCredsEnv  = "PORKBUN_CREDENTIALS"
	porkbunTimeout   = 10 * time.Second
	porkbunRecordTTL = "300"
	// porkbunDuplicateCode is Porkbun's machine-readable code for an exact
	// duplicate create (same name, type and content).
	porkbunDuplicateCode = "DUPLICATE_RECORD"
	// porkbunMaxResponse caps how much of a Porkbun response we buffer;
	// the responses we use are tiny, and a bounded read cannot be abused.
	porkbunMaxResponse = 1 << 20 // 1 MiB
	// zoneListingTTL bounds how long a cached zone listing may serve the
	// registration path's availability and per-DID cap lookups.  Single-
	// digit seconds: long enough that a burst of probes shares one
	// Porkbun call (the shared per-domain key is the exhaustible
	// resource), short enough that a claim made outside this process — a
	// manual Porkbun edit, another instance — is honored almost
	// immediately.
	zoneListingTTL = 5 * time.Second
)

// Grace-marker constants: the reaper stores its grace clocks in the zone
// itself, as TXT records the way the claims are.  That keeps the reaper
// fully stateless — a redeploy, a recreated Machine, or a lost state file
// can no longer restart grace clocks, because there is no state file.
const (
	// graceMarkerPrefix is the record-name prefix under which grace
	// clocks live: _niceat-grace.<label>.<domain>, one marker per
	// _atproto record.  The namespace is exclusively niceat's — user
	// labels are validated without underscores and every user claim
	// lives under the _atproto prefix, so no claim can ever collide
	// with (or be mistaken for) a marker.
	graceMarkerPrefix = "_niceat-grace"
	// The marker content is exactly four whitespace-separated fields:
	// the tag, a format version (unknown versions are treated as
	// corrupt, which only restarts clocks — the fail-safe direction),
	// the _atproto record id the clock belongs to, and the first
	// mismatch time.
	graceMarkerTag     = "niceat-grace"
	graceMarkerVersion = "v1"
)

// errDuplicateRecord reports that Porkbun refused a create because the exact
// record already exists.  It is a sentinel (not a wrapped error) so callers
// can branch on it with errors.Is.
var errDuplicateRecord = errors.New("porkbun: duplicate record")

// errBackendUnavailable reports that Porkbun could not serve the request:
// an HTML error page (what the API serves when the key is over its rate
// limit) or an HTTP 429/5xx status.  Every visitor of a domain shares that
// domain's single API key, so this is a whole-domain condition — callers
// shed load (503 + Retry-After) instead of answering 500 as if each
// individual request had failed.
var errBackendUnavailable = errors.New("porkbun: backend unavailable")

// porkbunCreds is one domain's Porkbun API key pair, as scoped in the
// Porkbun dashboard (dns scope, that domain only).
type porkbunCreds struct {
	apiKey    string
	secretKey string
}

// porkbunProvisioner provisions _atproto TXT records through the Porkbun
// DNS API.  DNS is the claim store — there is deliberately no local ledger:
// the existence and content of the TXT record are the single source of
// truth, so nothing can desync (not the reaper's grace-clock file, which
// only affects when a record is deleted and never who owns it, not manual
// dashboard edits, not zone restores).
//
// Sandbox-verified semantics this implementation relies on:
//
//   - An exact repeat create returns HTTP 400 DUPLICATE_RECORD with the
//     existing record's id in existingId — not a second record.
//   - A create with the same name but different content SUCCEEDS and stacks
//     a second TXT record on the name.  Per the atproto handle spec
//     ("only a single valid record should exist... resolution should fail"
//     otherwise), that breaks the handle for both parties.  This is why
//     registration must check for an existing record before creating:
//     create alone is not put-if-absent.
//
// The check-then-create race between two concurrent requests for the same
// label is closed by the handler's per-label lock (app.go); this client is
// safe to use under it.
//
// Credentials are held per domain and selected on every call by the same
// domain that is interpolated into the endpoint path, so a request for one
// community can never be authenticated (or billed, or rate-limited) with
// another's key.
//
// The listing cache is deliberately NOT a claim ledger: DNS remains the
// claim store.  The cache only saves Porkbun calls, and every write this
// process makes drops the affected zone's entry (see call), so this
// process's own records are always visible to the next lookup.
type porkbunProvisioner struct {
	creds   map[string]porkbunCreds
	baseURL string
	hc      *http.Client
	// cache briefly holds each domain's zone listing for the
	// registration path; see zoneCache.
	cache zoneCache
}

// newPorkbunProvisioner returns a client for the given per-domain
// credential pairs.  An empty baseURL selects the production endpoint;
// tests point it at an httptest server.
func newPorkbunProvisioner(creds map[string]porkbunCreds, baseURL string) *porkbunProvisioner {
	if baseURL == "" {
		baseURL = porkbunBaseURL
	}
	return &porkbunProvisioner{
		creds:   creds,
		baseURL: baseURL,
		hc:      &http.Client{Timeout: porkbunTimeout},
	}
}

// porkbunCredsEntry is the JSON shape of one domain's entry in the
// credentials document (porkbunCredsFile / porkbunCredsEnv).
type porkbunCredsEntry struct {
	APIKey    string `json:"api_key"`
	SecretKey string `json:"secret_key"`
}

// loadPorkbunCredentials resolves one API key pair per managed domain from
// a single domain-keyed credentials document: an object mapping domain →
// {api_key, secret_key}.  The document comes from the PORKBUN_CREDENTIALS
// environment variable, or — when that is unset or empty — from
// porkbun_keys.json under dir (the service working directory; the file is
// gitignored).
//
// The environment wins because that is Fly.io's secrets model
// (fly secrets set): in production the credentials arrive as an
// environment variable, and a stale porkbun_keys.json left over from local
// development must not shadow what the operator set with fly secrets set.
// The file remains the local-development path, where no secret manager
// exists.  Either way the document is per domain, mirroring the per-domain
// scoping of production Porkbun keys — there is deliberately no global
// fallback pair, as a shared pair would defeat the scoping's
// blast-radius containment.
//
// Mapping keys are validated and normalized like configured domains
// (parseDomain, lowercase, FQDN dot stripped), so an entry can be spelled
// "WAGG.ING" or "wagg.ing." interchangeably.  An entry no configured domain
// claims is a startup error naming it — with one document holding every
// domain, such an entry is almost certainly a typo for a managed domain,
// and silently ignoring it would leave the intended domain without keys.
//
// ok=false means no credentials exist at all (no env value, no file) and
// the caller should fall back to the in-memory provisioner.  Anything in
// between is a configuration error, not a fallback: a present-but-unparseable
// document, a key value that is empty or whitespace-only, or a configured
// domain missing from the document all fail loudly, because serving a
// domain whose records could never actually be published would silently
// eat registrations.
func loadPorkbunCredentials(dir string, domains []string) (map[string]porkbunCreds, bool, error) {
	envJSON := strings.TrimSpace(os.Getenv(porkbunCredsEnv))
	if envJSON == "" {
		b, err := os.ReadFile(filepath.Join(dir, porkbunCredsFile))
		if errors.Is(err, fs.ErrNotExist) {
			// No credentials anywhere: the caller falls back to the
			// in-memory provisioner (local development only).
			return nil, false, nil
		} else if err != nil {
			return nil, false, fmt.Errorf("read %s: %w", porkbunCredsFile, err)
		}
		envJSON = string(b)
	}

	// Strict decode: an unknown field is a typo and must fail loudly,
	// exactly like the main config.
	var entries map[string]porkbunCredsEntry
	dec := json.NewDecoder(strings.NewReader(envJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", porkbunCredsSource(envJSON), err)
	}

	// Normalize the mapping keys and reject entries no configured domain
	// claims (a typo would otherwise silently strand the intended domain).
	byDomain := make(map[string]porkbunCredsEntry, len(entries))
	normalized := make(map[string]string, len(entries)) // normalized key -> as written
	for raw, entry := range entries {
		d, err := parseDomain(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: invalid domain %q: %v", porkbunCredsSource(envJSON), raw, err)
		}
		if prev := normalized[d]; prev != "" && prev != raw {
			return nil, false, fmt.Errorf("%s: %q and %q are the same domain after normalization", porkbunCredsSource(envJSON), prev, raw)
		}
		normalized[d] = raw

		managed := false
		for _, domain := range domains {
			if domain == d {
				managed = true
				break
			}
		}
		if !managed {
			return nil, false, fmt.Errorf("%s: credentials for %q, which this instance does not manage (configured: %s)",
				porkbunCredsSource(envJSON), raw, strings.Join(domains, ", "))
		}
		byDomain[d] = entry
	}

	creds := make(map[string]porkbunCreds, len(domains))
	var missing []string
	for _, domain := range domains {
		entry, ok := byDomain[domain]
		if !ok {
			missing = append(missing, domain)
			continue
		}
		c := porkbunCreds{
			apiKey:    strings.TrimSpace(entry.APIKey),
			secretKey: strings.TrimSpace(entry.SecretKey),
		}
		if c.apiKey == "" || c.secretKey == "" {
			return nil, false, fmt.Errorf("%s: api_key / secret_key for %s are empty", porkbunCredsSource(envJSON), domain)
		}
		creds[domain] = c
	}
	if len(missing) > 0 {
		return nil, false, fmt.Errorf("%s: no credentials for %s", porkbunCredsSource(envJSON), strings.Join(missing, ", "))
	}
	return creds, true, nil
}

// porkbunCredsSource names where a credentials document came from, for
// error messages: the environment variable or the file path.
func porkbunCredsSource(envJSON string) string {
	if strings.TrimSpace(os.Getenv(porkbunCredsEnv)) != "" {
		return porkbunCredsEnv
	}
	return porkbunCredsFile
}

// pbEnvelope holds the fields of every Porkbun JSON response this client
// uses.  Successes carry status "SUCCESS" plus endpoint-specific fields;
// errors carry status "ERROR" with a machine-readable code, a human
// message, and sometimes a next_action object (ignored here — we branch on
// code only).
type pbEnvelope struct {
	Status     string     `json:"status"`
	Records    []pbRecord `json:"records"`
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	ExistingID string     `json:"existingId"`
}

// pbRecord is one DNS record as returned by the retrieve endpoints.
type pbRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     string `json:"ttl"`
}

// call posts an authenticated JSON request to path and decodes the response
// envelope, authenticating with the given managed domain's own key pair.
// Any non-SUCCESS status becomes an error carrying Porkbun's
// machine-readable code, so callers can branch on behavior rather than
// message text; exact duplicates become the errDuplicateRecord sentinel.
// Errors are for the server log only — the handler never leaks them to
// clients.
func (p *porkbunProvisioner) call(ctx context.Context, domain, path string, body map[string]string) (pbEnvelope, error) {
	// The endpoint paths interpolate only the Host-resolved domain and the
	// validated label, both of which are restricted to [a-z0-9.-] by
	// validation, so no URL escaping is needed.
	c, ok := p.creds[domain]
	if !ok {
		return pbEnvelope{}, fmt.Errorf("porkbun: no credentials loaded for %s", domain)
	}
	// Every create and delete changes the zone the listing cache
	// describes, so the cached listing must go — unconditionally,
	// including when the call below fails.  A create whose response was
	// lost may still have landed, and a listing stale enough to miss a
	// just-written record could pass an availability check and stack a
	// second TXT record (breaking the handle for both parties).  A lost
	// cache entry only costs one extra listing.
	if strings.HasPrefix(path, "/dns/create/") || strings.HasPrefix(path, "/dns/delete/") {
		defer p.invalidateZone(domain)
	}
	body["apikey"] = c.apiKey
	body["secretapikey"] = c.secretKey
	payload, err := json.Marshal(body)
	if err != nil {
		return pbEnvelope{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return pbEnvelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return pbEnvelope{}, err
	}
	defer resp.Body.Close()

	// HTTP 429/5xx is Porkbun (or a proxy in front of it) refusing to
	// serve the request — most importantly the shared per-domain key
	// being over its rate limit, which takes the whole domain's
	// registrations down with it.  Classify it as backend unavailability
	// so the handler can shed load (503) rather than answer 500 as if
	// each individual request had failed.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return pbEnvelope{}, fmt.Errorf("%w: HTTP %d", errBackendUnavailable, resp.StatusCode)
	}

	var env pbEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, porkbunMaxResponse)).Decode(&env); err != nil {
		// A body that is not JSON is what a rate-limited key gets: an
		// HTML error page (observed live).  That is the same shared-key
		// condition as a 429, so it classifies the same way.
		return pbEnvelope{}, fmt.Errorf("decoding response: %v: %w", err, errBackendUnavailable)
	}
	if env.Status != "SUCCESS" {
		if env.Code == porkbunDuplicateCode {
			return env, fmt.Errorf("%w: existing id %s", errDuplicateRecord, env.ExistingID)
		}
		return env, fmt.Errorf("porkbun error %s: %s", env.Code, env.Message)
	}
	return env, nil
}

// zoneCache briefly caches each managed domain's zone listing for the
// registration path.  Every visitor of a domain shares that domain's single
// Porkbun API key, and each uncached registration costs a full zone
// listing; aggregate probing (IP rotation defeats the per-IP limiter) can
// push the shared key over Porkbun's rate limit, which takes down every
// registration for the domain.  The cache bounds that pressure without
// becoming a ledger: writes invalidate (see call), the TTL is short, and
// the reaper never reads through it.
type zoneCache struct {
	// mu guards the zones map only; each entry carries its own lock, so
	// one zone's in-flight fetch never blocks another zone's.
	mu    sync.Mutex
	zones map[string]*zoneEntry
}

// zoneEntry is one domain's cached listing.  records is never mutated
// after being stored here — invalidateZone replaces the pointer — so
// callers may iterate a returned slice without holding the lock.
type zoneEntry struct {
	mu      sync.Mutex
	records []pbRecord
	expires time.Time
}

// entry returns the cache slot for domain, creating it on first use.
func (c *zoneCache) entry(domain string) *zoneEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.zones == nil {
		c.zones = make(map[string]*zoneEntry)
	}
	e := c.zones[domain]
	if e == nil {
		e = &zoneEntry{}
		c.zones[domain] = e
	}
	return e
}

// zoneRecords returns the domain's current zone listing, from the cache
// when it is fresh.  The entry lock is held across the Porkbun fetch,
// which serializes fetches per zone and closes the dangerous ordering: an
// invalidation from a concurrent write can only run BEFORE the fetch
// acquires the lock (the fetch then sees the written zone) or AFTER the
// fetch stores (the invalidation immediately clears what was stored) — a
// listing that predates a write can never survive that write.
func (p *porkbunProvisioner) zoneRecords(ctx context.Context, domain string) ([]pbRecord, error) {
	e := p.cache.entry(domain)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.expires.After(time.Now()) {
		return e.records, nil
	}
	env, err := p.call(ctx, domain, "/dns/retrieve/"+domain, map[string]string{})
	if err != nil {
		return nil, err
	}
	e.records = env.Records
	e.expires = time.Now().Add(zoneListingTTL)
	return e.records, nil
}

// invalidateZone drops the cached listing for domain so the next lookup
// re-fetches fresh truth.  Called on every create and delete against the
// zone (see call): a stale listing that missed this process's own
// just-written record could pass an availability or per-DID cap check and
// wrongly allow a second record.
func (p *porkbunProvisioner) invalidateZone(domain string) {
	e := p.cache.entry(domain)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records, e.expires = nil, time.Time{}
}

// Provision implements the provisioner interface: list the zone, refuse if
// the label is taken or this DID already holds a handle under the domain,
// and create the record if — and only if — both are clear.
//
// A single zone listing serves both checks (whether the label's record
// exists, and how many handles this DID holds), served from the brief
// listing cache (see zoneRecords) so bursts of probes do not each cost
// Porkbun a full listing.  The cache is dropped on every write this
// process makes, so the checks below always judge records this process
// has just created.
//
// For a label "john" it manages the TXT record _atproto.john.<domain>,
// exactly the name Bluesky's handle resolver queries for john.<domain>.
func (p *porkbunProvisioner) Provision(ctx context.Context, domain, label, did string) (provisionOutcome, error) {
	fqdn := "_atproto." + label + "." + domain

	records, err := p.zoneRecords(ctx, domain)
	if err != nil {
		return 0, fmt.Errorf("listing zone records: %w", err)
	}

	var labelRecords []pbRecord
	didHandles := 0
	for _, r := range records {
		if r.Type != "TXT" || !strings.HasPrefix(r.Name, "_atproto.") {
			continue
		}
		if r.Name == fqdn {
			labelRecords = append(labelRecords, r)
		}
		if r.Content == "did="+did {
			didHandles++
		}
	}

	switch len(labelRecords) {
	case 0:
		// No record: the label is free so far.
	case 1:
		if labelRecords[0].Content == "did="+did {
			return outcomeRegistered, nil
		}
		// The label is bound to a different DID.
		return outcomeConflict, nil
	default:
		// Multiple records on one _atproto name: per the atproto spec
		// this handle resolves for nobody.  Refuse rather than stack a
		// third record; the zone needs a human cleanup.
		return outcomeConflict, nil
	}

	// One handle per DID per domain: each DID gets a single label under
	// each community, which multiplies the cost of squatting (a fresh
	// Bluesky account per handle) and keeps the "one handle per person"
	// product shape enforceable.  The count runs after the label lookup,
	// so re-submitting one's own handle stays an idempotent 200.
	if didHandles > 0 {
		return outcomeDIDLimit, nil
	}

	_, err = p.call(ctx, domain, "/dns/create/"+domain, map[string]string{
		"type":    "TXT",
		"name":    "_atproto." + label,
		"content": "did=" + did,
		"ttl":     porkbunRecordTTL,
	})
	if err != nil {
		if errors.Is(err, errDuplicateRecord) {
			// The exact record we were about to write appeared between
			// our retrieve and this create (the per-label lock rules out
			// another request from this process).  DUPLICATE_RECORD
			// means same name/type/content, i.e. this requester's DID —
			// the handle is bound to them either way.
			return outcomeRegistered, nil
		}
		return 0, fmt.Errorf("creating record: %w", err)
	}
	return outcomeCreated, nil
}

// atprotoLabel extracts the handle label from a record name like
// _atproto.john.example.com in the zone example.com.  A name that is not a
// plain "_atproto.<label>.<domain>" (no FQDN dot, single label) is not a
// record niceat manages — e.g. a dashboard-created multi-level name — and is
// reported as such so callers skip it.
func atprotoLabel(name, domain string) (string, bool) {
	name = strings.TrimSuffix(name, ".")
	rest, ok := strings.CutPrefix(name, "_atproto.")
	if !ok {
		return "", false
	}
	label, ok := strings.CutSuffix(rest, "."+domain)
	if !ok || label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// atprotoRecord is one _atproto TXT record as the reaper sees it: where it
// lives, and which account it binds the handle to.
type atprotoRecord struct {
	// ID is Porkbun's record id, unique per zone record.  The reaper keys
	// its grace-period state on this, so a re-registered label (fresh
	// record, fresh id) starts a fresh grace clock automatically.
	ID string
	// Label is the handle's subdomain label (e.g. "john").
	Label string
	// DID is the record's value (the account the handle points at).
	DID string
}

// listAtproto returns every _atproto TXT record in the zone, parsed into
// label + DID pairs with Porkbun record ids attached.  The reaper is the
// only caller: it needs the full population of handle records to judge
// which are stale.  Non-_atproto records, foreign content, and
// non-niceat-shaped names are skipped.
//
// This listing deliberately bypasses the zone cache (it calls Porkbun
// directly): the reaper's judgments and marker pruning must be based on
// complete, current zone state, never a listing that could predate a
// recent write.
func (p *porkbunProvisioner) listAtproto(ctx context.Context, domain string) ([]atprotoRecord, error) {
	env, err := p.call(ctx, domain, "/dns/retrieve/"+domain, map[string]string{})
	if err != nil {
		return nil, fmt.Errorf("listing zone records: %w", err)
	}
	var out []atprotoRecord
	for _, r := range env.Records {
		if r.Type != "TXT" {
			continue
		}
		label, ok := atprotoLabel(r.Name, domain)
		if !ok {
			continue
		}
		did, ok := strings.CutPrefix(r.Content, "did=")
		if !ok {
			// A TXT record on an _atproto name without a did= value
			// is foreign; the reaper only judges its own records.
			continue
		}
		out = append(out, atprotoRecord{ID: r.ID, Label: label, DID: did})
	}
	return out, nil
}

// deleteRecord removes the record with the given Porkbun id from the zone.
// The reaper deletes by id — never by name — so a record that was replaced
// between the reaper's listing and this call is either already gone
// (harmless Porkbun error) or is a different record whose owner it did not
// judge.
func (p *porkbunProvisioner) deleteRecord(ctx context.Context, domain, id string) error {
	if _, err := p.call(ctx, domain, "/dns/delete/"+domain+"/"+id, map[string]string{}); err != nil {
		return fmt.Errorf("deleting record %s: %w", id, err)
	}
	return nil
}

// graceMarker is one reaper grace clock as stored in the zone: a TXT record
// at _niceat-grace.<label>.<domain> whose content encodes the _atproto
// record the clock belongs to and when the first affirmative mismatch was
// observed.  Keeping clocks in the zone (instead of a local file) makes the
// reaper stateless: redeploys, recreated Machines, and lost files can only
// ever be corrected by the zone, which is also the claim store.
type graceMarker struct {
	// ID is the marker record's own Porkbun id, used to delete it.
	ID string
	// RecordID is the _atproto record id the clock belongs to.  Keying
	// the clock on the claim's record id is what guarantees a
	// re-registered label (fresh record, fresh id) starts a fresh clock.
	RecordID string
	// Since is the first-mismatch observation time.
	Since time.Time
	// Corrupt marks a marker whose content could not be parsed.  It
	// matches no record and the reaper prunes it; treating garbage as
	// no clock is the fail-safe direction — a fresh clock starts and
	// reaps are delayed, never hastened.
	Corrupt bool
}

// listGraceMarkers returns every grace-marker TXT record in the zone.
// Unparseable contents are returned as Corrupt markers rather than dropped,
// so the reaper can prune them instead of leaving them to accumulate.
// Like listAtproto, this bypasses the zone cache: the reaper judges on
// fresh, complete zone state.
func (p *porkbunProvisioner) listGraceMarkers(ctx context.Context, domain string) ([]graceMarker, error) {
	env, err := p.call(ctx, domain, "/dns/retrieve/"+domain, map[string]string{})
	if err != nil {
		return nil, fmt.Errorf("listing zone records: %w", err)
	}
	var out []graceMarker
	for _, r := range env.Records {
		if r.Type != "TXT" || !strings.HasPrefix(r.Name, graceMarkerPrefix+".") {
			continue
		}
		m := graceMarker{ID: r.ID, Corrupt: true}
		fields := strings.Fields(r.Content)
		if len(fields) == 4 && fields[0] == graceMarkerTag && fields[1] == graceMarkerVersion {
			if since, err := time.Parse(time.RFC3339, fields[3]); err == nil {
				m.RecordID, m.Since, m.Corrupt = fields[2], since, false
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// createGraceMarker starts a grace clock by writing the marker record into
// the zone.  A DUPLICATE_RECORD response is success: the identical marker
// (same record id and timestamp) is already there, which is exactly as good.
func (p *porkbunProvisioner) createGraceMarker(ctx context.Context, domain, label, recordID string, since time.Time) error {
	content := fmt.Sprintf("%s %s %s %s",
		graceMarkerTag, graceMarkerVersion, recordID, since.UTC().Format(time.RFC3339))
	if _, err := p.call(ctx, domain, "/dns/create/"+domain, map[string]string{
		"type":    "TXT",
		"name":    graceMarkerPrefix + "." + label,
		"content": content,
		"ttl":     porkbunRecordTTL,
	}); err != nil {
		if errors.Is(err, errDuplicateRecord) {
			return nil
		}
		return fmt.Errorf("creating grace marker: %w", err)
	}
	return nil
}
