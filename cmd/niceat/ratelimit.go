package main

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
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
	ipCacheLimit  = 10_000          // hard cap on tracked IPs: the table can never hold more, so a spray of distinct source IPs cannot grow it without bound
)

// ipLimiter wraps a per-IP rate.Limiter plus the bookkeeping for idle
// eviction.
type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
	// seq is the LRU sequence stamp this entry received on its last
	// request; evictLocked uses it to pick the stalest quarter of the
	// table once the hard cap is reached.
	seq uint64
}

// rateLimiter is a per-client-IP token bucket.  Each IP accumulates tokens
// at registerRate per second up to registerBurst; requests that arrive when
// the bucket is empty receive 429 Too Many Requests.
type rateLimiter struct {
	mu    sync.Mutex
	ips   map[string]*ipLimiter
	clock uint64 // last LRU sequence handed out; grows monotonically
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{ips: make(map[string]*ipLimiter)}
}

// allow reports whether the IP may make a request right now, creating its
// bucket on first sight.  The key is canonicalized first so equivalent
// spellings of one address share a single bucket.
func (rl *rateLimiter) allow(ip string) bool {
	ip = canonicalIP(ip)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.clock++
	entry, ok := rl.ips[ip]
	if !ok {
		// Hard cap: a sustained spray of distinct source IPs (trivial
		// from one IPv6 /64) keeps every entry fresh, so idle-time
		// eviction alone never shrinks the table — the red-team audit
		// measured an OOM of the 256 MB serving Machine this way
		// (finding HIGH-2).  Make room before inserting,
		// unconditionally, not only above a threshold.
		if len(rl.ips) >= ipCacheLimit {
			rl.evictLocked()
		}
		entry = &ipLimiter{limiter: rate.NewLimiter(registerRate, registerBurst)}
		rl.ips[ip] = entry
	}
	entry.lastSeen = time.Now()
	entry.seq = rl.clock
	return entry.limiter.Allow()
}

// canonicalIP maps equivalent spellings of the same address to one string so
// they share one bucket.  IPv4-mapped IPv6 forms (Go yields them for IPv4
// peers on dual-stack listeners, and header-derived values may carry them)
// are unwrapped, and netip's canonical String collapses textual variants
// ("::0:1" is "::1") that a caller could otherwise use to mint extra
// buckets for a single address.  Values that do not parse (the callers
// gate on validity, but be safe) keep their own bucket.
func canonicalIP(ip string) string {
	if addr, err := netip.ParseAddr(ip); err == nil {
		return addr.Unmap().String()
	}
	return ip
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

// evictLocked makes room for one new entry when the table is at its hard
// cap.  Callers must hold rl.mu.
//
// Idle entries go first: an entry idle for ipIdleTTL has a full token
// bucket again anyway (burst refills in well under the TTL), so it
// contributes nothing but table bulk.  When a sustained spray keeps
// everything fresh, the least-recently-used quarter of the table goes
// instead — in one batch, so the O(n) selection pass amortizes to O(1)
// per new entry rather than costing a scan per request.  LRU order (seq is
// refreshed on every request) rather than FIFO: an attacker rotating
// source IPs can already mint fresh buckets at will, so evicting its own
// stale entries gains it nothing — but FIFO would let the same spray push
// out the entries of well-behaved repeat users.
func (rl *rateLimiter) evictLocked() {
	rl.sweepLocked()
	if len(rl.ips) < ipCacheLimit {
		return
	}
	seqs := make([]uint64, 0, len(rl.ips))
	for _, e := range rl.ips {
		seqs = append(seqs, e.seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	cutoff := seqs[len(seqs)/4]
	for ip, e := range rl.ips {
		if e.seq <= cutoff {
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

// trustedProxies are the CIDR prefixes of reverse proxies whose
// X-Forwarded-For may be trusted to identify the real client.  It is
// installed once at startup from the config's "trusted_proxies" field and
// is empty by default: implicit trust in "the peer is loopback" let any
// client behind a local proxy rotate the rightmost XFF entry per request
// and mint a fresh token bucket (plus a forged audit IP) every time
// (red-team finding LOW-6).  Opt-in configuration means a deployment
// states which proxies it actually fronts.  Package-level, like
// runningOnFly, so main can install the config-derived set and tests can
// flip it without touching the environment.
var trustedProxies []netip.Prefix

// isTrustedProxy reports whether the given host falls inside one of the
// configured trusted-proxy prefixes.  The host is canonicalized first so
// an IPv4-mapped spelling of a configured address cannot dodge (or sneak
// past) a prefix; Prefix.Contains is false for mismatched address
// families, so an IPv4 peer never matches an IPv6-only prefix.
func isTrustedProxy(host string) bool {
	addr, err := netip.ParseAddr(canonicalIP(host))
	if err != nil {
		return false
	}
	for _, p := range trustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
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
// When running behind a reverse proxy whose address falls inside a
// configured trusted_proxies prefix (e.g. Caddy on localhost — which must
// now be named explicitly in the config), r.RemoteAddr will be the
// proxy's IP.  Proxies append the address of their immediate peer to the
// X-Forwarded-For chain, so with a single trusted hop the rightmost entry
// is the one the trusted proxy appended and everything to its left may be
// client-supplied and spoofable.  We therefore use the rightmost entry;
// taking the leftmost would let a client rotate fake IPs and get a fresh
// rate-limit bucket per request.  If the rightmost entry is not a valid
// IP, the proxy did not append one and the whole chain is untrustworthy —
// fall back to the proxy's own IP.  X-Real-IP is never trusted because
// proxies do not strip a client-supplied X-Real-IP.  With no configured
// prefixes (the default) no proxy is trusted and the peer address is the
// client IP, whatever X-Forwarded-For claims.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr has no port; fall back to the raw value.
		host = r.RemoteAddr
	}

	// Fly's edge overwrites Fly-Client-IP, so behind the proxy it is
	// authoritative; a missing or unparseable value means the request
	// bypassed Fly's edge and we fall back to RemoteAddr's host (the
	// trusted-proxy XFF logic below still applies on top of that).
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
