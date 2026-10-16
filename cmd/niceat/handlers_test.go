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
	a := newTestApp(p)
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
	a := newTestApp(p)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvisioner{}
			a := newTestApp(p)
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
		a := newTestApp(&fakeProvisioner{})
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
			a := newTestApp(&fakeProvisioner{})
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
			a := newTestApp(p)
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
	a := newTestApp(p)
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
	a := newTestApp(p)
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

func TestRegisterRouting(t *testing.T) {
	a := newTestApp(&fakeProvisioner{})

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
		{"GET / is 404", http.MethodGet, "/", http.StatusNotFound},
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
