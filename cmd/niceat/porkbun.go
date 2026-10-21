package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Porkbun API constants.  Base URL, endpoints and credential filenames per
// the v3 docs (https://porkbun.com/api/json/v3/documentation); create
// semantics verified against the sandbox API on 2026-10-15.
const (
	porkbunBaseURL    = "https://api.porkbun.com/api/json/v3"
	porkbunAPIKeyFile = "porkbun_api_key.txt"
	porkbunSecretFile = "porkbun_secret_key.txt"
	porkbunTimeout    = 10 * time.Second
	porkbunRecordTTL  = "300"
	// porkbunDuplicateCode is Porkbun's machine-readable code for an exact
	// duplicate create (same name, type and content).
	porkbunDuplicateCode = "DUPLICATE_RECORD"
	// porkbunMaxResponse caps how much of a Porkbun response we buffer;
	// the responses we use are tiny, and a bounded read cannot be abused.
	porkbunMaxResponse = 1 << 20 // 1 MiB
)

// errDuplicateRecord reports that Porkbun refused a create because the exact
// record already exists.  It is a sentinel (not a wrapped error) so callers
// can branch on it with errors.Is.
var errDuplicateRecord = errors.New("porkbun: duplicate record")

// porkbunProvisioner provisions _atproto TXT records through the Porkbun
// DNS API.  DNS is the claim store — there is deliberately no local ledger:
// the existence and content of the TXT record are the single source of
// truth, so nothing can desync (not the reaper's grace-clock file, which
// only affects when a record is deleted and never who owns it, not manual
// edits
// in the Porkbun dashboard, not zone restores).
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
// Credential scoping: production keys will be scoped per domain (dns scope,
// allowed-domain allowlist), so this client will be expected to select and
// forward a unique key pair per managed domain.  Provision already receives
// the Host-resolved domain, so the change is contained here and in the
// credential loading — the handler and the create/retrieve flows stay as
// they are.
type porkbunProvisioner struct {
	apiKey    string
	secretKey string
	baseURL   string
	hc        *http.Client
}

// newPorkbunProvisioner returns a client for the given credential pair.  An
// empty baseURL selects the production endpoint; tests point it at an
// httptest server.
func newPorkbunProvisioner(apiKey, secretKey, baseURL string) *porkbunProvisioner {
	if baseURL == "" {
		baseURL = porkbunBaseURL
	}
	return &porkbunProvisioner{
		apiKey:    apiKey,
		secretKey: secretKey,
		baseURL:   baseURL,
		hc:        &http.Client{Timeout: porkbunTimeout},
	}
}

// loadPorkbunCredentials reads the API key pair from dir, which is the
// service working directory (the files are gitignored).  ok=false means
// both files are absent and the caller should fall back to the in-memory
// provisioner; one file present without the other is a configuration error,
// not a fallback, and is reported as such.
func loadPorkbunCredentials(dir string) (apiKey, secretKey string, ok bool, err error) {
	apiB, apiErr := os.ReadFile(filepath.Join(dir, porkbunAPIKeyFile))
	secB, secErr := os.ReadFile(filepath.Join(dir, porkbunSecretFile))
	if apiErr != nil && secErr != nil {
		return "", "", false, nil
	}
	if apiErr != nil || secErr != nil {
		return "", "", false, fmt.Errorf("found only one of %s / %s; provide both or neither", porkbunAPIKeyFile, porkbunSecretFile)
	}
	apiKey = strings.TrimSpace(string(apiB))
	secretKey = strings.TrimSpace(string(secB))
	if apiKey == "" || secretKey == "" {
		return "", "", false, fmt.Errorf("%s / %s are empty", porkbunAPIKeyFile, porkbunSecretFile)
	}
	return apiKey, secretKey, true, nil
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
// envelope.  Any non-SUCCESS status becomes an error carrying Porkbun's
// machine-readable code, so callers can branch on behavior rather than
// message text; exact duplicates become the errDuplicateRecord sentinel.
// Errors are for the server log only — the handler never leaks them to
// clients.
func (p *porkbunProvisioner) call(ctx context.Context, path string, body map[string]string) (pbEnvelope, error) {
	// The endpoint paths interpolate only the Host-resolved domain and the
	// validated label, both of which are restricted to [a-z0-9.-] by
	// validation, so no URL escaping is needed.
	body["apikey"] = p.apiKey
	body["secretapikey"] = p.secretKey
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

	env, err := p.call(ctx, "/dns/retrieve/"+domain, map[string]string{})
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

	env, err = p.call(ctx, "/dns/create/"+domain, map[string]string{
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
	env, err := p.call(ctx, "/dns/retrieve/"+domain, map[string]string{})
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
	if _, err := p.call(ctx, "/dns/delete/"+domain+"/"+id, map[string]string{}); err != nil {
		return fmt.Errorf("deleting record %s: %w", id, err)
	}
	return nil
}
