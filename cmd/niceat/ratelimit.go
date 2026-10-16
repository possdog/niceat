package main

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Per-client rate limiting defaults.  Registration is a rare, human action:
// a small burst covers fat-finger retries while a slow sustained rate makes
// automated abuse (label probing, mass squatting) unattractive.  A shared
// IP (CGNAT, VPN, university network) still gets the full burst, so
// collateral throttling of legitimate users is unlikely.
const (
	registerRate  = rate.Limit(0.1) // one request per 10 s sustained
	registerBurst = 5               // tolerate a handful of quick retries
	ipIdleTTL     = time.Hour       // idle IPs are evicted from the table
	ipCacheLimit  = 10_000          // sweep threshold, bounds memory under IP-spray abuse
)

// ipLimiter wraps a per-IP rate.Limiter plus the bookkeeping for idle
// eviction.
type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// rateLimiter is a per-client-IP token bucket.  Each IP accumulates tokens
// at registerRate per second up to registerBurst; requests that arrive when
// the bucket is empty receive 429 Too Many Requests.
type rateLimiter struct {
	mu  sync.Mutex
	ips map[string]*ipLimiter
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{ips: make(map[string]*ipLimiter)}
}

// allow reports whether the IP may make a request right now, creating its
// bucket on first sight.
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, ok := rl.ips[ip]
	if !ok {
		entry = &ipLimiter{limiter: rate.NewLimiter(registerRate, registerBurst)}
		rl.ips[ip] = entry
	}
	entry.lastSeen = time.Now()

	// Opportunistic eviction: an entry idle for ipIdleTTL has a full token
	// bucket again anyway (burst refills in well under the TTL), so it
	// contributes nothing but table bulk.  Sweeping only past the size
	// threshold keeps the common path allocation-free.
	if len(rl.ips) > ipCacheLimit {
		rl.sweepLocked()
	}
	return entry.limiter.Allow()
}

// sweepLocked evicts entries idle longer than ipIdleTTL.  Callers must hold
// rl.mu.
func (rl *rateLimiter) sweepLocked() {
	cutoff := time.Now().Add(-ipIdleTTL)
	for ip, e := range rl.ips {
		if e.lastSeen.Before(cutoff) {
			delete(rl.ips, ip)
		}
	}
}

// isTrustedProxy reports whether the given host is a trusted reverse proxy.
// Currently only localhost (127.0.0.1 and ::1) is trusted.
func isTrustedProxy(host string) bool {
	return host == "127.0.0.1" || host == "::1"
}

// validIP reports whether the given string is a valid IP address.
func validIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

// middleware wraps an http.Handler with per-IP rate limiting.  Clients that
// exceed their allowance receive 429 Too Many Requests.
func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.allow(ip) {
			log.Printf("WARN: rate limit exceeded for %s %s", r.Method, r.URL.Path)
			writeJSON(w, http.StatusTooManyRequests, errorResponse{
				Detail: "Rate limit exceeded. Please slow down.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the caller's IP from the request.
//
// When the server is directly exposed to the internet (no reverse proxy),
// r.RemoteAddr is the kernel-supplied TCP source IP and is used directly.
//
// When running behind a trusted reverse proxy (e.g., Caddy on localhost),
// r.RemoteAddr will be the proxy's IP.  Proxies append the address of their
// immediate peer to the X-Forwarded-For chain, so with a single trusted hop
// the rightmost entry is the one the trusted proxy appended and everything
// to its left may be client-supplied and spoofable.  We therefore use the
// rightmost entry; taking the leftmost would let a client rotate fake IPs
// and get a fresh rate-limit bucket per request.  If the rightmost entry is
// not a valid IP, the proxy did not append one and the whole chain is
// untrustworthy — fall back to the proxy's own IP.  X-Real-IP is never
// trusted because proxies do not strip a client-supplied X-Real-IP.
// Only localhost is considered a trusted proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr has no port; fall back to the raw value.
		host = r.RemoteAddr
	}

	if isTrustedProxy(host) {
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		ip := strings.TrimSpace(parts[len(parts)-1])
		if validIP(ip) {
			return ip
		}
		return host
	}

	// Direct connection or untrusted proxy: use RemoteAddr's host.
	return host
}
