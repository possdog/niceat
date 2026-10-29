package main

import (
	"regexp"
	"sync"
)

// maxRequestBody bounds the size of a registration request.  A well-formed
// request is a few hundred bytes; anything larger is abusive, and bounding it
// up front means a hostile client cannot make us buffer an unbounded body.
const maxRequestBody = 4 << 10 // 4 KiB

// maxDIDLength bounds the total length of an accepted DID.  A did:plc is 32
// bytes; a did:web may be longer but is still a URL.  The DID ends up as the
// value of a DNS TXT record ("did=<DID>", RFC 1035 string capped at 255
// octets), so it must stay well under that.
const maxDIDLength = 200

// Pre-compiled validation regexes.
//
// plcRegex matches did:plc DIDs: the "did:plc:" prefix followed by exactly 24
// characters of the Base32-BTC alphabet ([a-z2-7]) used by the plc method.
//
// webRegex matches did:web DIDs: a percent-encoded host (optionally with port)
// followed by optional %-encoded path segments.  The character class is the
// did:web spec's pchar set plus ":" and "@"; it excludes whitespace, quotes
// and semicolons, so a matched DID can never inject additional content into
// the TXT record we publish.
//
// The handle label regex lives in validate.go next to normalizeHandle, which
// enforces the union of the DNS-label and Bluesky-username specs.
var (
	plcRegex = regexp.MustCompile(`^did:plc:[a-z2-7]{24}$`)
	webRegex = regexp.MustCompile(`^did:web:[A-Za-z0-9._~%!$&'()*+,;=:@/-]+$`)
)

// reservedLabels are subdomain labels that can never be claimed through the
// public form.  Two reasons a label lands here:
//
//  1. The domain owner (or common tooling) plausibly uses it for mail,
//     nameservice, web, or other infrastructure (www, mx, ns1, autodiscover,
//     ...).  Handing these out would let a stranger shadow or pre-empt the
//     owner's own records.
//  2. It is protocol- or product-sensitive and would confuse Bluesky's
//     handle resolution or social-engineer users (_atproto, bsky, bluesky,
//     plc, pds, ...).
//
// This list is intentionally generous: adding names later is harmless, while
// recalling a claimed one is not.  Extend freely.
var reservedLabels = map[string]bool{
	// Mail infrastructure (MX targets, client autodiscovery, SPF/MTA names).
	"mail": true, "email": true, "webmail": true, "smtp": true,
	"imap": true, "pop": true, "pop3": true, "mx": true,
	"autodiscover": true, "autoconfig": true, "mta-sts": true,
	// Nameservice / infrastructure.
	"ns": true, "ns1": true, "ns2": true, "dns": true, "whois": true,
	"api": true, "admin": true, "root": true, "cpanel": true, "whm": true,
	// Role-account style names that impersonate the operator.
	"postmaster": true, "hostmaster": true, "webmaster": true,
	"abuse": true, "security": true, "support": true, "help": true,
	"info": true, "contact": true,
	// Common web properties the owner may want for itself.
	"www": true, "blog": true, "shop": true, "store": true,
	"app": true, "apps": true, "dev": true, "staging": true, "test": true,
	"cdn": true, "static": true, "assets": true, "media": true,
	"img": true, "images": true, "video": true, "docs": true, "doc": true,
	"status": true, "wiki": true, "forum": true, "chat": true,
	"discord": true, "matrix": true, "social": true, "git": true,
	"ftp": true, "ssh": true, "vpn": true, "cloud": true,
	// AT Protocol / Bluesky-specific names.  "_atproto" is also rejected by
	// handleRegex (leading underscore) but is listed so the intent is
	// explicit if the regex ever loosens.
	"_atproto": true, "atproto": true, "bsky": true, "bluesky": true,
	"plc": true, "pds": true, "relay": true, "handle": true,
	"did": true, "mod": true, "moderation": true,
}

// app bundles the configuration and collaborators the handlers need.
type app struct {
	// domains is the set of apex domains this instance manages, keyed by
	// normalized lowercase name (parseDomain's output).  Each request's
	// Host header selects which domain a handle is registered under, so a
	// single deployment can serve *.wagg.ing, *.vore.rip, ... at once.
	domains map[string]bool
	// provisioner creates the DNS record that maps a handle to a DID.
	// The Porkbun client (porkbun.go) is the real implementation; without
	// credentials an in-memory fallback is used.  Tests use fakes.
	provisioner provisioner
	// locks serializes the check-then-create registration flow per
	// (domain, label) pair; see handlers.go for why.
	locks labelLocks
}

// labelLockStripes is the fixed number of lock stripes in a labelLocks
// table.  A few hundred stripes make two *live* labels colliding on one
// stripe rare, while the table's footprint stays constant.
const labelLockStripes = 256

// labelLocks provides mutual exclusion per (domain, label) pair.  It is a
// fixed-size striped lock table, not a per-key map: a map keyed by
// (domain, label) never shrank — one entry was allocated per
// validation-passing request before Provision ran, so even requests that
// went on to fail (409, 500) grew it, and a spray of distinct labels was an
// unbounded heap leak that could OOM the serving Machine (red-team finding
// HIGH-2).  With stripes, memory is O(1) no matter how many distinct
// labels pass through.  The trade: two different labels occasionally share
// a stripe and serialize with each other — harmless extra contention, since
// the lock is held only for the brief check-then-create.  The same
// (domain, label) always maps to the same stripe, which is what preserves
// the no-record-stacking guarantee described on acquire.
type labelLocks struct {
	stripes [labelLockStripes]sync.Mutex
}

// acquire locks the (domain, label) pair and returns the unlock func.  The
// DNS-as-truth registration flow is a check-then-create over two Porkbun
// calls; without this lock, two concurrent requests for the same label
// could both see "no record" and both create, stacking two TXT records on
// one _atproto name — which per the atproto spec breaks the handle for both
// parties.  A process-local lock suffices while the deployment is a single
// instance; multiple instances would need a shared lock (or Porkbun-side
// conditional creates, which the API does not offer).
func (ll *labelLocks) acquire(domain, label string) func() {
	m := &ll.stripes[labelStripe(domain, label)]
	m.Lock()
	return m.Unlock
}

// labelStripe maps a (domain, label) pair to its lock stripe via FNV-1a
// over the NUL-joined key.  Any decent hash would do — the only property
// acquire relies on is that the choice is a pure function of
// (domain, label), never of timing, so the same label always lands on the
// same stripe even as entries come and go.
func labelStripe(domain, label string) uint32 {
	key := domain + "\x00" + label
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return h % labelLockStripes
}

// --- request / response types (JSON) ---

// registerRequest is the JSON body of POST /register.
type registerRequest struct {
	// Handle is the desired subdomain label, e.g. "john" for john.example.com.
	// Case and surrounding whitespace are normalized away.
	Handle string `json:"handle"`
	// DID is the Bluesky account DID, e.g. "did:plc:...".  DIDs are
	// case-sensitive and are never normalized.
	DID string `json:"did"`
}

// registerResponse is the success body of POST /register.
type registerResponse struct {
	Status string `json:"status"`
	Handle string `json:"handle"`
}

// errorResponse is the failure body of every endpoint, matching the shape
// { "detail": "..." } used across the sibling projects.
type errorResponse struct {
	Detail string `json:"detail"`
}
