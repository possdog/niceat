package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// loopbackResolverClient is the test stand-in for the production client:
// the same no-redirect policy, but no SSRF dial guard.  Every httptest
// server listens on the loopback the guard exists to refuse, so tests of
// document semantics use this; the guard itself has dedicated tests.
func loopbackResolverClient(tlsCfg *tls.Config) *http.Client {
	return resolverHTTPClient(&http.Transport{TLSClientConfig: tlsCfg})
}

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
	d := newPlcDirectory(srv.URL)
	d.hc = loopbackResolverClient(nil)
	return d, srv
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
	d.hc = loopbackResolverClient(nil)
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
	d.hc = loopbackResolverClient(nil)
	if _, _, err := d.resolve(context.Background(), validPLC); err == nil {
		t.Fatal("expected an error for a document whose id does not match")
	}
}

func TestResolvePlcRateLimitIsTypedSignal(t *testing.T) {
	// LOW-7: a 429 from plc.directory is the directory refusing our fetch
	// VOLUME — evidence about us, not about any DID — so it must surface
	// as a distinct error carrying the Retry-After window, letting the
	// reaper stop directory fetches for the rest of the sweep instead of
	// amplifying attacker-steered volume into the limit.
	for name, tc := range map[string]struct {
		header string
		want   time.Duration
	}{
		"retry-after seconds": {"42", 42 * time.Second},
		"absent":              {"", 0},
		"unparseable":         {"soon", 0},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.header != "" {
				w.Header().Set("Retry-After", tc.header)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(srv.Close)
		d := newPlcDirectory(srv.URL)
		d.hc = loopbackResolverClient(nil)
		_, registered, err := d.resolve(context.Background(), validPLC)
		var rl *rateLimitedError
		if !errors.As(err, &rl) {
			t.Fatalf("%s: a directory 429 must be a *rateLimitedError, got %v", name, err)
		}
		if registered {
			t.Fatalf("%s: a 429 must never be a verdict", name)
		}
		if rl.retryAfter != tc.want {
			t.Fatalf("%s: retryAfter = %s, want %s", name, rl.retryAfter, tc.want)
		}
	}
}

func TestResolveWebRateLimitIsPlainError(t *testing.T) {
	// A did:web host's 429 says the HOST is busy; it says nothing about
	// plc.directory, so it must stay a plain inconclusive error — the
	// reaper must never pause directory judging because a third party's
	// server answered 429.
	d, host := newTestWebHost(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, registered, err := d.resolve(context.Background(), "did:web:"+host)
	if err == nil || registered {
		t.Fatalf("a did:web 429 must stay an inconclusive error, got registered=%v err=%v", registered, err)
	}
	var rl *rateLimitedError
	if errors.As(err, &rl) {
		t.Fatal("a did:web 429 must not look like a plc.directory rate limit")
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
	d.hc = loopbackResolverClient(&tls.Config{InsecureSkipVerify: true})

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
	d.hc = loopbackResolverClient(&tls.Config{InsecureSkipVerify: true})
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

// scriptedDialResolver returns a resolver whose transport fails every
// dial with the given error — the exact shape a real client surfaces for
// DNS and connection failures (OpError, then the client's url.Error) —
// so the classification is exercised on realistic error chains with no
// network involved at all.
func scriptedDialResolver(dialErr error) *plcDirectory {
	d := newPlcDirectory("")
	d.hc = resolverHTTPClient(&http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, dialErr
		},
	})
	return d
}

// nxdomainDialError is the error Go's resolver and dialer really produce
// for a nonexistent host: the authoritative "no such name" answer, with
// IsNotFound set (and no transient flags).
func nxdomainDialError(host string) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
		Err:        "no such host",
		Name:       host,
		IsNotFound: true,
	}}
}

func TestResolveWebNXDomainIsAffirmative(t *testing.T) {
	// MEDIUM-5: for did:web the host IS the identity, so the DNS
	// system's authoritative "no such host" answer is the same class of
	// evidence as a directory 404 — the DID no longer exists, and a
	// record bound to it must not squat its label forever.
	d := scriptedDialResolver(nxdomainDialError("gone.example.com"))
	_, registered, err := d.resolve(context.Background(), "did:web:gone.example.com")
	if err != nil {
		t.Fatalf("an NXDOMAIN must be the affirmative verdict registered=false, got error %v", err)
	}
	if registered {
		t.Fatal("expected registered=false for a host that does not exist")
	}
}

func TestResolveWebTransientDNSErrorsAreInconclusive(t *testing.T) {
	// SERVFAIL and timeouts are transient resolver failures; a refused or
	// failed connection says nothing about whether the host's document
	// exists; and a contradictory not-found+temporary error is judged
	// transient to stay fail-safe.  None of these may ever reap.
	for name, dialErr := range map[string]error{
		"servfail":      &net.DNSError{Err: "server misbehaving", Name: "host.example.com", IsTemporary: true},
		"timeout":       &net.DNSError{Err: "i/o timeout", Name: "host.example.com", IsTimeout: true},
		"refused":       &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		"contradictory": &net.DNSError{Err: "no such host", Name: "host.example.com", IsNotFound: true, IsTemporary: true},
	} {
		d := scriptedDialResolver(dialErr)
		_, registered, err := d.resolve(context.Background(), "did:web:host.example.com")
		if err == nil || registered {
			t.Errorf("%s: a transient failure must stay an inconclusive error, got registered=%v err=%v", name, registered, err)
		}
	}
}

func TestResolvePlcDNSFailureNeverAffirmative(t *testing.T) {
	// The NXDOMAIN classification is scoped to the did:web branch: for
	// did:plc the fetch target is plc.directory, a third party, so a DNS
	// failure reaching it says nothing about the DID and must stay
	// inconclusive — otherwise a plc.directory outage would reap live
	// accounts.
	d := scriptedDialResolver(nxdomainDialError("plc.directory"))
	_, registered, err := d.resolve(context.Background(), validPLC)
	if err == nil || registered {
		t.Fatalf("a directory DNS failure must stay inconclusive, got registered=%v err=%v", registered, err)
	}
}

func TestResolverFetchTimeout(t *testing.T) {
	// The per-fetch timeout is a reaper-sweep-budget parameter (HIGH-3):
	// it is exactly what one hanging DID costs the bounded sweep (one
	// pool worker for the full timeout).  Pin it so a casual edit cannot
	// silently multiply the sweep's worst-case cost.
	if got := newPlcDirectory("").hc.Timeout; got != 5*time.Second {
		t.Fatalf("resolver per-fetch timeout = %s, want 5s", got)
	}
}

func TestGuardDialRefusesNonPublicAddresses(t *testing.T) {
	// The dial-time policy, exercised directly: every range the reaper
	// Machine must never reach, in every spelling (the Control hook runs
	// after DNS resolution, so through the full client these are the
	// addresses that would actually be dialed).
	for _, addr := range []string{
		// IPv4 loopback, unspecified, and 0/8.
		"127.0.0.1:443", "127.255.0.1:80", "0.0.0.0:443", "0.1.2.3:443",
		// RFC1918 private.
		"10.0.0.1:443", "172.16.0.1:443", "172.31.255.255:443", "192.168.0.1:443",
		// Link-local, incl. the cloud metadata endpoint.
		"169.254.169.254:80",
		// IPv6 loopback, unspecified, unique-local, link-local, multicast.
		"[::1]:443", "[::]:443", "[fc00::1]:443", "[fd12::1]:443", "[fe80::1]:443", "[ff02::1]:443",
		// IPv4-mapped and IPv4-compatible spellings of refused ranges.
		"[::ffff:127.0.0.1]:443", "[::ffff:10.0.0.1]:443", "[::127.0.0.1]:443",
	} {
		err := guardDial("tcp", addr, nil)
		var ud *unsafeDialError
		if !errors.As(err, &ud) {
			t.Errorf("guardDial(%q) = %v, want an unsafeDialError", addr, err)
		}
	}
	for _, addr := range []string{
		// Public destinations must dial normally.
		"93.184.216.34:443", "1.1.1.1:443",
		"[2606:2800:220:1:248:1893:25c8:1946]:443", "[::ffff:93.184.216.34]:443",
	} {
		if err := guardDial("tcp", addr, nil); err != nil {
			t.Errorf("guardDial(%q) = %v, want nil", addr, err)
		}
	}
}

// guardedWebResolver returns a resolver whose client is the production
// composition (no redirects + SSRF dial guard) with certificate
// verification skipped only for the self-signed httptest cert; production
// resolution never is.
func guardedWebResolver() *plcDirectory {
	d := newPlcDirectory("")
	d.hc = resolverHTTPClient(ssrfGuardedTransport(&tls.Config{InsecureSkipVerify: true}))
	return d
}

func TestResolveWebRefusesNonPublicTargets(t *testing.T) {
	// Registration accepts any syntactically valid did:web DID, so the
	// host is attacker-chosen: the reaper Machine must never fetch its
	// own loopback, private networks, or a metadata endpoint.  The
	// refusal must be an error (inconclusive: the reaper skips, never
	// reaps), never a verdict, and the target must not be contacted.
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"id": "did:web:whatever"}`))
	}))
	t.Cleanup(srv.Close)

	d := guardedWebResolver()
	host := strings.TrimPrefix(srv.URL, "https://")
	for _, did := range []string{
		"did:web:" + host,         // a live loopback listener: refused before connect
		"did:web:127.0.0.1",       // loopback literal, port 443
		"did:web:10.0.0.1",        // RFC1918 literal
		"did:web:169.254.169.254", // link-local metadata endpoint
		"did:web:localhost",       // a hostname resolving to loopback (rebinding shape)
	} {
		_, registered, err := d.resolve(context.Background(), did)
		if registered {
			t.Fatalf("%s: a refused dial must never be a verdict", did)
		}
		var ud *unsafeDialError
		if !errors.As(err, &ud) {
			t.Fatalf("%s: resolve error = %v, want an unsafeDialError", did, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback test server was contacted %d times; the guard must refuse before connecting", n)
	}
}

func TestResolveWebDoesNotFollowRedirects(t *testing.T) {
	// A did:web host answering 302 must not steer the fetch anywhere —
	// in particular a Location: http://... scheme downgrade to a
	// plain-HTTP endpoint.  The 3xx comes back to fetchDocument as the
	// final response and is classified inconclusive (non-2xx), and the
	// redirect target is never contacted.
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		_, _ = w.Write([]byte(`{"id": "did:web:downgraded"}`))
	}))
	t.Cleanup(plain.Close)

	d, host := newTestWebHost(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", plain.URL+"/downgrade?from="+r.URL.Path)
		w.WriteHeader(http.StatusFound)
	})
	_, registered, err := d.resolve(context.Background(), "did:web:"+host)
	if err == nil {
		t.Fatal("expected an unfollowed 302 to be an inconclusive error")
	}
	if registered {
		t.Fatal("an unfollowed 302 must never count as a verdict")
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the plain-http redirect target was contacted %d times; redirects must not be followed", n)
	}
}
