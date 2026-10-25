package main

import (
	"fmt"
	"strings"
	"testing"
)

// A syntactically valid did:plc: 8-char prefix + 24 chars of [a-z2-7].
const validPLC = "did:plc:abcdefghijklmnopqrstuvwx"

func TestNormalizeHandle(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// --- valid, canonicalized to lowercase without whitespace ---
		{"simple", "john", "john", false},
		{"three characters (username minimum)", "abc", "abc", false},
		{"uppercase normalized", "John", "john", false},
		{"mixed case and padding", "  MiXeD-CaSe  ", "mixed-case", false},
		{"digits", "42a", "42a", false},
		{"internal hyphen", "john-doe", "john-doe", false},
		{"max length (63)", strings.Repeat("a", 63), strings.Repeat("a", 63), false},
		{"punycode label", "xn--bcher-kva", "xn--bcher-kva", false},
		{"trailing digit after hyphen run", "a--b", "a--b", false},
		{"all digits at username minimum", "123", "123", false},

		// --- invalid format ---
		{"empty", "", "", true},
		{"whitespace only", "   ", "", true},
		{"single character (below username minimum)", "a", "", true},
		{"two characters (below username minimum)", "ab", "", true},
		{"leading hyphen", "-john", "", true},
		{"trailing hyphen", "john-", "", true},
		{"too long (64)", strings.Repeat("a", 64), "", true},
		{"underscore", "john_doe", "", true},
		{"leading underscore", "_atproto", "", true},
		{"dot escape attempt", "john.doe", "", true},
		{"dot to reach apex", "example.com", "", true},
		{"dot at end", "john.", "", true},
		{"slash injection", "john/x", "", true},
		{"space inside", "jo hn", "", true},
		{"non-ASCII", "café", "", true},
		{"emoji", "john🐈", "", true},
		{"newline inside", "jo\nhn", "", true},
		{"nul byte", "jo\x00hn", "", true},

		// --- reserved labels ---
		{"reserved www", "www", "", true},
		{"reserved _atproto", "_atproto", "", true},
		{"reserved case-insensitive", "WWW", "", true},
		{"reserved admin", "admin", "", true},
		{"reserved bsky", "bsky", "", true},
		{"non-reserved survives check", "wwww", "wwww", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeHandle(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeHandle(%q) = %q, expected error", tt.in, got)
				}
				if got != "" {
					t.Fatalf("normalizeHandle(%q) error case returned non-empty handle %q", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeHandle(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeHandle(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseDomain(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// --- valid, canonicalized ---
		{"simple", "wagg.ing", "wagg.ing", false},
		{"uppercase normalized", "WAGG.ING", "wagg.ing", false},
		{"padded", "  vore.rip  ", "vore.rip", false},
		{"fqdn trailing dot", "wagg.ing.", "wagg.ing", false},
		{"multi-label", "handles.wagg.ing", "handles.wagg.ing", false},
		{"hyphenated label", "my-site.example.com", "my-site.example.com", false},

		// --- AT Proto handle constraints on the TLD ---
		{"TLD starts with digit", "foo.123", "", true},
		{"TLD disallowed (.local)", "foo.local", "", true},
		{"TLD disallowed (.onion)", "foo.onion", "", true},
		{"TLD disallowed (.example)", "foo.example", "", true},
		{"TLD disallowed case differs (.invalid)", "foo.INVALID", "", true}, // normalized to lowercase first

		// --- invalid ---
		{"empty", "", "", true},
		{"whitespace only", "   ", "", true},
		{"dot only", ".", "", true},
		{"single label", "localhost", "", true},
		{"empty label inside", "wagg..ing", "", true},
		{"leading dot", ".wagg.ing", "", true},
		{"leading hyphen label", "-wagg.ing", "", true},
		{"trailing hyphen label", "wagg.ing-", "", true},
		{"underscore", "wagg_ing.example.com", "", true},
		{"space inside", "wagg ing.example.com", "", true},
		{"non-ASCII", "wäg.ing", "", true},
		{"too long", strings.Repeat("a", 250) + ".com", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDomain(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDomain(%q) = %q, expected error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDomain(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseDomain(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEnsureHandleClaimable(t *testing.T) {
	// wagg.ing is 8 chars, so the label budget is 30 - 8 - 1 = 21.
	const domain = "wagg.ing"
	const max = 21

	tests := []struct {
		name    string
		label   string
		wantErr bool
	}{
		{"typical handle", "john", false},
		{"label at exact budget", strings.Repeat("a", max), false},
		{"label one over budget", strings.Repeat("a", max+1), true},
		{"label at DNS-label max", strings.Repeat("a", 63), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ensureHandleClaimable(tt.label, domain)
			if tt.wantErr && err == nil {
				t.Fatalf("ensureHandleClaimable(%q, %q) succeeded, expected error", tt.label, domain)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ensureHandleClaimable(%q, %q) unexpected error: %v", tt.label, domain, err)
			}
			// The over-budget error must name the per-domain maximum
			// so the frontend can surface it directly.
			if tt.wantErr && !strings.Contains(err.Error(), fmt.Sprintf("%d", max)) {
				t.Fatalf("expected error to name max %d, got: %v", max, err)
			}
		})
	}

	// The exact-30-char composed handle must be accepted: the budget is
	// inclusive.
	composed := "aaaaaaaaaaaaaaaaaaaaa" + "." + domain // 21 + 1 + 8 = 30
	if len(composed) != maxAppHandleLength {
		t.Fatalf("test fixture: composed handle is %d chars, want %d", len(composed), maxAppHandleLength)
	}
	if err := ensureHandleClaimable("aaaaaaaaaaaaaaaaaaaaa", domain); err != nil {
		t.Fatalf("composed handle of exactly %d chars rejected: %v", maxAppHandleLength, err)
	}
}

func TestValidateDID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// --- valid ---
		{"plc", validPLC, validPLC, false},
		{"plc digits 2-7", "did:plc:2345672abcdefghij2345672", "did:plc:2345672abcdefghij2345672", false},
		{"plc padded with whitespace", " " + validPLC + "\t", validPLC, false},
		{"web host", "did:web:example.com", "did:web:example.com", false},
		{"web host with port", "did:web:example.com:8443", "did:web:example.com:8443", false},
		{"web percent-encoded port", "did:web:example.com%3A8443", "did:web:example.com%3A8443", false},
		{"web with path", "did:web:example.com:users:john", "did:web:example.com:users:john", false},
		{"web with path slashes", "did:web:example.com:user%3Ajohn", "did:web:example.com:user%3Ajohn", false},

		// --- malformed ---
		{"empty", "", "", true},
		{"whitespace only", "  ", "", true},
		{"not a did", "john.example.com", "", true},
		{"missing method", "did::abcdef", "", true},
		{"unsupported method", "did:key:z6MkhaXg", "", true},
		{"uppercase method", "did:PLC:abcdefghijklmnopqrstuvwx", "", true},
		{"uppercase plc body", "did:plc:ABCDEFGHIJKLMNOPQRSTUVWX", "", true},

		// --- plc structure ---
		{"plc too short", "did:plc:abcdefghijklmnopqrstuvw", "", true},
		{"plc too long", "did:plc:abcdefghijklmnopqrstuvwxy", "", true},
		{"plc invalid chars", "did:plc:abcdefghijklmnopqrstuv01", "", true}, // 0/1 not in base32
		{"plc empty body", "did:plc:", "", true},

		// --- injection attempts: anything that could break out of the
		// "did=<value>" TXT string must be rejected ---
		{"txt quote injection", "did:plc:abcdefghijklmnopqrstuv\" did=did:plc:evil", "", true},
		{"txt space injection", "did:web:example.com did=did:plc:evil", "", true},
		{"newline injection", "did:web:example.com\ndid=did:plc:evil", "", true},
		{"tab injection", "did:web:example.com\tdid=did:plc:evil", "", true},
		{"carriage return injection", "did:web:example.com\r\nx", "", true},
		{"backslash injection", "did:web:example.com\\", "", true},

		// --- length ---
		{"web too long", "did:web:" + strings.Repeat("a", 200), "", true},
		{"web at limit", "did:web:" + strings.Repeat("a", 191), "did:web:" + strings.Repeat("a", 191), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateDID(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateDID(%q) = %q, expected error", tt.in, got)
				}
				if got != "" {
					t.Fatalf("validateDID(%q) error case returned non-empty did %q", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateDID(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("validateDID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
