package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestDirectory runs a plc.directory stand-in (serving DID documents
// from the given map) and returns a resolver pointed at it.
func newTestDirectory(t *testing.T, docs map[string][]string) (*plcDirectory, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		aka, ok := docs[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "DID not registered: " + r.URL.Path})
			return
		}
		did := strings.TrimPrefix(r.URL.Path, "/")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"@context":    []string{"https://www.w3.org/ns/did/v1"},
			"id":          did,
			"alsoKnownAs": aka,
		})
	}))
	t.Cleanup(srv.Close)
	return newPlcDirectory(srv.URL), srv
}

func TestResolvePlcRegistered(t *testing.T) {
	d, _ := newTestDirectory(t, map[string][]string{
		validPLC: {"at://john.example.com", "at://other.example.org"},
	})
	aka, registered, err := d.resolve(context.Background(), validPLC)
	if err != nil || !registered {
		t.Fatalf("resolve: registered=%v err=%v", registered, err)
	}
	if !akaHasHandle(aka, "john.example.com") {
		t.Fatalf("expected aka to contain john.example.com, got %v", aka)
	}
	if akaHasHandle(aka, "nobody.example.com") {
		t.Fatalf("aka must not contain nobody.example.com, got %v", aka)
	}
}

func TestResolvePlcCaseInsensitiveHandle(t *testing.T) {
	// DNS names are case-insensitive; a document that somehow stores an
	// uppercase handle must still count as using the lowercase one.
	d, _ := newTestDirectory(t, map[string][]string{
		validPLC: {"at://John.Example.Com"},
	})
	aka, registered, err := d.resolve(context.Background(), validPLC)
	if err != nil || !registered {
		t.Fatalf("resolve: registered=%v err=%v", registered, err)
	}
	if !akaHasHandle(aka, "john.example.com") {
		t.Fatalf("expected case-insensitive aka match, got %v", aka)
	}
}

func TestResolvePlcUnregistered(t *testing.T) {
	// A directory 404 is affirmative: the account is gone.
	d, _ := newTestDirectory(t, map[string][]string{})
	_, registered, err := d.resolve(context.Background(), validPLC)
	if err != nil {
		t.Fatalf("a 404 must not be an error, got %v", err)
	}
	if registered {
		t.Fatal("expected registered=false for a 404")
	}
}

func TestResolvePlcServerError(t *testing.T) {
	// A 5xx is inconclusive: the reaper must never conclude anything
	// from a directory that is failing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	d := newPlcDirectory(srv.URL)
	if _, _, err := d.resolve(context.Background(), validPLC); err == nil {
		t.Fatal("expected an error from a 5xx directory")
	}
}

func TestResolvePlcIDMismatchIsError(t *testing.T) {
	// Something answering for a different DID is inconclusive — never a
	// basis for reaping.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "did:plc:elsewhere", "alsoKnownAs": []string{}})
	}))
	t.Cleanup(srv.Close)
	d := newPlcDirectory(srv.URL)
	if _, _, err := d.resolve(context.Background(), validPLC); err == nil {
		t.Fatal("expected an error for a document whose id does not match")
	}
}

func TestResolveWebWellKnown(t *testing.T) {
	// did:web resolves through the DID's own domain at
	// /.well-known/did.json — always HTTPS per the spec — so the test
	// stands up a TLS server and makes its host the DID.  The client
	// skips certificate verification for the self-signed test cert
	// only; production resolution never does.
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/did.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "did:web:" + strings.TrimPrefix(srv.URL, "https://"),
			"alsoKnownAs": []string{"at://example.com"},
		})
	}))
	t.Cleanup(srv.Close)

	d := newPlcDirectory("")
	d.hc = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	did := "did:web:" + strings.TrimPrefix(srv.URL, "https://")
	aka, registered, err := d.resolve(context.Background(), did)
	if err != nil || !registered {
		t.Fatalf("resolve: registered=%v err=%v", registered, err)
	}
	if !akaHasHandle(aka, "example.com") {
		t.Fatalf("expected aka to contain example.com, got %v", aka)
	}
}

// newTestWebHost stands up the did:web half of the test rig: a TLS server
// (did:web resolution is always HTTPS per the spec) and a resolver whose
// client skips certificate verification for the self-signed test cert
// only; production resolution never does.  Returns the resolver and the
// server's host[:port] for building DIDs.  The caller supplies the
// handler, which must serve the document at the path the did:web mapping
// prescribes.
func newTestWebHost(t *testing.T, handler http.HandlerFunc) (*plcDirectory, string) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	d := newPlcDirectory("")
	d.hc = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	return d, strings.TrimPrefix(srv.URL, "https://")
}

func TestResolveWebWithPathSegments(t *testing.T) {
	// Per the did:web spec, did:web:host:users:alice resolves to
	// https://host/users/alice/did.json (each colon after the host
	// becomes one path segment; only the root form uses /.well-known).
	d, host := newTestWebHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/alice/did.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "did:web:" + r.Host + ":users:alice",
			"alsoKnownAs": []string{"at://alice.example.com"},
		})
	})
	did := "did:web:" + host + ":users:alice"
	aka, registered, err := d.resolve(context.Background(), did)
	if err != nil || !registered {
		t.Fatalf("resolve: registered=%v err=%v", registered, err)
	}
	if !akaHasHandle(aka, "alice.example.com") {
		t.Fatalf("expected aka to contain alice.example.com, got %v", aka)
	}
}

func TestResolveWebPercentEncodedPort(t *testing.T) {
	// The did:web spec: a port colon in the identifier MUST be
	// percent-encoded ("did:web:example.com%3A3000"), and resolution
	// percent-decodes it back into the URL's host.  The test server
	// listens on the loopback, so the identifier is built around
	// 127.0.0.1 — the resolver's job is only the mapping, not the
	// spec's issuer-side "no IP addresses" rule.
	d, host := newTestWebHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/did.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "did:web:" + strings.Replace(r.Host, ":", "%3A", 1),
			"alsoKnownAs": []string{"at://port.example.com"},
		})
	})
	_, port, _ := strings.Cut(host, ":")
	did := "did:web:127.0.0.1%3A" + port
	aka, registered, err := d.resolve(context.Background(), did)
	if err != nil || !registered {
		t.Fatalf("resolve: registered=%v err=%v", registered, err)
	}
	if !akaHasHandle(aka, "port.example.com") {
		t.Fatalf("expected aka to contain port.example.com, got %v", aka)
	}
}

func TestResolveWebRefusesHostileSegments(t *testing.T) {
	// Segments that would build a URL with path traversal or a path
	// other than the spec mapping are unverifiable (error), never
	// best-effort resolved; an error is the reaper's inconclusive signal.
	d, _ := newTestWebHost(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "did:web:anything"})
	})
	for _, did := range []string{
		"did:web:example.com/users",         // literal slash in the host
		"did:web:example.com:users%2Fx",     // encoded slash inside a segment
		"did:web:example.com:users:.",       // dot segment
		"did:web:example.com:users:..",      // traversal segment
		"did:web:example.com:users:",        // empty segment
		"did:web:example.com%2F..%2F..%2Fx", // encoded traversal in the host
	} {
		if _, _, err := d.resolve(context.Background(), did); err == nil {
			t.Fatalf("expected an error for %s", did)
		}
	}
}

func TestResolveWebPathIDMismatchIsError(t *testing.T) {
	// The id-mismatch check applies to path-segment documents too: a
	// document answering for a different DID is inconclusive — never a
	// basis for reaping.
	d, host := newTestWebHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/alice/did.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "did:web:" + r.Host + ":users:bob",
			"alsoKnownAs": []string{"at://alice.example.com"},
		})
	})
	if _, _, err := d.resolve(context.Background(), "did:web:"+host+":users:alice"); err == nil {
		t.Fatal("expected an error for a path-form document whose id does not match")
	}
}

func TestResolveWebPath404IsUnregistered(t *testing.T) {
	// A 404 from a path-form document location is affirmative: the
	// account is gone, so such a record can now be reaped.
	d, _ := newTestWebHost(t, http.NotFound)
	_, registered, err := d.resolve(context.Background(), "did:web:example.com:users:alice")
	if err != nil {
		t.Fatalf("a 404 must not be an error, got %v", err)
	}
	if registered {
		t.Fatal("expected registered=false for a 404")
	}
}

func TestResolveUnsupportedMethod(t *testing.T) {
	d, _ := newTestDirectory(t, map[string][]string{})
	if _, _, err := d.resolve(context.Background(), "did:key:z6Mkexample"); err == nil {
		t.Fatal("expected an error for an unsupported DID method")
	}
}
