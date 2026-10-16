package main

import (
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
)

// handleRegister handles POST /register.
//
// Request:  JSON body {"handle": "<label>", "did": "did:plc:..."} where
// handle is the desired subdomain label under the served domain.
// Response: 201 {"status": "created", "handle": "<label>.<domain>"}
//
// Status codes:
//
//	201 — record provisioned (the handle is live once DNS propagates)
//	400 — invalid handle or DID, malformed JSON
//	405 — non-POST method (enforced by the mux's method-specific pattern)
//	413 — request body exceeds maxRequestBody
//	415 — Content-Type is not application/json
//	421 — Host header does not match a domain this server manages
//	500 — DNS provisioning failed
//
// Error bodies use the shared {"detail": "..."} shape; detail strings for 4xx
// are written for display to the end user, while 5xx details stay generic so
// internal errors are not leaked.
func (a *app) handleRegister(w http.ResponseWriter, r *http.Request) {
	// --- 1. Resolve the target domain ---
	// The Host header — not the request body — decides which managed
	// domain the handle is registered under.  The frontend is served
	// from the same origin it posts to, so a browser's Host is the
	// domain the user is actually browsing.  A spoofed Host can only
	// select among domains we already manage, never reach outside them,
	// because resolveDomain only ever returns configured names.
	domain, ok := a.resolveDomain(r.Host)
	if !ok {
		writeJSON(w, http.StatusMisdirectedRequest, errorResponse{Detail: "This host is not available for handle registration"})
		return
	}
	// --- 1. Content-Type gate ---
	// Registration is a JSON API; rejecting other types up front keeps
	// form-encoded or multipart bodies from being silently misparsed.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{Detail: "Content-Type must be application/json"})
		return
	}

	// --- 2. Bound and decode the body ---
	// MaxBytesReader caps how much a client can make us buffer; the
	// resulting *http.MaxBytesError maps to 413 rather than a generic 400.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Detail: "Request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Detail: "Request body must be valid JSON"})
		return
	}

	// --- 3. Validate and normalize inputs ---
	label, err := normalizeHandle(req.Handle)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Detail: err.Error()})
		return
	}
	did, err := validateDID(req.DID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Detail: err.Error()})
		return
	}

	// --- 4. Provision the DNS record ---
	if err := a.provisioner.Provision(r.Context(), domain, label, did); err != nil {
		// Log the error server-side; the client gets a generic message.
		log.Printf("ERROR: provisioning _atproto.%s.%s failed: %v", label, domain, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Detail: "Internal server error"})
		return
	}

	// --- 5. Success ---
	writeJSON(w, http.StatusCreated, registerResponse{
		Status: "created",
		Handle: label + "." + domain,
	})
}

// resolveDomain maps a request's Host header to a domain this instance
// manages.  The Host may carry a port ("wagg.ing:8443") or an FQDN trailing
// dot ("wagg.ing."); both are normalized away, as is case (DNS names are
// case-insensitive).  An empty or unknown host resolves to nothing.
func (a *app) resolveDomain(hostPort string) (string, bool) {
	host := strings.ToLower(strings.TrimSpace(hostPort))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if a.domains[host] {
		return host, true
	}
	return "", false
}

// writeJSON sets the JSON content type, writes the status code, and encodes v
// as the response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("ERROR: writing response: %v", err)
	}
}
