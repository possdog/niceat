package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The handle reaper's core question is "does this account still use this
// handle?", and the account — not DNS — is the authority on that.  A DID
// document's alsoKnownAs array lists the handles an account currently
// uses as at://<handle> entries; when a user changes their handle in-app,
// the old handle drops out of the list even though the DNS record remains.
// That stale-record-plus-dropped-handle state is what the reaper cleans up.

// plcDirectoryBaseURL is the production DID directory for did:plc.
const plcDirectoryBaseURL = "https://plc.directory"

// didDocMaxResponse caps how much of a DID document we buffer; documents
// are a few KiB, so a bounded read keeps a hostile endpoint from making us
// allocate without limit.
const didDocMaxResponse = 256 << 10

// didResolver answers "which handles does this account actually use?".
//
// The contract matters for reaping safety:
//
//   - registered=false, err=nil is AFFIRMATIVE evidence that the account no
//     longer exists (a 404 from the directory).  The reaper may treat the
//     handle as unused — subject to the grace period.
//   - err != nil means the lookup failed or the method cannot be verified;
//     NO conclusion may be drawn.  The reaper skips the record and tries
//     again next pass, never reaping on the basis of an error.
type didResolver interface {
	resolve(ctx context.Context, did string) (aka []string, registered bool, err error)
}

// plcDirectory resolves DIDs the way Bluesky itself does: did:plc through
// plc.directory, did:web through the DID's own domain per the did:web spec
// (see webDocumentURL).
type plcDirectory struct {
	baseURL string
	hc      *http.Client
}

// newPlcDirectory returns a resolver; an empty baseURL selects the
// production plc.directory.  The did:web method uses the DID's own domain,
// so only the plc.baseURL is configurable (tests point it at httptest).
func newPlcDirectory(baseURL string) *plcDirectory {
	if baseURL == "" {
		baseURL = plcDirectoryBaseURL
	}
	return &plcDirectory{
		baseURL: baseURL,
		hc:      &http.Client{Timeout: 10 * time.Second},
	}
}

// resolve implements didResolver.  Unsupported DID methods are an error,
// not "unregistered": the reaper only deletes what it can affirmatively
// show is unused.
func (d *plcDirectory) resolve(ctx context.Context, did string) ([]string, bool, error) {
	switch {
	case strings.HasPrefix(did, "did:plc:"):
		return d.fetchDocument(ctx, d.baseURL+"/"+did, did)
	case strings.HasPrefix(did, "did:web:"):
		u, err := webDocumentURL(did)
		if err != nil {
			return nil, false, err
		}
		return d.fetchDocument(ctx, u, did)
	default:
		return nil, false, fmt.Errorf("unsupported DID method")
	}
}

// webDocumentURL maps a did:web DID to its document URL per the did:web
// spec (https://w3c-ccg.github.io/did-method-web/): the first
// colon-separated segment of the method-specific identifier is the host
// (an optional port colon is percent-encoded, e.g. %3A8443), each further
// segment becomes one URL path segment, and the document lives at
// <path>/did.json — /.well-known/did.json only for the root form.
//
// Hostile identifiers are errors, never best-effort mappings: a segment
// that decodes to empty, ".", "..", or contains "/" could otherwise build
// a URL with path traversal or a path other than the spec mapping, and a
// resolver error is the fail-safe inconclusive signal (the reaper skips,
// it never reaps on an error).  The same goes for percent-decode
// failures — a malformed identifier is not something to guess at.  The
// url.Parse backstop re-checks the finally-constructed URL against the
// mapping, so characters that change URL meaning (raw "?" or "#", a host
// that decodes with a "/" in it) are caught even though the per-segment
// checks above look only at decoded content.
func webDocumentURL(did string) (string, error) {
	remainder := strings.TrimPrefix(did, "did:web:")
	if remainder == "" {
		return "", fmt.Errorf("empty did:web identifier")
	}
	segs := strings.Split(remainder, ":")
	hostSegs := 1
	// The spec wants the port colon percent-encoded (%3A), but a raw
	// all-digit segment right after the host has always parsed as a
	// port here (url.Parse reads "host:123" as host:port), and the
	// pre-existing validate/resolver behavior and tests rely on that.
	// Keep it: treating such a segment as a path would silently change
	// root-form behavior.
	if len(segs) > 1 && isASCIIDigits(segs[1]) {
		hostSegs = 2
	}
	host, err := url.PathUnescape(strings.Join(segs[:hostSegs], ":"))
	if err != nil || host == "" {
		return "", fmt.Errorf("invalid did:web host %q", strings.Join(segs[:hostSegs], ":"))
	}
	// The backstop compares the parsed URL's path against the fully
	// decoded mapping, so keep the decoded segments for that check while
	// the URL itself is built from the identifier's raw (still-encoded)
	// segments — re-encoding a decoded segment would be ambiguous about
	// literal "%" characters.
	decoded := make([]string, 0, len(segs)-hostSegs)
	for _, seg := range segs[hostSegs:] {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return "", fmt.Errorf("invalid did:web path segment %q", seg)
		}
		// "." and ".." are traversal, "" and anything with "/" would
		// change the path shape the spec prescribes; refuse all of them.
		if dec == "" || dec == "." || dec == ".." || strings.Contains(dec, "/") {
			return "", fmt.Errorf("did:web path segment %q is not a usable path element", seg)
		}
		decoded = append(decoded, dec)
	}
	want := "/.well-known"
	rawPath := "/.well-known"
	if len(decoded) > 0 {
		want = "/" + strings.Join(decoded, "/")
		rawPath = "/" + strings.Join(segs[hostSegs:], "/")
	}
	u, err := url.Parse("https://" + host + rawPath + "/did.json")
	if err != nil || u.Host != host || u.Path != want+"/did.json" {
		return "", fmt.Errorf("did:web identifier %q does not map to a well-formed document URL", did)
	}
	return u.String(), nil
}

// isASCIIDigits reports whether s is non-empty and all ASCII digits.
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// fetchDocument retrieves and validates the DID document at url, returning
// its alsoKnownAs entries.  A 404 is affirmative: the account is gone.
// Any other non-2xx, a transport failure, or a document whose id does not
// match the requested DID is an inconclusive error.
func (d *plcDirectory) fetchDocument(ctx context.Context, url, did string) ([]string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/did+json, application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, false, fmt.Errorf("directory returned %s", resp.Status)
	}

	var doc struct {
		ID          string   `json:"id"`
		AlsoKnownAs []string `json:"alsoKnownAs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, didDocMaxResponse)).Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("decoding DID document: %w", err)
	}
	// The directory's routing guarantees id == requested DID; a mismatch
	// means something unexpected is answering (misconfigured did:web
	// host, MITM, upstream bug) — inconclusive, never a reap.
	if doc.ID != did {
		return nil, false, fmt.Errorf("document id %q does not match requested DID %q", doc.ID, did)
	}
	return doc.AlsoKnownAs, true, nil
}

// akaHasHandle reports whether the alsoKnownAs list contains the given
// handle as an at:// entry.  Handles are case-insensitive (DNS names), so
// the comparison is folded even though documents normally store lowercase.
func akaHasHandle(aka []string, handle string) bool {
	want := "at://" + strings.ToLower(handle)
	for _, a := range aka {
		if strings.ToLower(a) == want {
			return true
		}
	}
	return false
}
