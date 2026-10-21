package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// registerBody builds a JSON request body.  handle and did are interpolated
// verbatim; either may be omitted by not referencing its %s slot.
func registerBody(t *testing.T, handle, did string) string {
	t.Helper()
	b, err := json.Marshal(registerRequest{Handle: handle, DID: did})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(b)
}

func TestHandleRegisterSuccess(t *testing.T) {
	p := &fakeProvisioner{}
	a := newTestApp(t, p)
	w := doRegister(t, a, registerBody(t, "John", validPLC), "")

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeRegister(t, w)
	if resp.Status != "created" {
		t.Fatalf("expected status \"created\", got %q", resp.Status)
	}
	if resp.Handle != "john."+testDomain {
		t.Fatalf("expected handle john.%s, got %q", testDomain, resp.Handle)
	}

	// The provisioner must receive the normalized label, the raw validated
	// DID, and the configured domain — it composes the record from these.
	if len(p.calls) != 1 {
		t.Fatalf("expected 1 provisioner call, got %d", len(p.calls))
	}
	call := p.calls[0]
	if call.domain != testDomain || call.label != "john" || call.did != validPLC {
		t.Fatalf("unexpected provisioner call: %+v", call)
	}
}

func TestHandleRegisterReserved(t *testing.T) {
	p := &fakeProvisioner{}
	a := newTestApp(t, p)
	w := doRegister(t, a, registerBody(t, "www", validPLC), "")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if d := decodeDetail(t, w); !strings.Contains(d, "not available") {
		t.Fatalf("expected unavailability detail, got %q", d)
	}
	if len(p.calls) != 0 {
		t.Fatalf("provisioner must not be called for reserved handle, got %+v", p.calls)
	}
}

func TestHandleRegisterInvalidInputs(t *testing.T) {
	tests := []struct {
		name   string
		handle string
		did    string
	}{
		{"empty handle", "", validPLC},
		{"dot in handle", "john.doe", validPLC},
		{"non-ASCII handle", "café", validPLC},
		{"empty did", "john", ""},
		{"non-DID string", "john", "not-a-did"},
		{"unsupported method", "john", "did:key:z6MkhaXg"},
		{"short plc", "john", "did:plc:short"},
		// Valid label shape, but the composed handle exceeds bsky.app's
		// 30-char custom-domain budget: example.com leaves 18 for the
		// label, and this one is 22.
		{"composed handle too long", strings.Repeat("a", 22), validPLC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvisioner{}
			a := newTestApp(t, p)
			w := doRegister(t, a, registerBody(t, tt.handle, tt.did), "")

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			decodeDetail(t, w)
			if len(p.calls) != 0 {
				t.Fatalf("provisioner must not be called on invalid input, got %+v", p.calls)
			}
		})
	}
}

func TestHandleRegisterContentType(t *testing.T) {
	// missing builds the request by hand because doRegister defaults an
	// empty Content-Type to application/json.
	missingReq := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		a := newTestApp(t, &fakeProvisioner{})
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(registerBody(t, "john", validPLC)))
		w := httptest.NewRecorder()
		a.handleRegister(w, req)
		return w
	}

	tests := []struct {
		name        string
		contentType string
		want        int
	}{
		{"json", "application/json", http.StatusCreated},
		{"json with charset", "application/json; charset=utf-8", http.StatusCreated},
		{"form data", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"plain text", "text/plain", http.StatusUnsupportedMediaType},
		{"missing", "", http.StatusUnsupportedMediaType},
		{"garbage", "not a media type!", http.StatusUnsupportedMediaType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.contentType == "" {
				if w := missingReq(t); w.Code != tt.want {
					t.Fatalf("expected %d, got %d: %s", tt.want, w.Code, w.Body.String())
				}
				return
			}
			a := newTestApp(t, &fakeProvisioner{})
			w := doRegister(t, a, registerBody(t, "john", validPLC), tt.contentType)
			if w.Code != tt.want {
				t.Fatalf("expected %d, got %d: %s", tt.want, w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleRegisterMalformedJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"truncated", `{"handle": "john"`},
		{"not an object", `["john"]`},
		{"trailing garbage", `{"handle":"john","did":"x"} leftover`},
		{"null", `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvisioner{}
			a := newTestApp(t, p)
			w := doRegister(t, a, tt.body, "")

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			decodeDetail(t, w)
			if len(p.calls) != 0 {
				t.Fatalf("provisioner must not be called for malformed body, got %+v", p.calls)
			}
		})
	}
}

func TestHandleRegisterBodyTooLarge(t *testing.T) {
	p := &fakeProvisioner{}
	a := newTestApp(t, p)
	// 8 KiB of padding exceeds the 4 KiB request cap before the JSON is
	// even complete.
	body := `{"handle":"` + strings.Repeat("a", 8<<10) + `"}`
	w := doRegister(t, a, body, "")

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}
	decodeDetail(t, w)
	if len(p.calls) != 0 {
		t.Fatalf("provisioner must not be called for oversized body, got %+v", p.calls)
	}
}

func TestHandleRegisterProvisionerError(t *testing.T) {
	p := &fakeProvisioner{err: errors.New("porkbun is down")}
	a := newTestApp(t, p)
	w := doRegister(t, a, registerBody(t, "john", validPLC), "")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// The backend error must not leak to the client.
	if d := decodeDetail(t, w); strings.Contains(d, "porkbun") {
		t.Fatalf("internal error leaked to client: %q", d)
	}
	if len(p.calls) != 1 {
		t.Fatalf("expected 1 provisioner call, got %d", len(p.calls))
	}
}

// otherDID is a second valid did:plc distinct from validPLC, for exercising
// ownership conflicts.
func otherDID() string {
	return "did:plc:" + strings.Repeat("y", 24)
}

func TestHandleRegisterClaimSemantics(t *testing.T) {
	// The memoryProvisioner applies the same created/registered/conflict
	// semantics as the Porkbun client, so the handler's mapping from
	// outcome to status code is exercised against real logic.
	p := newMemoryProvisioner()
	a := newTestApp(t, p)
	body := registerBody(t, "john", validPLC)

	// First registration publishes the record.
	w := doRegister(t, a, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for fresh registration, got %d: %s", w.Code, w.Body.String())
	}
	if resp := decodeRegister(t, w); resp.Status != "created" {
		t.Fatalf("expected status \"created\", got %q", resp.Status)
	}

	// The same DID re-submitting is an idempotent 200: the record already
	// binds the handle to this account.
	w = doRegister(t, a, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for same-DID repeat, got %d: %s", w.Code, w.Body.String())
	}
	if resp := decodeRegister(t, w); resp.Status != "registered" {
		t.Fatalf("expected status \"registered\", got %q", resp.Status)
	}

	// A different DID is refused: the handle is taken.
	w = doRegister(t, a, registerBody(t, "john", otherDID()), "")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for different DID, got %d: %s", w.Code, w.Body.String())
	}
	if d := decodeDetail(t, w); !strings.Contains(d, "not available") {
		t.Fatalf("expected unavailability detail, got %q", d)
	}
}

func TestHandleRegisterProvisionerConflict(t *testing.T) {
	// A provisioner reporting outcomeConflict must surface as 409 with the
	// shared "not available" wording, and the fake must still have been
	// consulted (the decision is the DNS backend's, not the handler's).
	p := &fakeProvisioner{outcome: outcomeConflict}
	a := newTestApp(t, p)
	w := doRegister(t, a, registerBody(t, "john", validPLC), "")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if d := decodeDetail(t, w); !strings.Contains(d, "not available") {
		t.Fatalf("expected unavailability detail, got %q", d)
	}
	if len(p.calls) != 1 {
		t.Fatalf("expected 1 provisioner call, got %d", len(p.calls))
	}
}

func TestHandleRegisterDIDLimit(t *testing.T) {
	// A provisioner reporting outcomeDIDLimit must surface as 409 with
	// the one-handle-per-domain wording, distinct from the "not available"
	// refusal so the requester knows the label itself is free.
	p := &fakeProvisioner{outcome: outcomeDIDLimit}
	a := newTestApp(t, p)
	w := doRegister(t, a, registerBody(t, "john", validPLC), "")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if d := decodeDetail(t, w); !strings.Contains(d, "already have a handle under "+testDomain) {
		t.Fatalf("expected per-domain limit detail, got %q", d)
	}
	if len(p.calls) != 1 {
		t.Fatalf("expected 1 provisioner call, got %d", len(p.calls))
	}
}

func TestHandleRegisterProvisionerErrorIsRetryable(t *testing.T) {
	// A failed registration must leave nothing behind (DNS-as-truth has no
	// separate claim to roll back): swapping in a working provisioner and
	// retrying the same label must succeed.
	a := newTestApp(t, &fakeProvisioner{err: errors.New("porkbun is down")})
	w := doRegister(t, a, registerBody(t, "john", validPLC), "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	a.provisioner = newMemoryProvisioner()
	w = doRegister(t, a, registerBody(t, "john", otherDID()), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 on retry after failure, got %d: %s", w.Code, w.Body.String())
	}
	if resp := decodeRegister(t, w); resp.Status != "created" {
		t.Fatalf("expected status \"created\" on retry, got %q", resp.Status)
	}
}

func TestResolveDomain(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{}, "wagg.ing", "Yeen.World")

	tests := []struct {
		host string
		want string
		ok   bool
	}{
		{"wagg.ing", "wagg.ing", true},
		{"yeen.world", "yeen.world", true},
		{"WAGG.ING", "wagg.ing", true},       // DNS names are case-insensitive
		{"wagg.ing:8443", "wagg.ing", true},  // Host header may carry a port
		{"wagg.ing.", "wagg.ing", true},      // FQDN trailing dot
		{" wagg.ing ", "wagg.ing", true},     // padded
		{"handles.wagg.ing", "", false},      // subdomain, not a managed domain
		{"evil.example", "", false},          // not ours
		{"", "", false},                      // no Host at all
		{"wagg.ing.evil.example", "", false}, // suffix trick
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got, ok := a.resolveDomain(tt.host)
			if ok != tt.ok {
				t.Fatalf("resolveDomain(%q) ok = %v (got %q), want ok = %v", tt.host, ok, got, tt.ok)
			}
			if ok && got != tt.want {
				t.Fatalf("resolveDomain(%q) = %q, want %q", tt.host, got, tt.want)
			}
			if !ok && got != "" {
				t.Fatalf("resolveDomain(%q) miss must return empty domain, got %q", tt.host, got)
			}
		})
	}
}

func TestHandleRegisterHostSelection(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		wantHandle string
	}{
		{"first managed domain", "wagg.ing", "john.wagg.ing"},
		{"second managed domain", "yeen.world", "john.yeen.world"},
		{"case-insensitive host", "Wagg.Ing", "john.wagg.ing"},
		{"host with port", "wagg.ing:8443", "john.wagg.ing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvisioner{}
			a := newTestApp(t, p, "wagg.ing", "yeen.world")
			w := doRegisterAt(t, a, tt.host, registerBody(t, "john", validPLC), "")

			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
			}
			resp := decodeRegister(t, w)
			if resp.Handle != tt.wantHandle {
				t.Fatalf("expected handle %q, got %q", tt.wantHandle, resp.Handle)
			}
			if len(p.calls) != 1 {
				t.Fatalf("expected 1 provisioner call, got %d", len(p.calls))
			}
			// The provisioner must receive the Host-resolved domain, so
			// the Porkbun client operates on the matching zone.
			if p.calls[0].domain != tt.host && p.calls[0].domain != strings.ToLower(strings.Split(tt.host, ":")[0]) {
				t.Fatalf("provisioner called with domain %q, expected %q", p.calls[0].domain, tt.host)
			}
			if p.calls[0].label != "john" || p.calls[0].did != validPLC {
				t.Fatalf("unexpected provisioner call: %+v", p.calls[0])
			}
		})
	}
}

func TestHandleRegisterUnknownHost(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"foreign domain", "evil.example"},
		{"subdomain of managed domain", "handles.wagg.ing"},
		{"empty host", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvisioner{}
			a := newTestApp(t, p, "wagg.ing", "yeen.world")
			w := doRegisterAt(t, a, tt.host, registerBody(t, "john", validPLC), "")

			if w.Code != http.StatusMisdirectedRequest {
				t.Fatalf("expected 421, got %d: %s", w.Code, w.Body.String())
			}
			decodeDetail(t, w)
			if len(p.calls) != 0 {
				t.Fatalf("provisioner must not be called for unknown host, got %+v", p.calls)
			}
		})
	}
}

func TestHandleRegisterComposedLengthPerDomain(t *testing.T) {
	// A 20-char label composes to 29 chars under wagg.ing (within bsky.app's
	// 30-char budget) but to 32 under example.com (over it).  The same
	// requested name must be accepted or rejected depending on the Host's
	// domain, since the domain eats into the same 30 characters.
	label := strings.Repeat("a", 20)
	body := registerBody(t, label, validPLC)

	waggP := &fakeProvisioner{}
	waggApp := newTestApp(t, waggP, "wagg.ing")
	if w := doRegisterAt(t, waggApp, "wagg.ing", body, ""); w.Code != http.StatusCreated {
		t.Fatalf("expected 201 under wagg.ing, got %d: %s", w.Code, w.Body.String())
	}
	if len(waggP.calls) != 1 {
		t.Fatalf("expected provisioning under wagg.ing, got %+v", waggP.calls)
	}

	exampleP := &fakeProvisioner{}
	exampleApp := newTestApp(t, exampleP) // manages example.com
	w := doRegisterAt(t, exampleApp, "example.com", body, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 under example.com, got %d: %s", w.Code, w.Body.String())
	}
	if len(exampleP.calls) != 0 {
		t.Fatalf("provisioner must not be called when the composed handle is over budget, got %+v", exampleP.calls)
	}
}

func TestRegisterRouting(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{})

	tests := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"POST /register", http.MethodPost, "/register", http.StatusCreated},
		{"GET /register is 405", http.MethodGet, "/register", http.StatusMethodNotAllowed},
		{"PUT /register is 405", http.MethodPut, "/register", http.StatusMethodNotAllowed},
		{"POST /other is 404", http.MethodPost, "/other", http.StatusNotFound},
		{"GET / is the form page", http.MethodGet, "/", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doRegisterMux(t, a, tt.method, tt.target, registerBody(t, "john", validPLC))
			if w.Code != tt.want {
				t.Fatalf("expected %d, got %d: %s", tt.want, w.Code, w.Body.String())
			}
		})
	}
}
