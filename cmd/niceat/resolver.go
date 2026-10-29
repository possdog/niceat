package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
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
//   - registered=false, err=nil is AFFIRMATIVE evidence that the account
//     no longer exists, and the reaper may treat the handle as unused —
//     subject to the grace period.  Two shapes count: a 404 from the
//     directory (did:plc), and — for did:web, where the host IS the
//     identity — the DNS system's authoritative answer that the DID's
//     host does not exist (see dnsNameNotFound).
//   - err != nil means the lookup failed or the method cannot be verified;
//     NO conclusion may be drawn.  The reaper skips the record and tries
//     again next pass, never reaping on the basis of an error.
//
// One error is typed: a *rateLimitedError from a did:plc lookup means
// plc.directory answered 429 — the directory is refusing our fetch
// VOLUME, which says nothing about any DID, and the reaper reacts by
// pausing directory fetches for the rest of the sweep instead of
// amplifying an attacker-steered request volume into the limit (LOW-7).
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
// The returned client refuses redirects and non-public dial destinations
// (see resolverHTTPClient / ssrfGuardedTransport); tests that need to
// reach their loopback httptest servers replace d.hc after construction.
func newPlcDirectory(baseURL string) *plcDirectory {
	if baseURL == "" {
		baseURL = plcDirectoryBaseURL
	}
	return &plcDirectory{
		baseURL: baseURL,
		hc:      resolverHTTPClient(ssrfGuardedTransport(nil)),
	}
}

// resolverHTTPClient returns the resolver's HTTP client.  Redirects are
// never followed (http.ErrUseLastResponse returns the 3xx response to the
// caller as the final answer): a did:web host is attacker-chosen, so a 302
// there could otherwise steer the reaper Machine's fetch anywhere —
// including a scheme downgrade to plain http.  Because there are no hops,
// no hop-by-hop re-validation is needed; the unfollowed 3xx is simply a
// non-2xx status, i.e. an inconclusive error the reaper skips on.
// The transport is an http.RoundTripper (not *http.Transport) so tests can
// script whole responses without a network; production always passes an
// *http.Transport.
func resolverHTTPClient(rt http.RoundTripper) *http.Client {
	return &http.Client{
		// 5 s, not a rounder 10: legitimate plc.directory and did:web
		// fetches complete in well under a second, so anything
		// approaching this bound is a stalled or hostile endpoint — and
		// this timeout is exactly what a deliberately hanging DID costs
		// the reaper's bounded sweep (one worker for the full timeout).
		// 5 s keeps enough slack that a slow-but-healthy path (cold TLS,
		// a directory hiccup) still resolves instead of flapping to an
		// inconclusive skip, while halving the per-hang damage.
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: rt,
	}
}

// ssrfGuardedTransport returns a transport whose dials are refused unless
// the destination is a public address (see guardDial).  The resolver's
// fetch destinations are attacker-chosen (a did:web DID is registered with
// only format validation), and in production this code runs on the reaper
// Machine — which holds the Porkbun credentials and sits on the Fly
// private network — so a loopback, private, link-local, or unique-local
// destination must never be connected to.
func ssrfGuardedTransport(tlsCfg *tls.Config) *http.Transport {
	return &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: (&net.Dialer{
			// The dial must fit inside the fetch's overall budget, so it
			// stays in lockstep with the client timeout above.
			Timeout: 5 * time.Second,
			Control: guardDial,
		}).DialContext,
	}
}

// unsafeDialError reports a dial refused by the SSRF guard.  It is a
// distinct type so error classification can tell policy refusals apart
// from ordinary network failures; the reaper treats both identically
// (inconclusive — skip, never reap on it).
type unsafeDialError struct {
	network string
	address string
}

func (e *unsafeDialError) Error() string {
	return fmt.Sprintf("refusing %s dial to non-public address %s", e.network, e.address)
}

// guardDial is the net.Dialer Control hook enforcing isPublicDialTarget.
// It runs after DNS resolution but before the connection is attempted, on
// the address actually being dialed — checking at dial time rather than
// URL-parse time is what defeats DNS rebinding (a hostname that resolves
// public at validation time and private at connect time still lands here
// with its real destination) and what covers the DNS-resolved IP rather
// than the DID's literal text.
func guardDial(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return &unsafeDialError{network: network, address: address}
	}
	if ip := net.ParseIP(host); ip == nil || !isPublicDialTarget(ip) {
		return &unsafeDialError{network: network, address: address}
	}
	return nil
}

// isPublicDialTarget reports whether ip is an address the resolver may
// connect to.  Refused: loopback (127/8, ::1), RFC1918 private (10/8,
// 172.16/12, 192.168/16), unique-local (fc00::/7), link-local
// (169.254/16 incl. cloud metadata endpoints, fe80::/10), unspecified
// (0.0.0.0, ::), multicast, and the IPv4-mapped/IPv4-compatible IPv6
// spellings of any of them — anything not explicitly public is refused.
func isPublicDialTarget(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		// To4 is non-nil for IPv4 and for IPv4-mapped IPv6
		// (::ffff:a.b.c.d), so both spellings are checked here.
		switch {
		case ip4[0] == 0: // 0.0.0.0/8: "this network", incl. unspecified
			return false
		case ip4[0] == 10: // RFC1918
			return false
		case ip4[0] == 127: // loopback
			return false
		case ip4[0] == 169 && ip4[1] == 254: // link-local incl. 169.254.169.254
			return false
		case ip4[0] == 172 && ip4[1]&0xf0 == 16: // 172.16.0.0/12
			return false
		case ip4[0] == 192 && ip4[1] == 168: // RFC1918
			return false
		}
		return true
	}
	ip6 := ip.To16()
	if ip6 == nil {
		return false // not an address form we recognize: refuse
	}
	switch {
	case ip6[0]&0xfe == 0xfc: // unique-local fc00::/7
		return false
	case ip6[0] == 0xfe && ip6[1]&0xc0 == 0x80: // link-local fe80::/10
		return false
	case ip6[0] == 0xff: // multicast ff00::/8
		return false
	case allZero(ip6[:10]) && !(ip6[10] == 0xff && ip6[11] == 0xff):
		// The deprecated IPv4-compatible form ::a.b.c.d is not folded
		// by To4; judge it by its embedded IPv4 so ::127.0.0.1 cannot
		// bypass the v4 policy.  :: and ::1 land here too, as the
		// all-zero/loopback embeds they are.
		return isPublicDialTarget(net.IP(ip6[12:16]))
	}
	return true
}

// allZero reports whether every byte of b is zero.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// resolve implements didResolver.  Unsupported DID methods are an error,
// not "unregistered": the reaper only deletes what it can affirmatively
// show is unused.
func (d *plcDirectory) resolve(ctx context.Context, did string) ([]string, bool, error) {
	switch {
	case strings.HasPrefix(did, "did:plc:"):
		return d.fetchDocument(ctx, d.baseURL+"/"+did, did, true)
	case strings.HasPrefix(did, "did:web:"):
		u, err := webDocumentURL(did)
		if err != nil {
			return nil, false, err
		}
		aka, registered, err := d.fetchDocument(ctx, u, did, false)
		if err != nil && dnsNameNotFound(err) {
			// For did:web the host IS the identity, so the DNS
			// system's authoritative "no such name" answer is the
			// same class of evidence as a directory 404 for
			// did:plc: the DID no longer exists and a record bound
			// to it must not squat its label forever (MEDIUM-5).
			// The classification is deliberately HERE, in the
			// did:web branch only: a DNS failure reaching
			// plc.directory says nothing about a did:plc DID and
			// must stay inconclusive.  Surfacing it as
			// registered=false keeps the reaper consuming a plain
			// verdict, with no HTTP or DNS internals leaking in.
			return nil, false, nil
		}
		return aka, registered, err
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

// dnsNameNotFound reports whether err is the DNS system's authoritative
// "no such name" answer (NXDOMAIN-class) rather than a transient lookup
// failure.  Go sets DNSError.IsNotFound for NXDOMAIN and its cgo
// equivalents (EAI_NONAME/ENOTFOUND), and for nothing else; SERVFAIL,
// timeouts, and network errors carry the temporary/timeout flags instead.
// The errors.As walk crosses the HTTP client's url.Error/OpError wrapping
// down to the underlying DNSError, so the check works on real fetch
// errors as they surface from the client.  A contradictory error carrying
// both the not-found and transient flags is conservatively treated as
// transient: when in doubt, the reaper must skip, never reap.
func dnsNameNotFound(err error) bool {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false
	}
	return dnsErr.IsNotFound && !dnsErr.IsTemporary && !dnsErr.IsTimeout
}

// rateLimitedError reports that plc.directory answered 429.  It is a
// distinct type so the reaper can recognize the directory's "stop
// asking" signal and defer did:plc judging for the rest of the sweep:
// the reaper's directory fetch volume is attacker-steerable (minting
// DIDs is free), and hammering a rate-limiting directory both deepens
// the limit and silently stops reaping (every fetch becomes a skip).
// retryAfter is the directory's requested backoff; 0 means none was
// given, and the reaper applies its own default window.
type rateLimitedError struct {
	retryAfter time.Duration
}

func (e *rateLimitedError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("plc.directory rate limited (Retry-After: %s)", e.retryAfter)
	}
	return "plc.directory rate limited"
}

// parseRetryAfter extracts a Retry-After header's delta-seconds.  Only
// the delta-seconds form is honored: the HTTP-date form would need a
// wall clock inside the resolver, and 0 (absent or unparseable) already
// tells the reaper to fall back to its default backoff window.
func parseRetryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// fetchDocument retrieves and validates the DID document at url, returning
// its alsoKnownAs entries.  A 404 is affirmative: the account is gone.
// Any other non-2xx, a transport failure, or a document whose id does not
// match the requested DID is an inconclusive error — except that resolve's
// did:web branch re-classifies the NXDOMAIN class of transport failure as
// affirmative before handing the verdict to the reaper.
//
// fromDirectory distinguishes the two fetch origins: plc.directory (true)
// and the DID's own host per the did:web spec (false).  Only a 429 from
// plc.directory becomes a *rateLimitedError — the reaper must never back
// off the directory because some third party's did:web host was busy, so
// a did:web 429 stays a plain inconclusive error.
func (d *plcDirectory) fetchDocument(ctx context.Context, url, did string, fromDirectory bool) ([]string, bool, error) {
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
	if resp.StatusCode == http.StatusTooManyRequests && fromDirectory {
		return nil, false, &rateLimitedError{retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
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
