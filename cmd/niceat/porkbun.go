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
type porkbunProvisioner struct {
	creds   map[string]porkbunCreds
	baseURL string
	hc      *http.Client
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

	var env pbEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, porkbunMaxResponse)).Decode(&env); err != nil {
		return pbEnvelope{}, fmt.Errorf("decoding response: %w", err)
	}
	if env.Status != "SUCCESS" {
		if env.Code == porkbunDuplicateCode {
			return env, fmt.Errorf("%w: existing id %s", errDuplicateRecord, env.ExistingID)
		}
		return env, fmt.Errorf("porkbun error %s: %s", env.Code, env.Message)
	}
	return env, nil
}

// Provision implements the provisioner interface: list the zone, refuse if
// the label is taken or this DID already holds a handle under the domain,
// and create the record if — and only if — both are clear.
//
// A single zone listing serves both checks (whether the label's record
// exists, and how many handles this DID holds).  Zones are small — Porkbun
// caps them at 2,500 records — and registration is rate-limited, so one
// full list per registration is proportionate.
//
// For a label "john" it manages the TXT record _atproto.john.<domain>,
// exactly the name Bluesky's handle resolver queries for john.<domain>.
func (p *porkbunProvisioner) Provision(ctx context.Context, domain, label, did string) (provisionOutcome, error) {
	fqdn := "_atproto." + label + "." + domain

	env, err := p.call(ctx, domain, "/dns/retrieve/"+domain, map[string]string{})
	if err != nil {
		return 0, fmt.Errorf("listing zone records: %w", err)
	}

	var labelRecords []pbRecord
	didHandles := 0
	for _, r := range env.Records {
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

	env, err = p.call(ctx, domain, "/dns/create/"+domain, map[string]string{
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
