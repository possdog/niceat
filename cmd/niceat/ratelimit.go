package main

import (
	"log"
	"net"
	"net/http"
	"os"
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

// runningOnFly reports whether this process runs as a Fly.io Machine (Fly
// injects FLY_MACHINE_ID into every Machine).  Fly-Client-IP is trustworthy
// only behind Fly's edge, so the gate must exist: outside Fly nothing strips
// or overwrites Fly-Client-IP, so honoring it unconditionally would let any
// client mint fresh rate-limit buckets (and fake audit IPs) per request just
// by setting the header.
// Package-level so tests can flip it without touching the environment.
var runningOnFly = os.Getenv("FLY_MACHINE_ID") != ""

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
// When running on Fly.io (detected via FLY_MACHINE_ID), TLS terminates at
// Fly's edge proxy and the request arrives over Fly's internal network, so
// r.RemoteAddr is Fly's proxy infrastructure — not the client, and not a
// localhost proxy the X-Forwarded-For logic below could recognize.  Fly's
// edge sets or overwrites Fly-Client-IP on every proxied request, so a
// client cannot forge it once the request has passed through Fly's proxy;
// the FLY_MACHINE_ID gate keeps that trust from extending to non-Fly
// deployments, where nothing strips the header and it would be pure
// client input.  A present-but-invalid value is not trusted: it means the
// request did not come through Fly's edge (e.g., another Machine on the
// internal network), and the peer address is the best available fact —
// falling back to a spoofable value would be worse than the peer's.
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

	// Fly's edge overwrites Fly-Client-IP, so behind the proxy it is
	// authoritative; a missing or unparseable value means the request
	// bypassed Fly's edge and we fall back to RemoteAddr's host (the
	// localhost XFF logic below still applies on top of that).
	if runningOnFly {
		if flyIP := r.Header.Get("Fly-Client-IP"); validIP(flyIP) {
			return flyIP
		}
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
