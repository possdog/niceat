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
// plc.directory, did:web through the domain's /.well-known/did.json.
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
		host := strings.TrimPrefix(did, "did:web:")
		// Only root-DID documents (a bare host) are verified.  Per the
		// did:web spec, DIDs with path segments (colon- or
		// slash-separated) serve their document deeper in the tree;
		// those are vanishingly rare for Bluesky accounts, and treating
		// them as unverifiable (never reaped) is the safe default.
		// url.Parse rejects path-splattered or bad-port hosts.
		u, err := url.Parse("https://" + host + "/.well-known/did.json")
		if err != nil || u.Host != host || u.Path != "/.well-known/did.json" {
			return nil, false, fmt.Errorf("did:web with path segments is not supported")
		}
		return d.fetchDocument(ctx, u.String(), did)
	default:
		return nil, false, fmt.Errorf("unsupported DID method")
	}
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
