package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testDomain = "example.com"

// fakeCall records one invocation of Provision.
type fakeCall struct {
	domain string
	label  string
	did    string
}

// fakeProvisioner is the in-memory provisioner used by handler tests.  It
// records every call, and can be armed with an error to simulate a DNS
// backend failure.
type fakeProvisioner struct {
	calls []fakeCall
	err   error
}

func (f *fakeProvisioner) Provision(ctx context.Context, domain, label, did string) error {
	_ = ctx
	f.calls = append(f.calls, fakeCall{domain: domain, label: label, did: did})
	return f.err
}

// newTestApp creates an app managing the given domains (parsed with the same
// startup validation as main.go's -domain flag).  With no domains, the app
// manages just testDomain.
func newTestApp(t *testing.T, p provisioner, domains ...string) *app {
	t.Helper()
	if len(domains) == 0 {
		domains = []string{testDomain}
	}
	managed := make(map[string]bool, len(domains))
	for _, raw := range domains {
		d, err := parseDomain(raw)
		if err != nil {
			t.Fatalf("test domain %q invalid: %v", raw, err)
		}
		managed[d] = true
	}
	return &app{domains: managed, provisioner: p}
}

// doRegister sends a POST /register request to the given Host with the given
// body and Content-Type, invoking the handler directly (no mux), and
// returns the recorder.  contentType defaults to application/json when
// empty; host defaults to testDomain.
func doRegister(t *testing.T, a *app, body, contentType string) *httptest.ResponseRecorder {
	return doRegisterAt(t, a, testDomain, body, contentType)
}

// doRegisterAt is doRegister with an explicit Host header, to exercise the
// per-domain routing.
func doRegisterAt(t *testing.T, a *app, host, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Host = host
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	a.handleRegister(w, req)
	return w
}

// doRegisterMux sends a request through a mux wired exactly like main.go's,
// to exercise method routing (405/404) rather than just the handler.
func doRegisterMux(t *testing.T, a *app, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", a.handleRegister)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = testDomain
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// decodeDetail unmarshals the {"detail": ...} error body and returns the
// detail string, failing the test if the body is not a valid error response.
func decodeDetail(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error response: %v (body: %s)", err, w.Body.String())
	}
	if resp.Detail == "" {
		t.Fatalf("error response has empty detail (body: %s)", w.Body.String())
	}
	return resp.Detail
}

// decodeRegister unmarshals a success body into registerResponse.
func decodeRegister(t *testing.T, w *httptest.ResponseRecorder) registerResponse {
	t.Helper()
	var resp registerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal register response: %v (body: %s)", err, w.Body.String())
	}
	return resp
}
