package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestRateLimiterAllow(t *testing.T) {
	rl := newRateLimiter()

	// A fresh IP gets the full burst.
	for i := 0; i < registerBurst; i++ {
		if !rl.allow("192.0.2.1") {
			t.Fatalf("request %d within burst was rejected", i+1)
		}
	}
	// The bucket is now empty; the next request must be refused.
	if rl.allow("192.0.2.1") {
		t.Fatalf("request beyond burst was allowed")
	}
	// Other IPs have independent buckets.
	if !rl.allow("198.51.100.7") {
		t.Fatalf("different IP was throttled by another IP's usage")
	}
}

func TestRateLimiterSweep(t *testing.T) {
	rl := newRateLimiter()
	if !rl.allow("192.0.2.1") {
		t.Fatalf("first request rejected")
	}
	if !rl.allow("198.51.100.7") {
		t.Fatalf("second IP's first request rejected")
	}

	// Age the first IP past the idle TTL while leaving the second fresh.
	rl.mu.Lock()
	rl.ips["192.0.2.1"].lastSeen = time.Now().Add(-ipIdleTTL - time.Second)
	rl.mu.Unlock()

	rl.mu.Lock()
	rl.sweepLocked()
	rl.mu.Unlock()

	rl.mu.Lock()
	_, haveIdle := rl.ips["192.0.2.1"]
	_, haveFresh := rl.ips["198.51.100.7"]
	rl.mu.Unlock()
	if haveIdle {
		t.Fatalf("idle IP was not evicted by sweep")
	}
	if !haveFresh {
		t.Fatalf("recently seen IP was evicted by sweep")
	}
}

func TestRateLimiterCanonicalIPKeys(t *testing.T) {
	rl := newRateLimiter()

	// First sight via the IPv4-mapped IPv6 spelling.
	if !rl.allow("::ffff:192.0.2.1") {
		t.Fatalf("first request from the mapped form rejected")
	}

	// The table must key on the canonical form: one bucket for both
	// spellings, not two buckets for one address.
	rl.mu.Lock()
	_, plain := rl.ips["192.0.2.1"]
	_, mapped := rl.ips["::ffff:192.0.2.1"]
	rl.mu.Unlock()
	if !plain {
		t.Fatalf("mapped IPv4 form was not canonicalized to the plain form")
	}
	if mapped {
		t.Fatalf("mapped spelling stored as its own bucket")
	}

	// Draining the bucket through the plain spelling must exhaust the
	// mapped spelling too.
	for i := 1; i < registerBurst; i++ {
		if !rl.allow("192.0.2.1") {
			t.Fatalf("request %d within burst rejected", i+1)
		}
	}
	if rl.allow("::ffff:192.0.2.1") {
		t.Fatalf("mapped spelling minted a fresh bucket after the plain form drained it")
	}
}

func TestRateLimiterHardCap(t *testing.T) {
	rl := newRateLimiter()

	// Spray three times the cap in distinct valid IPs.  The finding was
	// that every distinct IP string — including requests the limiter then
	// rejects — allocated a permanent entry, so the hammered IP below
	// (drained past its burst, hence refused) must be inside the cap too.
	for i := 0; i < 3*ipCacheLimit; i++ {
		rl.allow(netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)}).String())
	}
	for i := 0; i < registerBurst+5; i++ {
		rl.allow("2001:db8::bad")
	}

	rl.mu.Lock()
	n := len(rl.ips)
	rl.mu.Unlock()
	if n > ipCacheLimit {
		t.Fatalf("table holds %d entries, want at most the hard cap of %d", n, ipCacheLimit)
	}

	// The table still works after the eviction churn: a fresh IP gets its
	// full burst.
	for i := 0; i < registerBurst; i++ {
		if !rl.allow("2001:db8::fresh") {
			t.Fatalf("fresh IP's request %d rejected after cap churn", i+1)
		}
	}
}

func TestClientIP(t *testing.T) {
	// With the default (empty) trusted_proxies configuration no proxy is
	// trusted, so X-Forwarded-For is ignored even when the peer is
	// loopback — implicit loopback trust let a client behind any local
	// proxy mint fresh rate-limit buckets and forge audit IPs (LOW-6).
	tests := []struct {
		name         string
		remoteAddr   string
		forwardedFor string
		realIP       string
		want         string
	}{
		{
			name:       "direct connection uses RemoteAddr",
			remoteAddr: "198.51.100.7:44321",
			want:       "198.51.100.7",
		},
		{
			// XFF from a direct client is attacker input and must be
			// ignored, otherwise the client mints fresh buckets.
			name:         "spoofed XFF ignored on direct connection",
			remoteAddr:   "198.51.100.7:44321",
			forwardedFor: "1.2.3.4",
			want:         "198.51.100.7",
		},
		{
			// The LOW-6 regression: a loopback peer is no longer
			// implicitly trusted, so the whole chain is ignored.
			name:         "spoofed XFF ignored from loopback peer",
			remoteAddr:   "127.0.0.1:8080",
			forwardedFor: "1.2.3.4, 5.6.7.8",
			want:         "127.0.0.1",
		},
		{
			// Proxies do not strip a client-supplied X-Real-IP, so
			// it is never trusted to identify the client.
			name:       "X-Real-IP never trusted on direct connection",
			remoteAddr: "198.51.100.7:44321",
			realIP:     "1.2.3.4",
			want:       "198.51.100.7",
		},
		{
			name:       "X-Real-IP never trusted from loopback peer",
			remoteAddr: "127.0.0.1:8080",
			realIP:     "1.2.3.4",
			want:       "127.0.0.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/register", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.forwardedFor)
			}
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}
			if got := clientIP(req); got != tt.want {
				t.Fatalf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	// Flip the package-level trust set rather than the config file: main
	// installs the config-derived prefixes into exactly this var.
	orig := trustedProxies
	defer func() { trustedProxies = orig }()

	tests := []struct {
		name         string
		prefixes     []netip.Prefix
		remoteAddr   string
		forwardedFor string
		realIP       string
		want         string
	}{
		{
			// Only the rightmost XFF entry is trusted: everything to
			// its left may be client-supplied and spoofable.
			name:         "trusted proxy uses rightmost XFF entry",
			prefixes:     []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
			remoteAddr:   "127.0.0.1:8080",
			forwardedFor: "1.2.3.4, 198.51.100.7",
			want:         "198.51.100.7",
		},
		{
			// A garbage rightmost entry means the proxy appended
			// nothing trustworthy; fall back to the proxy itself.
			name:         "garbage XFF falls back to proxy IP",
			prefixes:     []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
			remoteAddr:   "127.0.0.1:8080",
			forwardedFor: "not-an-ip",
			want:         "127.0.0.1",
		},
		{
			// Trust is prefix-scoped, not address-listed: a corporate
			// range trusts every proxy inside it.
			name:         "prefix range trusts any proxy inside it",
			prefixes:     []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
			remoteAddr:   "10.1.2.3:8080",
			forwardedFor: "spoofed, 198.51.100.7",
			want:         "198.51.100.7",
		},
		{
			// A peer outside every configured prefix is a direct
			// client as far as this policy is concerned.
			name:         "peer outside configured prefixes ignores XFF",
			prefixes:     []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
			remoteAddr:   "10.0.0.9:8080",
			forwardedFor: "1.2.3.4",
			want:         "10.0.0.9",
		},
		{
			// The peer is canonicalized before the prefix match, so
			// an IPv4-mapped spelling of a trusted proxy is still
			// recognized (and an IPv4 prefix never matches an IPv6
			// peer).
			name:         "IPv4-mapped proxy spelling matches IPv4 prefix",
			prefixes:     []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
			remoteAddr:   "[::ffff:127.0.0.1]:8080",
			forwardedFor: "1.2.3.4, 198.51.100.7",
			want:         "198.51.100.7",
		},
		{
			// X-Real-IP is never trusted even behind a trusted proxy.
			name:       "X-Real-IP never trusted behind trusted proxy",
			prefixes:   []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
			remoteAddr: "127.0.0.1:8080",
			realIP:     "1.2.3.4",
			want:       "127.0.0.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trustedProxies = tt.prefixes
			req := httptest.NewRequest(http.MethodPost, "/register", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.forwardedFor)
			}
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}
			if got := clientIP(req); got != tt.want {
				t.Fatalf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPFly(t *testing.T) {
	// Flip the package-level detection var rather than the environment:
	// os.Setenv would not re-run the package var's initialization.
	orig := runningOnFly
	defer func() { runningOnFly = orig }()
	// The Fly gate and the configured-proxy gate are independent layers:
	// the trusted-proxy XFF logic still applies underneath a missing or
	// invalid Fly-Client-IP.
	proxyPrefixes := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	origProxies := trustedProxies
	defer func() { trustedProxies = origProxies }()

	tests := []struct {
		name         string
		onFly        bool
		remoteAddr   string
		flyClientIP  string
		forwardedFor string
		prefixes     []netip.Prefix
		want         string
	}{
		{
			// Fly's edge sets/overwrites Fly-Client-IP on proxied
			// requests, so behind the proxy it is authoritative.
			name:        "valid Fly-Client-IP is used",
			onFly:       true,
			remoteAddr:  "fdaa:0:1::3:4567",
			flyClientIP: "198.51.100.7",
			want:        "198.51.100.7",
		},
		{
			// An unparseable header value means the request did not
			// come through Fly's edge; the peer address is the best
			// available fact and a garbage value must never become
			// the key of a rate-limit bucket.
			name:        "invalid Fly-Client-IP falls back to RemoteAddr",
			onFly:       true,
			remoteAddr:  "fdaa:0:1::3:4567",
			flyClientIP: "not-an-ip",
			want:        "fdaa:0:1::3:4567",
		},
		{
			name:       "missing Fly-Client-IP falls back to RemoteAddr",
			onFly:      true,
			remoteAddr: "fdaa:0:1::3:4567",
			want:       "fdaa:0:1::3:4567",
		},
		{
			// Missing Fly-Client-IP (the request bypassed the edge)
			// falls through to the configured-proxy layer: a trusted
			// loopback proxy's XFF still identifies the client.
			name:         "missing Fly-Client-IP then trusted proxy XFF",
			onFly:        true,
			remoteAddr:   "127.0.0.1:8080",
			forwardedFor: "1.2.3.4, 198.51.100.7",
			prefixes:     proxyPrefixes,
			want:         "198.51.100.7",
		},
		{
			// Fly-Client-IP wins over XFF when both are present:
			// the edge overwrites its header, the proxy only appends.
			name:         "Fly-Client-IP beats XFF",
			onFly:        true,
			remoteAddr:   "127.0.0.1:8080",
			flyClientIP:  "198.51.100.7",
			forwardedFor: "1.2.3.4, 5.6.7.8",
			prefixes:     proxyPrefixes,
			want:         "198.51.100.7",
		},
		{
			// Outside Fly the header is pure client input: honoring
			// it would let a client mint fresh buckets per request.
			name:         "spoofed Fly-Client-IP ignored when not on Fly",
			remoteAddr:   "198.51.100.7:44321",
			flyClientIP:  "1.2.3.4",
			forwardedFor: "5.6.7.8",
			want:         "198.51.100.7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runningOnFly = tt.onFly
			trustedProxies = tt.prefixes
			req := httptest.NewRequest(http.MethodPost, "/register", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.flyClientIP != "" {
				req.Header.Set("Fly-Client-IP", tt.flyClientIP)
			}
			if tt.forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.forwardedFor)
			}
			if got := clientIP(req); got != tt.want {
				t.Fatalf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	t.Run("within burst passes through", func(t *testing.T) {
		rl := newRateLimiter()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := rl.middleware(next)

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/register", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 from wrapped handler, got %d", w.Code)
		}
	})

	t.Run("beyond burst gets 429", func(t *testing.T) {
		rl := newRateLimiter()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := rl.middleware(next)

		// httptest.NewRequest defaults RemoteAddr to 192.0.2.1:1234, so
		// draining that IP's bucket makes the next request 429.
		for i := 0; i < registerBurst; i++ {
			if !rl.allow("192.0.2.1") {
				t.Fatalf("pre-drain request %d rejected", i+1)
			}
		}

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/register", nil))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
		}
		if d := decodeDetail(t, w); !strings.Contains(d, "slow down") {
			t.Fatalf("expected rate-limit detail, got %q", d)
		}
	})

	t.Run("rotating XFF behind loopback proxy shares one bucket", func(t *testing.T) {
		// The LOW-6 demonstration: a loopback peer (e.g. the frontend
		// dev proxy, or any local reverse proxy) used to be implicitly
		// trusted, so a client rotating the rightmost XFF entry got a
		// fresh bucket every request.  With no configured prefixes the
		// bucket key is the proxy's own address, so rotation is
		// useless: the burst drains and the next request 429s.
		rl := newRateLimiter()
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := rl.middleware(next)

		for i := 0; i < registerBurst; i++ {
			req := httptest.NewRequest(http.MethodPost, "/register", nil)
			req.RemoteAddr = "127.0.0.1:8080"
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.18.%d.%d", i>>8, i))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("request %d within burst got %d, want 200", i+1, w.Code)
			}
		}

		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.RemoteAddr = "127.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "198.18.99.99")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("rotated XFF minted a fresh bucket: got %d, want 429", w.Code)
		}
	})
}
