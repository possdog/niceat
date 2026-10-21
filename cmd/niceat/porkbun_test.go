package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
		// Sandbox-verified semantics: an exact duplicate is rejected
		// with DUPLICATE_RECORD; anything else stacks.
		for i, c := range f.records[body["name"]] {
			if c == body["content"] {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":     "ERROR",
					"code":       porkbunDuplicateCode,
					"message":    "A DNS record with this name, type, and content already exists.",
					"existingId": fakeRecordID(body["name"], i),
				})
				return
			}
		}
		f.records[body["name"]] = append(f.records[body["name"]], body["content"])
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS", "id": fakeRecordID(body["name"], len(f.records[body["name"]])-1)})

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
// tears the server down when the test ends.
func newTestPorkbun(t *testing.T, f *fakePorkbunServer) *porkbunProvisioner {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newPorkbunProvisioner("testkey", "testsecret", srv.URL)
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
	if got := f.records["_atproto.john"]; len(got) != 1 || got[0] != "did="+validPLC {
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
	p := newPorkbunProvisioner("testkey", "testsecret", srv.URL)
	srv.Close() // every request now fails at the transport layer

	if outcome, err := p.Provision(context.Background(), testDomain, "john", validPLC); err == nil {
		t.Fatalf("expected a transport error, got outcome %d", outcome)
	}
}

func TestLoadPorkbunCredentials(t *testing.T) {
	write := func(t *testing.T, dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	t.Run("both files present", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, porkbunAPIKeyFile, "pk1_sb_test\n")
		write(t, dir, porkbunSecretFile, "sk1_sb_test\n")
		apiKey, secretKey, ok, err := loadPorkbunCredentials(dir)
		if err != nil || !ok {
			t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
		}
		if apiKey != "pk1_sb_test" || secretKey != "sk1_sb_test" {
			t.Fatalf("credentials not trimmed: %q / %q", apiKey, secretKey)
		}
	})

	t.Run("neither file present", func(t *testing.T) {
		_, _, ok, err := loadPorkbunCredentials(t.TempDir())
		if err != nil {
			t.Fatalf("missing pair must be a fallback, not an error: %v", err)
		}
		if ok {
			t.Fatalf("expected ok=false for a missing pair")
		}
	})

	t.Run("one file present", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, porkbunAPIKeyFile, "pk1_sb_test")
		if _, _, ok, err := loadPorkbunCredentials(dir); err == nil || ok {
			t.Fatalf("expected an error for a half-present pair, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, porkbunAPIKeyFile, "  \n")
		write(t, dir, porkbunSecretFile, "sk1_sb_test")
		if _, _, ok, err := loadPorkbunCredentials(dir); err == nil || ok {
			t.Fatalf("expected an error for empty credentials, got ok=%v err=%v", ok, err)
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
