package main

import (
	"net/http"
	"net/http/httptest"
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

func TestClientIP(t *testing.T) {
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
			// Only the rightmost XFF entry is trusted: everything to
			// its left may be client-supplied and spoofable.
			name:         "trusted proxy uses rightmost XFF entry",
			remoteAddr:   "127.0.0.1:8080",
			forwardedFor: "1.2.3.4, 198.51.100.7",
			want:         "198.51.100.7",
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
			// A garbage rightmost entry means the proxy appended
			// nothing trustworthy; fall back to the proxy itself.
			name:         "garbage XFF falls back to proxy IP",
			remoteAddr:   "127.0.0.1:8080",
			forwardedFor: "not-an-ip",
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
			name:       "X-Real-IP never trusted behind proxy",
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

func TestClientIPFly(t *testing.T) {
	// Flip the package-level detection var rather than the environment:
	// os.Setenv would not re-run the package var's initialization.
	orig := runningOnFly
	defer func() { runningOnFly = orig }()

	tests := []struct {
		name         string
		onFly        bool
		remoteAddr   string
		flyClientIP  string
		forwardedFor string
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
}
