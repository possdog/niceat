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
	if !strings.Contains(w.Body.String(), "Get yourself a") {
		t.Errorf("page body does not contain the app markup (len %d)", w.Body.Len())
	}
}

// TestServeIndexHardeningHeaders pins the security headers of the served
// page (audit INFO-8).  The CSP is asserted exactly so that a future
// frontend change that adds a subresource (a font, an image, an external
// script) fails loudly here until the policy is widened on purpose —
// every directive is derived from the current single-file page's needs
// (see the comment in web.go).
func TestServeIndexHardeningHeaders(t *testing.T) {
	const wantCSP = "default-src 'none'; base-uri 'none'; form-action 'self'; " +
		"frame-ancestors 'none'; object-src 'none'; " +
		"script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'"

	a := newTestApp(t, &fakeProvisioner{})
	w := doRegisterMux(t, a, http.MethodGet, "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", w.Code, http.StatusOK)
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != wantCSP {
		t.Errorf("Content-Security-Policy = %q, want %q", csp, wantCSP)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", xcto, "nosniff")
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

// TestRegisterAbsoluteFormRejected reproduces the audit's INFO-9 probe:
// "POST http://<managed-domain>/register HTTP/1.1" alongside a mismatched
// Host header.  Go's server derives r.Host from an absolute-form target and
// ignores the Host header entirely (net/http ReadRequest: "any Host line is
// ignored"), so before the rejectAbsoluteForm middleware this request was
// attributed to the target's host and returned 201.  It must now be refused
// outright — before domain resolution, so it must never reach the
// provisioner.
func TestRegisterAbsoluteFormRejected(t *testing.T) {
	fp := &fakeProvisioner{outcome: outcomeCreated}
	a := newTestApp(t, fp)
	w := doRegisterMux(t, a, http.MethodPost, "http://"+testDomain+"/register",
		`{"handle":"john","did":"`+validPLC+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("absolute-form POST /register status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if got := decodeDetail(t, w); got == "" {
		t.Fatal("expected non-empty error detail")
	}
	if len(fp.calls) != 0 {
		t.Errorf("absolute-form request reached the provisioner: %+v", fp.calls)
	}
}

// TestServeIndexAbsoluteFormRejected: the form page route rejects
// absolute-form targets with the same 400, so no route disagrees with
// /register about request-target form.
func TestServeIndexAbsoluteFormRejected(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{})
	w := doRegisterMux(t, a, http.MethodGet, "https://"+testDomain+"/", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("absolute-form GET / status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if got := decodeDetail(t, w); got == "" {
		t.Fatal("expected non-empty error detail")
	}
}

// TestRegisterOriginFormHostGating pins that rejecting absolute-form
// targets changed nothing about ordinary origin-form requests: domain
// selection still comes from the Host header alone — a managed Host
// registers (201), an unmanaged Host still gets 421.
func TestRegisterOriginFormHostGating(t *testing.T) {
	a := newTestApp(t, &fakeProvisioner{outcome: outcomeCreated})
	mux := a.newMux(newRateLimiter())

	// Managed host: a plain origin-form POST still registers.
	body := `{"handle":"john","did":"` + validPLC + `"}`
	w := doMux(t, mux, http.MethodPost, "/register", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("origin-form POST /register managed host status = %d, want %d", w.Code, http.StatusCreated)
	}

	// Unmanaged host: same request is still refused with 421.
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Host = "unmanaged.example.com"
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("origin-form POST /register unmanaged host status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
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
