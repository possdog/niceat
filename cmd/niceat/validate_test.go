package main

import (
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
		{"uppercase normalized", "John", "john", false},
		{"mixed case and padding", "  MiXeD-CaSe  ", "mixed-case", false},
		{"single character", "a", "a", false},
		{"digits", "42", "42", false},
		{"internal hyphen", "john-doe", "john-doe", false},
		{"max length (63)", strings.Repeat("a", 63), strings.Repeat("a", 63), false},
		{"punycode label", "xn--bcher-kva", "xn--bcher-kva", false},
		{"trailing digit after hyphen run", "a--b", "a--b", false},

		// --- invalid format ---
		{"empty", "", "", true},
		{"whitespace only", "   ", "", true},
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
