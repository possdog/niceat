package main

import (
	"regexp"
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
	// single deployment can serve *.wagg.ing, *.yeen.world, ... at once.
	domains map[string]bool
	// provisioner creates the DNS record that maps a handle to a DID.
	// The real implementation (porkbun.go, not yet written) talks to the
	// Porkbun API; tests use a fake.
	provisioner provisioner
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
