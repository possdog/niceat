package main

import (
	"fmt"
	"strings"
)

// normalizeHandle validates and canonicalizes a requested handle label.
//
// The input is the subdomain label a user wants (e.g. "john" to become
// john.example.com).  Surrounding whitespace is trimmed and uppercase is
// lowercased — DNS names are case-insensitive, so this is a canonicalization
// rather than a semantic change.  The result must be a single DNS label:
// 1-63 characters of [a-z0-9-], not starting or ending with a hyphen, and
// not on the reserved list.
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
	if !handleRegex.MatchString(handle) {
		return "", fmt.Errorf("handle must be 1-63 characters: letters, digits, or hyphens, and cannot start or end with a hyphen")
	}
	if reservedLabels[handle] {
		return "", fmt.Errorf("handle %q is not available", handle)
	}
	return handle, nil
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
