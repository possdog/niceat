package main

import (
	"fmt"
	"regexp"
	"strings"
)

// domainRegex matches a fully-qualified domain name composed of well-formed
// DNS labels: 1-63 characters of [a-z0-9-] per label, none starting or
// ending with a hyphen, at least two labels.  The multi-label requirement
// excludes single-word pseudo-domains like "localhost" — a domain here must
// be something Porkbun can actually manage.
var domainRegex = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// parseDomain validates and canonicalizes a configured domain name.
// Input is trimmed, lowercased, and stripped of a trailing FQDN dot; the
// result must be a plain dotted domain of well-formed labels, at most 253
// characters (the DNS name limit).  The returned error's message is meant
// for the operator reading a startup log, not for end users.
func parseDomain(raw string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(raw))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("domain is required")
	}
	if len(domain) > 253 {
		return "", fmt.Errorf("domain exceeds the 253-character DNS limit")
	}
	if !domainRegex.MatchString(domain) {
		return "", fmt.Errorf("must be a dotted domain of DNS labels (letters, digits, hyphens)")
	}

	// AT Proto handle constraints on the final label (atproto.com/specs/
	// handle): the TLD "can not start with a numeric digit", and the
	// reference implementation additionally refuses registration under
	// reserved TLDs (disallowedTLDs).  A deployment under such a domain
	// could never produce a valid handle, so both fail at startup.
	tld := domain[strings.LastIndex(domain, ".")+1:]
	if tld[0] < 'a' || tld[0] > 'z' {
		return "", fmt.Errorf("TLD must start with a letter, not a digit (AT Proto handle rule)")
	}
	if disallowedTLDs[tld] {
		return "", fmt.Errorf("TLD %q is reserved and cannot host AT Proto handles", tld)
	}
	return domain, nil
}

// handleLabelRegex matches a single DNS label: 1-63 characters of [a-z0-9-],
// not starting or ending with a hyphen.  This is the syntax shared by DNS
// labels and AT Proto handle segments (atproto.com/specs/handle:
// "segments can not start or end with a hyphen", 1-63 chars, ASCII
// letters/digits/hyphens only; punycode "xn--" labels are explicitly valid).
//
// Input is lowercased and trimmed before matching, so the lowercase-only
// class is fine.  Dots are rejected separately with a specific message so a
// caller cannot reach outside the served domain; underscore-prefixed labels
// (_atproto et al.) fail the character class and are additionally reserved.
var handleLabelRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Bluesky's app-level username rules and niceat's own policies.
//
// The app-level rules come from the official support article "What is the
// suggested user handle format?" (blueskyweb.zendesk.com, "User Handle –
// Error Messages"): "Handle Too Long – Custom domain users: up to 30
// characters total".  The AT Proto handle spec allows far more (253 chars
// total, 244 in practice because DNS verification prefixes "_atproto."),
// but a handle longer than 30 characters cannot be entered in bsky.app's
// custom-domain handle flow — the user would own a DNS record for a handle
// the official app will not let them claim.  niceat enforces the app's
// budget so every provisioned handle is claimable end-to-end.
//
// minUserHandleLabel is a niceat policy, not an upstream requirement:
// Bluesky enforces no minimum for custom-domain handles (short ones like
// s.example.com work).  We keep a floor anyway — short labels are few and
// valuable under any single domain, and a minimum discourages squatting
// them one or two characters at a time.
const (
	// minUserHandleLabel is the shortest label niceat will provision.
	minUserHandleLabel = 3
	// maxAppHandleLength is the composed handle length (label + "." +
	// domain) bsky.app accepts for custom-domain handles.
	maxAppHandleLength = 30
)

// disallowedTLDs are the TLDs the AT Proto reference implementation refuses
// to register handles under (@atproto/syntax handle.ts, DISALLOWED_TLDS).
// A niceat deployment under one of these could never produce a usable
// handle, so parseDomain rejects them at startup.
var disallowedTLDs = map[string]bool{
	"local": true, "arpa": true, "invalid": true, "localhost": true,
	"internal": true, "example": true, "alt": true, "onion": true,
}

// normalizeHandle validates and canonicalizes a requested handle label.
//
// The input is the subdomain label a user wants (e.g. "john" to become
// john.example.com).  Surrounding whitespace is trimmed and uppercase is
// lowercased — DNS names and handles are case-insensitive, so this is a
// canonicalization rather than a semantic change.  The result must be a
// single DNS label that is also a valid Bluesky username: 3-63 characters of
// [a-z0-9-], not starting or ending with a hyphen, and not on the reserved
// list.  (See handleLabelRegex and minUserHandleLabel for the union of the
// two specs; the composed-handle length budget is checked separately by
// ensureHandleClaimable once the target domain is known.)
//
// The returned error's message is safe to show the user verbatim.
func normalizeHandle(raw string) (string, error) {
	handle := strings.ToLower(strings.TrimSpace(raw))
	if handle == "" {
		return "", fmt.Errorf("handle is required")
	}
	if strings.Contains(handle, ".") {
		return "", fmt.Errorf("handle must be a single name, not a full domain (dots are not allowed)")
	}
	if len(handle) < minUserHandleLabel {
		return "", fmt.Errorf("handle must be at least %d characters", minUserHandleLabel)
	}
	if !handleLabelRegex.MatchString(handle) {
		return "", fmt.Errorf("handle must be 3-63 characters: letters, digits, or hyphens, and cannot start or end with a hyphen")
	}
	if reservedLabels[handle] {
		return "", fmt.Errorf("handle %q is not available", handle)
	}
	return handle, nil
}

// ensureHandleClaimable checks the composed handle (label + "." + domain)
// against bsky.app's custom-domain budget: at most maxAppHandleLength
// characters total, per the official Bluesky support docs.  This is the
// binding constraint — it sits far below the AT Proto limits (253 total,
// 244 with the "_atproto." DNS prefix) — so one check covers both specs.
//
// The error names the per-domain maximum so the frontend can display it
// directly ("at most 21 characters under wagg.ing").
func ensureHandleClaimable(label, domain string) error {
	max := maxAppHandleLength - len(domain) - 1
	if len(label) > max {
		return fmt.Errorf("handle is too long: at most %d characters are available under %s", max, domain)
	}
	return nil
}

// validateDID validates a Bluesky account DID.
//
// Only did:plc and did:web are accepted: these are the methods Bluesky's
// resolver supports in practice, and restricting the method lets the rest of
// the DID be checked against a tight character class.  That matters because
// the DID becomes the value of a public TXT record ("did=<DID>") — the
// per-method regexes exclude whitespace, quotes, and record separators, so a
// validated DID can only ever produce exactly one TXT string.
//
// DIDs are case-sensitive per spec, so only surrounding whitespace is trimmed.
// The returned error's message is safe to show the user verbatim.
func validateDID(raw string) (string, error) {
	did := strings.TrimSpace(raw)
	// bsky.app's "Change Handle" flow hands users the exact TXT record
	// value to configure — "did=<DID>" — and pasting it verbatim is the
	// natural thing to do.  A real DID never starts with "did=" (it
	// starts with "did:"), so stripping that prefix is unambiguous; trim
	// again for pastes that pick up a space after the equals sign.
	did = strings.TrimPrefix(did, "did=")
	did = strings.TrimSpace(did)
	if did == "" {
		return "", fmt.Errorf("did is required")
	}
	if len(did) > maxDIDLength {
		return "", fmt.Errorf("did is too long (maximum %d characters)", maxDIDLength)
	}
	if plcRegex.MatchString(did) || webRegex.MatchString(did) {
		return did, nil
	}
	return "", fmt.Errorf("did must be a valid did:plc or did:web identifier")
}
