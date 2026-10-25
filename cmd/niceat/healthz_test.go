package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestHealthzOK: GET /healthz answers 200 with {"status":"ok"} through the
// real mux — the shape Fly's [http_service.checks] liveness probe expects.
func TestHealthzOK(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{})
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
