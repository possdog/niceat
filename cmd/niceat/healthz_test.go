package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestHealthzOK: GET /healthz answers 200 with {"status":"ok"} through the
// real mux — the shape Fly's [http_service.checks] liveness probe expects —
// and reports which provisioner is active.  With the credential-less
// fallback running, the body says "provisioner":"memory": a deploy that
// lost its Porkbun secret serves fake successes, so this is the value
// operators alert on (the probe itself only asserts the 200, so the extra
// field cannot break deploys).
func TestHealthzOK(t *testing.T) {
	a := newTestApp(t, newMemoryProvisioner())
	w := doRegisterMux(t, a, http.MethodGet, "/healthz", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp healthzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal healthz response: %v (body: %s)", err, w.Body.String())
	}
	if resp.Status != "ok" {
		t.Errorf("healthz status = %q, want %q", resp.Status, "ok")
	}
	if resp.Provisioner != "memory" {
		t.Errorf("healthz provisioner = %q, want %q", resp.Provisioner, "memory")
	}
}

// TestHealthzReportsPorkbun: with the real Porkbun client wired (as main.go
// does when credentials load), /healthz reports "provisioner":"porkbun".
// The healthz handler never calls Provision, so the client needs no server
// behind it.
func TestHealthzReportsPorkbun(t *testing.T) {
	a := newTestApp(t, newPorkbunProvisioner(map[string]porkbunCreds{
		testDomain: {apiKey: "testkey", secretKey: "testsecret"},
	}, ""))
	w := doRegisterMux(t, a, http.MethodGet, "/healthz", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp healthzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal healthz response: %v (body: %s)", err, w.Body.String())
	}
	if resp.Status != "ok" {
		t.Errorf("healthz status = %q, want %q", resp.Status, "ok")
	}
	if resp.Provisioner != "porkbun" {
		t.Errorf("healthz provisioner = %q, want %q", resp.Provisioner, "porkbun")
	}
}

// TestHealthzMethodRouting: /healthz is GET-only (other methods get the
// mux's 405), and only the exact path matches (/healthz/ or /healthzX are
// 404) — the liveness probe must not be reachable on lookalike paths.
func TestHealthzMethodRouting(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{})
	for _, tt := range []struct {
		method, target string
		want           int
	}{
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/healthz/", http.StatusNotFound},
		{http.MethodGet, "/healthzX", http.StatusNotFound},
	} {
		w := doRegisterMux(t, a, tt.method, tt.target, "")
		if w.Code != tt.want {
			t.Errorf("%s %s status = %d, want %d", tt.method, tt.target, w.Code, tt.want)
		}
	}
}
