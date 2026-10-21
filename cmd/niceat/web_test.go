package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServeIndexPage checks that the mux serves the embedded frontend page
// at "/" with HTML content type.  The body assertion uses text that exists
// in the built page (the Svelte app's own heading), so a broken or empty
// build artifact fails the test loudly.
func TestServeIndexPage(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{})
	w := doRegisterMux(t, a, http.MethodGet, "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	if !strings.Contains(w.Body.String(), "Claim your handle") {
		t.Errorf("page body does not contain the app markup (len %d)", w.Body.Len())
	}
}

// TestServeIndexMethodRouting: the page is registered as GET-only, so other
// methods get the mux's 405, and paths other than exactly "/" get a 404 —
// the server deliberately exposes nothing else.
func TestServeIndexMethodRouting(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{})
	for _, tt := range []struct {
		method, target string
		want           int
	}{
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/form", http.StatusNotFound},
		{http.MethodGet, "/register", http.StatusMethodNotAllowed}, // GET on a POST-only route
	} {
		w := doRegisterMux(t, a, tt.method, tt.target, "")
		if w.Code != tt.want {
			t.Errorf("%s %s status = %d, want %d", tt.method, tt.target, w.Code, tt.want)
		}
	}
}

// TestServeIndexUnmanagedHost: the form page is gated on the Host like
// /register — an unmanaged host gets 421 instead of a form whose every
// submission would fail.
func TestServeIndexUnmanagedHost(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{})
	mux := a.newMux(newRateLimiter())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "unmanaged.example.com"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("GET / unmanaged host status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
	if got := decodeDetail(t, w); got == "" {
		t.Fatal("expected non-empty error detail")
	}
}

// TestRegisterRateLimitDoesNotThrottleForm pins the routing decision that the
// per-IP rate limiter wraps only /register: a visitor whose registration
// budget is exhausted must still be able to load the form page.
func TestRegisterRateLimitDoesNotThrottleForm(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{outcome: outcomeCreated})
	mux := a.newMux(newRateLimiter())

	// Drain the burst against /register.  The bodies are invalid JSON, but
	// the limiter runs before the handler, so every request still spends a
	// token; the last one must come back as 429.
	for i := 0; i <= registerBurst; i++ {
		w := doMux(t, mux, http.MethodPost, "/register", "{}")
		want := http.StatusBadRequest
		if i == registerBurst {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("register request %d status = %d, want %d", i, w.Code, want)
		}
	}

	// The form page shares no budget with the API.
	w := doMux(t, mux, http.MethodGet, "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / after exhausting /register status = %d, want %d", w.Code, http.StatusOK)
	}
}
