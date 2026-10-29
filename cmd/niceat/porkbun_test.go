package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fqdn builds the record name Porkbun returns in zone listings:
// _atproto.<label>.<domain>.
func fqdn(domain, label string) string {
	return "_atproto." + label + "." + domain
}

// fakeRecordID mirrors the real API's per-record ids: unique per record.
// The reaper keys its grace state on these, so the fake must not reuse one
// across records.
func fakeRecordID(name string, index int) string {
	return fmt.Sprintf("%s:%d", name, index)
}

// fakePorkbunServer is a minimal Porkbun API stand-in behind an httptest
// server, implementing exactly the endpoints porkbunProvisioner uses
// (zone listing, record create, record delete), with the semantics verified
// against the real sandbox: exact-duplicate creates are rejected,
// everything else stacks.
type fakePorkbunServer struct {
	mu sync.Mutex
	// records maps a record's FQDN name to its TXT contents; multiple
	// contents on one name simulate a stacked (corrupt) zone.
	records map[string][]string
	// failRetrieveCode / failCreateCode make that endpoint return a
	// Porkbun-style ERROR with the given code instead of its usual reply.
	failRetrieveCode string
	failCreateCode   string
	// rawRetrieveStatus / rawRetrieveBody, when status is non-zero, make
	// the zone listing answer with this raw status and body instead of
	// the JSON envelope — Porkbun's observed behavior when the API key
	// is over its rate limit (an HTML error page).
	rawRetrieveStatus int
	rawRetrieveBody   string
	// lastCreate is the decoded body of the most recent create call.
	lastCreate map[string]string

	retrieves int
	creates   int
	deletes   int
}

func newFakePorkbun() *fakePorkbunServer {
	return &fakePorkbunServer{records: make(map[string][]string)}
}

func (f *fakePorkbunServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	f.mu.Lock()
	defer f.mu.Unlock()

	writeError := func(code string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ERROR",
			"code":    code,
			"message": code + " (simulated)",
		})
	}

	switch {
	case strings.HasPrefix(r.URL.Path, "/dns/retrieve/"):
		f.retrieves++
		if f.rawRetrieveStatus != 0 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(f.rawRetrieveStatus)
			_, _ = w.Write([]byte(f.rawRetrieveBody))
			return
		}
		if f.failRetrieveCode != "" {
			writeError(f.failRetrieveCode)
			return
		}
		// A zone listing: every record whose name is inside this
		// domain, with names as FQDNs like the real API returns them.
		domain := strings.TrimPrefix(r.URL.Path, "/dns/retrieve/")
		resp := map[string]any{"status": "SUCCESS"}
		var records []map[string]string
		for name, contents := range f.records {
			if !strings.HasSuffix(name, "."+domain) {
				continue
			}
			for i, c := range contents {
				records = append(records, map[string]string{
					"id":      fakeRecordID(name, i),
					"name":    name,
					"type":    "TXT",
					"content": c,
					"ttl":     porkbunRecordTTL,
				})
			}
		}
		resp["records"] = records
		_ = json.NewEncoder(w).Encode(resp)

	case strings.HasPrefix(r.URL.Path, "/dns/create/"):
		f.creates++
		if f.failCreateCode != "" {
			writeError(f.failCreateCode)
			return
		}
		f.lastCreate = body
		// Like the real API, a record created with a relative name
		// (e.g. "_atproto.john") is listed back under its FQDN.
		domain := strings.TrimPrefix(r.URL.Path, "/dns/create/")
		name := body["name"]
		if !strings.HasSuffix(name, "."+domain) {
			name = name + "." + domain
		}
		// Sandbox-verified semantics: an exact duplicate is rejected
		// with DUPLICATE_RECORD; anything else stacks.
		for i, c := range f.records[name] {
			if c == body["content"] {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":     "ERROR",
					"code":       porkbunDuplicateCode,
					"message":    "A DNS record with this name, type, and content already exists.",
					"existingId": fakeRecordID(name, i),
				})
				return
			}
		}
		f.records[name] = append(f.records[name], body["content"])
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS", "id": fakeRecordID(name, len(f.records[name])-1)})

	case strings.HasPrefix(r.URL.Path, "/dns/delete/"):
		f.deletes++
		rest := strings.TrimPrefix(r.URL.Path, "/dns/delete/")
		domain, id, ok := strings.Cut(rest, "/")
		if !ok {
			http.Error(w, "bad delete path", http.StatusBadRequest)
			return
		}
		for name, contents := range f.records {
			if !strings.HasSuffix(name, "."+domain) {
				continue
			}
			for i := range contents {
				if fakeRecordID(name, i) == id {
					f.records[name] = append(contents[:i:i], contents[i+1:]...)
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS"})
					return
				}
			}
		}
		// Deleting an id that is not in the zone: the real API errors
		// rather than deleting something else.
		writeError("INVALID_RECORD_ID")

	default:
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}
}

// newTestPorkbun wires a porkbunProvisioner to a fake Porkbun server and
// tears the server down when the test ends.  The client carries the standard
// test credential pair for testDomain.
func newTestPorkbun(t *testing.T, f *fakePorkbunServer) *porkbunProvisioner {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newPorkbunProvisioner(map[string]porkbunCreds{
		testDomain: {apiKey: "testkey", secretKey: "testsecret"},
	}, srv.URL)
}

func TestPorkbunProvisionCreates(t *testing.T) {
	f := newFakePorkbun()
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if outcome != outcomeCreated {
		t.Fatalf("expected outcomeCreated, got %d", outcome)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retrieves != 1 || f.creates != 1 {
		t.Fatalf("expected 1 zone listing + 1 create, got %d listings, %d creates", f.retrieves, f.creates)
	}
	// The create payload must publish the exact record Bluesky resolves.
	want := map[string]string{
		"apikey":       "testkey",
		"secretapikey": "testsecret",
		"type":         "TXT",
		"name":         "_atproto.john",
		"content":      "did=" + validPLC,
		"ttl":          porkbunRecordTTL,
	}
	for k, v := range want {
		if f.lastCreate[k] != v {
			t.Fatalf("create body %q = %q, want %q", k, f.lastCreate[k], v)
		}
	}
	if got := f.records[fqdn(testDomain, "john")]; len(got) != 1 || got[0] != "did="+validPLC {
		t.Fatalf("record not stored: %v", f.records)
	}
}

func TestPorkbunProvisionSameDIDIsIdempotent(t *testing.T) {
	f := newFakePorkbun()
	f.records[fqdn(testDomain, "john")] = []string{"did=" + validPLC}
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if outcome != outcomeRegistered {
		t.Fatalf("expected outcomeRegistered, got %d", outcome)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatalf("no create expected for an already-bound handle, got %d", f.creates)
	}
}

func TestPorkbunProvisionDifferentDIDConflicts(t *testing.T) {
	f := newFakePorkbun()
	f.records[fqdn(testDomain, "john")] = []string{"did=" + validPLC}
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", "did:plc:"+strings.Repeat("y", 24))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if outcome != outcomeConflict {
		t.Fatalf("expected outcomeConflict, got %d", outcome)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatalf("no create expected for a taken handle, got %d", f.creates)
	}
}

func TestPorkbunProvisionMultipleRecordsConflict(t *testing.T) {
	// A zone with two _atproto records on one name is corrupt per the
	// atproto spec; the client must refuse to stack a third.
	f := newFakePorkbun()
	f.records[fqdn(testDomain, "john")] = []string{"did=" + validPLC, "did=did:plc:" + strings.Repeat("y", 24)}
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if outcome != outcomeConflict {
		t.Fatalf("expected outcomeConflict for a multi-record name, got %d", outcome)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatalf("no create expected for a multi-record name, got %d", f.creates)
	}
}

func TestPorkbunProvisionDIDLimit(t *testing.T) {
	// One handle per DID per domain: this DID already holds "mary" under
	// the domain, so a second label must be refused even though "john" is
	// free.
	f := newFakePorkbun()
	f.records[fqdn(testDomain, "mary")] = []string{"did=" + validPLC}
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if outcome != outcomeDIDLimit {
		t.Fatalf("expected outcomeDIDLimit, got %d", outcome)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatalf("no create expected when the DID already holds a handle, got %d", f.creates)
	}
}

func TestPorkbunProvisionDIDLimitIsPerDomain(t *testing.T) {
	// The cap is per domain: the same DID may hold handles under several
	// communities, so a record under a different domain must not count.
	f := newFakePorkbun()
	f.records[fqdn("other.example", "mary")] = []string{"did=" + validPLC}
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if outcome != outcomeCreated {
		t.Fatalf("expected outcomeCreated across domains, got %d", outcome)
	}
}

func TestPorkbunProvisionDuplicateOnCreateIsRegistered(t *testing.T) {
	// If the exact record appears between the zone listing and the create
	// (the per-label lock rules out another request from this process,
	// but a manual Porkbun edit could race), DUPLICATE_RECORD means the
	// requester's DID is bound — report registered, not an error.
	f := newFakePorkbun()
	f.failCreateCode = porkbunDuplicateCode
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if outcome != outcomeRegistered {
		t.Fatalf("expected outcomeRegistered on duplicate create, got %d", outcome)
	}
}

func TestPorkbunProvisionAPIError(t *testing.T) {
	f := newFakePorkbun()
	f.failRetrieveCode = "INVALID_DOMAIN"
	p := newTestPorkbun(t, f)

	outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
	if err == nil {
		t.Fatalf("expected an error from a failing zone listing, got outcome %d", outcome)
	}
	// The machine-readable code must be in the error for the server log.
	if !strings.Contains(err.Error(), "INVALID_DOMAIN") {
		t.Fatalf("error should carry Porkbun's error code, got: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatalf("no create expected after a failed listing, got %d", f.creates)
	}
}

func TestPorkbunProvisionTransportError(t *testing.T) {
	f := newFakePorkbun()
	srv := httptest.NewServer(f)
	p := newPorkbunProvisioner(map[string]porkbunCreds{
		testDomain: {apiKey: "testkey", secretKey: "testsecret"},
	}, srv.URL)
	srv.Close() // every request now fails at the transport layer

	if outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC); err == nil {
		t.Fatalf("expected a transport error, got outcome %d", outcome)
	}
}

func TestPorkbunBackendUnavailable(t *testing.T) {
	// Porkbun signals an exhausted or unavailable API either with a
	// non-JSON HTML error page (observed live when the shared per-domain
	// key is over its rate limit) or with an HTTP 429/5xx status.  Every
	// shape must classify as errBackendUnavailable so the handler can
	// shed load with 503 instead of masking a whole-domain outage as a
	// per-request 500.
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"HTML error page with 200", http.StatusOK, "<html><body>Too many requests</body></html>"},
		{"HTTP 429 with HTML body", http.StatusTooManyRequests, "<html>slow down</html>"},
		{"HTTP 429 with JSON body", http.StatusTooManyRequests, `{"status":"ERROR","code":"RATE_LIMITED"}`},
		{"HTTP 500", http.StatusInternalServerError, "internal error"},
		{"HTTP 502 with JSON envelope", http.StatusBadGateway, `{"status":"ERROR","code":"UPSTREAM"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePorkbun()
			f.rawRetrieveStatus = tt.status
			f.rawRetrieveBody = tt.body
			p := newTestPorkbun(t, f)

			outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC)
			if err == nil {
				t.Fatalf("expected an error, got outcome %d", outcome)
			}
			if !errors.Is(err, errBackendUnavailable) {
				t.Fatalf("expected errBackendUnavailable, got: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.creates != 0 {
				t.Fatalf("no create expected when the backend is unavailable, got %d", f.creates)
			}
		})
	}
}

func TestPorkbunListingCacheServesRepeatLookups(t *testing.T) {
	// Within the TTL, repeat availability and per-DID cap probes must be
	// served from the cached listing instead of each costing Porkbun a
	// full zone listing — the shared per-domain key is the resource an
	// aggregate burst exhausts.
	f := newFakePorkbun()
	f.records[fqdn(testDomain, "taken")] = []string{"did=" + otherDID()}
	f.records[fqdn(testDomain, "mary")] = []string{"did=" + validPLC}
	p := newTestPorkbun(t, f)

	// One availability probe (label taken by another DID) and two
	// per-DID cap probes (this DID already holds "mary"): none writes,
	// so none invalidates.
	if outcome, err := p.Provision(context.Background(), testDomain, "taken", validPLC); err != nil || outcome != outcomeConflict {
		t.Fatalf("taken-label probe: outcome %d, err %v", outcome, err)
	}
	if outcome, err := p.Provision(context.Background(), testDomain, "aaa", validPLC); err != nil || outcome != outcomeDIDLimit {
		t.Fatalf("cap probe aaa: outcome %d, err %v", outcome, err)
	}
	if outcome, err := p.Provision(context.Background(), testDomain, "bbb", validPLC); err != nil || outcome != outcomeDIDLimit {
		t.Fatalf("cap probe bbb: outcome %d, err %v", outcome, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retrieves != 1 || f.creates != 0 {
		t.Fatalf("expected the three probes to share 1 listing with 0 creates, got %d listings, %d creates", f.retrieves, f.creates)
	}
}

func TestPorkbunListingCacheInvalidatedByOwnWrites(t *testing.T) {
	// A write this process makes must be visible to the very next
	// lookup: a listing cached before the create must never judge a
	// request that arrives after it, or a same-label different-DID
	// create would stack a second record and a second label for the
	// same DID would slip past the cap.
	f := newFakePorkbun()
	p := newTestPorkbun(t, f)
	ctx := context.Background()

	if outcome, err := p.Provision(ctx, testDomain, "john", validPLC); err != nil || outcome != outcomeCreated {
		t.Fatalf("fresh registration: outcome %d, err %v", outcome, err)
	}
	// The create dropped the cached listing, so this probe re-lists the
	// zone and sees the fresh record.
	if outcome, err := p.Provision(ctx, testDomain, "john", otherDID()); err != nil || outcome != outcomeConflict {
		t.Fatalf("same label, different DID must conflict: outcome %d, err %v", outcome, err)
	}
	// And the DID that just registered holds its one-handle cap: a
	// second label must be judged against the post-create zone.
	if outcome, err := p.Provision(ctx, testDomain, "mary", validPLC); err != nil || outcome != outcomeDIDLimit {
		t.Fatalf("second label, same DID must hit the cap: outcome %d, err %v", outcome, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 {
		t.Fatalf("only the first registration may create, got %d creates", f.creates)
	}
	if got := f.records[fqdn(testDomain, "john")]; len(got) != 1 || got[0] != "did="+validPLC {
		t.Fatalf("the label's record must not be stacked, contents: %v", got)
	}
}

func TestPorkbunReaperBypassesListingCache(t *testing.T) {
	// The reaper judges records and prunes markers on complete, current
	// zone state: its listings must always fetch fresh truth, even when
	// a fresh cached listing is available.
	f := newFakePorkbun()
	p := newTestPorkbun(t, f)
	ctx := context.Background()

	if _, err := p.Provision(ctx, testDomain, "john", validPLC); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Prime the cache with a post-create listing of the zone (the
	// idempotent repeat reads but does not write).
	if outcome, err := p.Provision(ctx, testDomain, "john", validPLC); err != nil || outcome != outcomeRegistered {
		t.Fatalf("idempotent repeat: outcome %d, err %v", outcome, err)
	}
	f.mu.Lock()
	retrievesBefore := f.retrieves
	f.mu.Unlock()

	records, err := p.listAtproto(ctx, testDomain)
	if err != nil {
		t.Fatalf("listAtproto: %v", err)
	}
	if len(records) != 1 || records[0].Label != "john" {
		t.Fatalf("reaper must see the created record, got %v", records)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retrieves != retrievesBefore+1 {
		t.Fatalf("reaper listing must fetch fresh (bypass the cache): %d -> %d", retrievesBefore, f.retrieves)
	}
}

func TestPorkbunProvisionPerDomainCredentials(t *testing.T) {
	// Each managed domain authenticates with its own scoped key pair:
	// a create under a community must carry that community's keys, never
	// another's (the fake captures the full request body, including the
	// forwarded credentials).
	f := newFakePorkbun()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	other := "other.example"
	p := newPorkbunProvisioner(map[string]porkbunCreds{
		testDomain: {apiKey: "key-a", secretKey: "secret-a"},
		other:      {apiKey: "key-b", secretKey: "secret-b"},
	}, srv.URL)

	if _, err := p.Provision(context.Background(), testDomain, "john", validPLC); err != nil {
		t.Fatalf("Provision under %s: %v", testDomain, err)
	}
	f.mu.Lock()
	if f.lastCreate["apikey"] != "key-a" || f.lastCreate["secretapikey"] != "secret-a" {
		t.Fatalf("create under %s carried wrong credentials: %v", testDomain, f.lastCreate)
	}
	f.mu.Unlock()

	if _, err := p.Provision(context.Background(), other, "mary", validPLC); err != nil {
		t.Fatalf("Provision under %s: %v", other, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastCreate["apikey"] != "key-b" || f.lastCreate["secretapikey"] != "secret-b" {
		t.Fatalf("create under %s carried wrong credentials: %v", other, f.lastCreate)
	}
}

func TestPorkbunNoCredentialsForDomain(t *testing.T) {
	// A domain with no loaded pair must fail loudly rather than send some
	// other community's keys at its zone.  (Strict startup loading makes
	// this unreachable in production; the guard is defense in depth.)
	f := newFakePorkbun()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := newPorkbunProvisioner(map[string]porkbunCreds{
		testDomain: {apiKey: "testkey", secretKey: "testsecret"},
	}, srv.URL)

	if _, err := p.Provision(context.Background(), "other.example", "john", validPLC); err == nil {
		t.Fatal("expected an error provisioning under a domain with no credentials")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatalf("no create expected without credentials, got %d", f.creates)
	}
}

func TestLoadPorkbunCredentials(t *testing.T) {
	// write places a credentials document in dir's porkbun_keys.json.
	write := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, porkbunCredsFile), []byte(content), 0600); err != nil {
			t.Fatalf("writing %s: %v", porkbunCredsFile, err)
		}
	}
	// envDoc sets PORKBUN_CREDENTIALS to a credentials document.
	envDoc := func(t *testing.T, content string) {
		t.Helper()
		t.Setenv(porkbunCredsEnv, content)
	}
	domains := []string{"wagg.ing", "vore.rip"}

	t.Run("all domains have pairs", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, `{
			"wagg.ing":   {"api_key": "pk1_a\n", "secret_key": "sk1_a\n"},
			"vore.rip": {"api_key": "pk1_b", "secret_key": "sk1_b"}
		}`)
		creds, ok, err := loadPorkbunCredentials(dir, domains)
		if err != nil || !ok {
			t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
		}
		want := map[string]porkbunCreds{
			"wagg.ing": {apiKey: "pk1_a", secretKey: "sk1_a"},
			"vore.rip": {apiKey: "pk1_b", secretKey: "sk1_b"},
		}
		for _, d := range domains {
			if creds[d] != want[d] {
				t.Fatalf("creds for %s = %+v, want %+v", d, creds[d], want[d])
			}
		}
	})

	t.Run("no pair for any domain", func(t *testing.T) {
		_, ok, err := loadPorkbunCredentials(t.TempDir(), domains)
		if err != nil {
			t.Fatalf("missing pairs must be a fallback, not an error: %v", err)
		}
		if ok {
			t.Fatal("expected ok=false when no domain has a pair")
		}
	})

	t.Run("some domains missing", func(t *testing.T) {
		// Pairs for some communities but not others is a partial
		// deployment: the unprovisionable domains would silently eat
		// registrations, so it must fail loudly.
		dir := t.TempDir()
		write(t, dir, `{"wagg.ing": {"api_key": "pk1_a", "secret_key": "sk1_a"}}`)
		_, ok, err := loadPorkbunCredentials(dir, domains)
		if err == nil || ok {
			t.Fatalf("expected an error naming the missing domain, got ok=%v err=%v", ok, err)
		}
		if !strings.Contains(err.Error(), "vore.rip") {
			t.Fatalf("error should name the missing domain, got: %v", err)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, `{"wagg.ing": {"api_key": "  \n", "secret_key": "sk1_a"}}`)
		_, ok, err := loadPorkbunCredentials(dir, []string{"wagg.ing"})
		if err == nil || ok {
			t.Fatalf("expected an error for empty credentials, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("unconfigured domain entry refused", func(t *testing.T) {
		// One document holds every domain, so an entry no configured
		// domain claims is almost certainly a typo for a managed one;
		// silently ignoring it would strand the intended domain.
		dir := t.TempDir()
		write(t, dir, `{
			"wagg.ing":          {"api_key": "pk1_a", "secret_key": "sk1_a"},
			"vore.rip":        {"api_key": "pk1_b", "secret_key": "sk1_b"},
			"unmanaged.example": {"api_key": "pk1_x", "secret_key": "sk1_x"}
		}`)
		_, ok, err := loadPorkbunCredentials(dir, domains)
		if err == nil || ok {
			t.Fatalf("expected an error for the unmanaged entry, got ok=%v err=%v", ok, err)
		}
		if !strings.Contains(err.Error(), "unmanaged.example") {
			t.Fatalf("error should name the unmanaged domain, got: %v", err)
		}
	})

	t.Run("invalid document", func(t *testing.T) {
		// Malformed JSON and unknown fields (typos) both fail loudly,
		// mirroring the strict parse of niceat.json.
		for name, doc := range map[string]string{
			"not JSON":      "{",
			"unknown field": `{"wagg.ing": {"api_key": "pk1_a", "secret": "sk1_a"}}`,
		} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				write(t, dir, doc)
				_, ok, err := loadPorkbunCredentials(dir, []string{"wagg.ing"})
				if err == nil || ok {
					t.Fatalf("expected an error for %s, got ok=%v err=%v", name, ok, err)
				}
			})
		}
	})

	t.Run("keys are normalized like configured domains", func(t *testing.T) {
		// "WAGG.ING" and "vore.rip." denote the same domains as the
		// normalized configured entries.
		dir := t.TempDir()
		write(t, dir, `{
			"WAGG.ING":    {"api_key": "pk1_a", "secret_key": "sk1_a"},
			"vore.rip.": {"api_key": "pk1_b", "secret_key": "sk1_b"}
		}`)
		creds, ok, err := loadPorkbunCredentials(dir, domains)
		if err != nil || !ok {
			t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
		}
		if want := (porkbunCreds{apiKey: "pk1_a", secretKey: "sk1_a"}); creds["wagg.ing"] != want {
			t.Fatalf("creds for wagg.ing = %+v, want %+v", creds["wagg.ing"], want)
		}
		if want := (porkbunCreds{apiKey: "pk1_b", secretKey: "sk1_b"}); creds["vore.rip"] != want {
			t.Fatalf("creds for vore.rip = %+v, want %+v", creds["vore.rip"], want)
		}
	})

	t.Run("duplicate after normalization", func(t *testing.T) {
		// JSON keys are case-sensitive, so two spellings of one domain
		// can coexist; they must not silently pick one.
		dir := t.TempDir()
		write(t, dir, `{
			"wagg.ing":  {"api_key": "pk1_a", "secret_key": "sk1_a"},
			"WAGG.ING":  {"api_key": "pk1_z", "secret_key": "sk1_z"}
		}`)
		_, ok, err := loadPorkbunCredentials(dir, []string{"wagg.ing"})
		if err == nil || ok {
			t.Fatalf("expected an error for duplicate spellings, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("env credentials", func(t *testing.T) {
		// Fly.io's secrets model is environment variables: one document
		// covering every domain must be accepted with no files present.
		envDoc(t, `{
			"wagg.ing":   {"api_key": "  pk1_env\n", "secret_key": "sk1_env"},
			"vore.rip": {"api_key": "pk1_env2", "secret_key": "sk1_env2"}
		}`)
		creds, ok, err := loadPorkbunCredentials(t.TempDir(), domains)
		if err != nil || !ok {
			t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
		}
		// Env values are trimmed like file contents.
		want := map[string]porkbunCreds{
			"wagg.ing": {apiKey: "pk1_env", secretKey: "sk1_env"},
			"vore.rip": {apiKey: "pk1_env2", secretKey: "sk1_env2"},
		}
		for _, d := range domains {
			if creds[d] != want[d] {
				t.Fatalf("creds for %s = %+v, want %+v", d, creds[d], want[d])
			}
		}
	})

	t.Run("env wins over files", func(t *testing.T) {
		// A stale file from local development must not shadow what the
		// operator set with fly secrets set.
		dir := t.TempDir()
		write(t, dir, `{"wagg.ing": {"api_key": "pk1_file", "secret_key": "sk1_file"}}`)
		envDoc(t, `{"wagg.ing": {"api_key": "pk1_env", "secret_key": "sk1_env"}}`)
		creds, ok, err := loadPorkbunCredentials(dir, []string{"wagg.ing"})
		if err != nil || !ok {
			t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
		}
		if want := (porkbunCreds{apiKey: "pk1_env", secretKey: "sk1_env"}); creds["wagg.ing"] != want {
			t.Fatalf("creds = %+v, want the env pair %+v", creds["wagg.ing"], want)
		}
	})

	t.Run("empty env falls back to files", func(t *testing.T) {
		// An env value that is empty or whitespace-only counts as
		// absent, so the file document is used.
		dir := t.TempDir()
		write(t, dir, `{"wagg.ing": {"api_key": "pk1_file", "secret_key": "sk1_file"}}`)
		t.Setenv(porkbunCredsEnv, "  \n")
		creds, ok, err := loadPorkbunCredentials(dir, []string{"wagg.ing"})
		if err != nil || !ok {
			t.Fatalf("expected ok from the file document, got ok=%v err=%v", ok, err)
		}
		if want := (porkbunCreds{apiKey: "pk1_file", secretKey: "sk1_file"}); creds["wagg.ing"] != want {
			t.Fatalf("creds = %+v, want the file pair %+v", creds["wagg.ing"], want)
		}
	})
}

func TestPorkbunListAtproto(t *testing.T) {
	// A realistic zone: two handle records, a stacked (corrupt) name, a
	// non-handle TXT record on an _atproto name, a nested multi-level
	// name, and a foreign record type — only the niceat-shaped handle
	// records may be returned.
	f := newFakePorkbun()
	other := "did:plc:" + strings.Repeat("y", 24)
	f.records[fqdn(testDomain, "john")] = []string{"did=" + validPLC}
	f.records[fqdn(testDomain, "mary")] = []string{"did=" + other}
	f.records[fqdn(testDomain, "stacked")] = []string{"did=" + validPLC, "did=" + other}
	f.records[fqdn(testDomain, "spf")] = []string{"v=spf1 -all"}
	f.records["_atproto.a.b."+testDomain] = []string{"did=" + validPLC}
	p := newTestPorkbun(t, f)

	records, err := p.listAtproto(context.Background(), testDomain)
	if err != nil {
		t.Fatalf("listAtproto: %v", err)
	}
	got := make(map[string]string) // id -> label
	for _, r := range records {
		got[r.ID] = r.Label
	}
	// john, mary, and both halves of the stacked name: 4 records.
	want := map[string]string{
		fakeRecordID(fqdn(testDomain, "john"), 0):    "john",
		fakeRecordID(fqdn(testDomain, "mary"), 0):    "mary",
		fakeRecordID(fqdn(testDomain, "stacked"), 0): "stacked",
		fakeRecordID(fqdn(testDomain, "stacked"), 1): "stacked",
	}
	if len(got) != len(want) {
		t.Fatalf("listAtproto returned %d records (%v), want %d", len(got), got, len(want))
	}
	for id, label := range want {
		if got[id] != label {
			t.Errorf("record %s = label %q, want %q", id, got[id], label)
		}
	}
}

func TestPorkbunDeleteRecord(t *testing.T) {
	f := newFakePorkbun()
	other := "did:plc:" + strings.Repeat("y", 24)
	// A stacked name: the reaper deletes one half by id and must leave
	// the other half untouched.
	f.records[fqdn(testDomain, "john")] = []string{"did=" + validPLC, "did=" + other}
	p := newTestPorkbun(t, f)

	if err := p.deleteRecord(context.Background(), testDomain, fakeRecordID(fqdn(testDomain, "john"), 1)); err != nil {
		t.Fatalf("deleteRecord: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	remaining := f.records[fqdn(testDomain, "john")]
	if len(remaining) != 1 || remaining[0] != "did="+validPLC {
		t.Fatalf("wrong record deleted; remaining: %v", remaining)
	}

	// Deleting an id that is not in the zone must be an error, not a
	// silent success.
	if err := p.deleteRecord(context.Background(), testDomain, "no-such-id"); err == nil {
		t.Fatal("expected an error deleting a nonexistent record id")
	}
}

func TestPorkbunGraceMarkers(t *testing.T) {
	// The marker round trip: a created marker lists back with the record
	// id and timestamp it was written with, markers never appear in
	// listAtproto, and a duplicate create (same content) is idempotent.
	f := newFakePorkbun()
	p := newTestPorkbun(t, f)
	ctx := context.Background()
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if err := p.createGraceMarker(ctx, testDomain, "john", "rec-1", since); err != nil {
		t.Fatalf("createGraceMarker: %v", err)
	}
	// An identical write is a no-op, not an error.
	if err := p.createGraceMarker(ctx, testDomain, "john", "rec-1", since); err != nil {
		t.Fatalf("duplicate createGraceMarker: %v", err)
	}

	markers, err := p.listGraceMarkers(ctx, testDomain)
	if err != nil {
		t.Fatalf("listGraceMarkers: %v", err)
	}
	if len(markers) != 1 {
		t.Fatalf("expected exactly one marker, got %v", markers)
	}
	m := markers[0]
	if m.Corrupt || m.RecordID != "rec-1" || !m.Since.Equal(since) {
		t.Fatalf("marker round trip mismatch: %+v", m)
	}
	if m.ID == "" {
		t.Fatal("marker must carry its own record id for deletion")
	}

	// Markers live in their own namespace: listAtproto must not see one.
	records, err := p.listAtproto(ctx, testDomain)
	if err != nil {
		t.Fatalf("listAtproto: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("grace markers must not be listed as claims, got %v", records)
	}

	// And a marker whose content is foreign (or a future version) is
	// corrupt, not silently misread as a clock.
	f.records[graceMarkerPrefix+".mary."+testDomain] = []string{"niceat-grace v999 rec-2 2026-01-01T00:00:00Z"}
	markers, err = p.listGraceMarkers(ctx, testDomain)
	if err != nil {
		t.Fatalf("listGraceMarkers: %v", err)
	}
	var corrupt *graceMarker
	for i := range markers {
		if markers[i].ID == fakeRecordID(graceMarkerPrefix+".mary."+testDomain, 0) {
			corrupt = &markers[i]
		}
	}
	if corrupt == nil || !corrupt.Corrupt || corrupt.RecordID != "" {
		t.Fatalf("future-version marker must be corrupt with no record id, got %+v", corrupt)
	}
}
