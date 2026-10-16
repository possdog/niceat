package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
//	201 — handle registered
//	200 — idempotent repeat: the handle is already bound to this DID
//	400 — invalid handle or DID (including reserved names), malformed JSON
//	405 — non-POST method (enforced by the mux's method-specific pattern)
//	409 — handle already claimed by a different DID
//	413 — request body exceeds maxRequestBody
//	415 — Content-Type is not application/json
//	421 — Host header does not match a domain this server manages
//	429 — caller exceeded its per-IP rate limit (middleware)
//	500 — DNS provisioning or claims-store failure
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
	// The domain-specific half of the union: the composed handle must fit
	// bsky.app's custom-domain budget.
	if err := ensureHandleClaimable(label, domain); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Detail: err.Error()})
		return
	}
	did, err := validateDID(req.DID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Detail: err.Error()})
		return
	}

	// --- 4. Claim the label (put-if-absent) ---
	// The claim is acquired *before* provisioning so two concurrent
	// requests cannot both win the same label, and so an existing handle
	// can never be silently re-bound to a different account.
	created, existingDID, err := a.claims.tryClaim(domain, label, did)
	if err != nil {
		log.Printf("ERROR: claiming %s.%s failed: %v", label, domain, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Detail: "Internal server error"})
		return
	}
	if !created && existingDID != did {
		// Deliberately the same wording as the reserved-name error: a
		// prober learns only "not available", never whether a handle
		// is taken by a person or blocked by policy.
		writeJSON(w, http.StatusConflict, errorResponse{Detail: fmt.Sprintf("handle %q is not available", label)})
		return
	}

	// --- 5. Provision the DNS record ---
	// This also runs on the idempotent same-DID repeat: Porkbun treats an
	// identical re-create as success, so a claim left dangling by a crash
	// between claim and provision is healed by its owner re-submitting.
	if err := a.provisioner.Provision(r.Context(), domain, label, did); err != nil {
		// A newly created claim must be rolled back so the failure does
		// not squat the label; a pre-existing claim belongs to its
		// owner and stays.
		if created {
			if rerr := a.claims.release(domain, label); rerr != nil {
				log.Printf("ERROR: releasing claim on %s.%s after provisioning failure: %v", label, domain, rerr)
			}
		}
		log.Printf("ERROR: provisioning _atproto.%s.%s failed: %v", label, domain, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Detail: "Internal server error"})
		return
	}

	// --- 6. Success ---
	// 201 for a fresh registration, 200 for an idempotent repeat by the
	// same DID (the handle was already bound to this account).
	status := "created"
	code := http.StatusCreated
	if !created {
		status = "registered"
		code = http.StatusOK
	}
	writeJSON(w, code, registerResponse{
		Status: status,
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
