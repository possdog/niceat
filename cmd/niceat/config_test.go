package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes content to a temp config file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "niceat.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantDomains []string
		wantAddr    string
		wantErr     string // non-empty => error must contain this substring
	}{
		{
			name:        "happy path",
			content:     `{"domains": ["wagg.ing", "vore.rip"], "addr": ":9000"}`,
			wantDomains: []string{"wagg.ing", "vore.rip"},
			wantAddr:    ":9000",
		},
		{
			// Case and FQDN dots are normalized; duplicates collapse.
			name:        "normalized and deduplicated",
			content:     `{"domains": ["WAGG.ING.", "wagg.ing"], "addr": ":9000"}`,
			wantDomains: []string{"wagg.ing"},
			wantAddr:    ":9000",
		},
		{
			name:        "addr defaults when omitted",
			content:     `{"domains": ["wagg.ing"]}`,
			wantAddr:    defaultListenAddr,
			wantDomains: []string{"wagg.ing"},
		},
		{
			name:        "addr defaults when empty",
			content:     `{"domains": ["wagg.ing"], "addr": ""}`,
			wantAddr:    defaultListenAddr,
			wantDomains: []string{"wagg.ing"},
		},
		{
			name:    "missing file",
			content: "",
			wantErr: "read config",
		},
		{
			name:    "malformed JSON",
			content: `{"domains": [`,
			wantErr: "parse",
		},
		{
			name:    "empty file",
			content: "",
			wantErr: "parse",
		},
		{
			// Strict decoding: a typo like "domain" must fail loudly
			// rather than silently producing an empty domain set.
			name:    "unknown field",
			content: `{"domain": ["wagg.ing"]}`,
			wantErr: "unknown field",
		},
		{
			name:    "domains empty array",
			content: `{"domains": []}`,
			wantErr: "at least one domain",
		},
		{
			name:    "domains missing",
			content: `{"addr": ":9000"}`,
			wantErr: "at least one domain",
		},
		{
			name:    "invalid domain entry",
			content: `{"domains": ["wagg.ing", "wagg ing"]}`,
			wantErr: `invalid domain "wagg ing"`,
		},
		{
			name:    "null body",
			content: `null`,
			wantErr: "at least one domain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.content
			if tt.name != "missing file" {
				path = writeConfig(t, tt.content)
			} else {
				path = filepath.Join(t.TempDir(), "does-not-exist.json")
			}

			cfg, err := loadConfig(path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("loadConfig succeeded, expected error containing %q (cfg: %+v)", tt.wantErr, cfg)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig unexpected error: %v", err)
			}
			if strings.Join(cfg.Domains, ",") != strings.Join(tt.wantDomains, ",") {
				t.Fatalf("domains = %v, want %v", cfg.Domains, tt.wantDomains)
			}
			if cfg.Addr != tt.wantAddr {
				t.Fatalf("addr = %q, want %q", cfg.Addr, tt.wantAddr)
			}
		})
	}
}

func TestLoadConfigTrustedProxies(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []netip.Prefix // nil => no trust, the safe default
		wantErr string
	}{
		{
			// Absent means no trust: the peer address is always the
			// client IP (LOW-6).
			name:    "absent: no proxy trusted",
			content: `{"domains": ["wagg.ing"]}`,
			want:    nil,
		},
		{
			name:    "empty array: no proxy trusted",
			content: `{"domains": ["wagg.ing"], "trusted_proxies": []}`,
			want:    nil,
		},
		{
			name:    "single loopback prefix",
			content: `{"domains": ["wagg.ing"], "trusted_proxies": ["127.0.0.1/32"]}`,
			want:    []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		},
		{
			name:    "ipv6 prefix",
			content: `{"domains": ["wagg.ing"], "trusted_proxies": ["::1/128"]}`,
			want:    []netip.Prefix{netip.MustParsePrefix("::1/128")},
		},
		{
			// Host bits are masked away, so spellings of the same
			// network collapse into one prefix — mirroring how
			// domains are normalized and de-duplicated.
			name:    "masked spellings of one network deduplicate",
			content: `{"domains": ["wagg.ing"], "trusted_proxies": ["10.0.0.5/24", "10.0.0.0/24", "10.0.0.9/24"]}`,
			want:    []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		},
		{
			// A name is not a prefix: trust must be expressed as
			// CIDR so an operator states exactly which addresses
			// may identify the client.
			name:    "non-CIDR entry is a startup error",
			content: `{"domains": ["wagg.ing"], "trusted_proxies": ["localhost"]}`,
			wantErr: `invalid trusted_proxies entry "localhost"`,
		},
		{
			name:    "bare IP is not a CIDR prefix",
			content: `{"domains": ["wagg.ing"], "trusted_proxies": ["127.0.0.1"]}`,
			wantErr: `invalid trusted_proxies entry "127.0.0.1"`,
		},
		{
			name:    "invalid mask bits",
			content: `{"domains": ["wagg.ing"], "trusted_proxies": ["10.0.0.0/99"]}`,
			wantErr: `invalid trusted_proxies entry "10.0.0.0/99"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(writeConfig(t, tt.content))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if len(cfg.trustedProxies) != len(tt.want) {
				t.Fatalf("trustedProxies = %v, want %v", cfg.trustedProxies, tt.want)
			}
			for i := range tt.want {
				if cfg.trustedProxies[i] != tt.want[i] {
					t.Fatalf("trustedProxies[%d] = %v, want %v", i, cfg.trustedProxies[i], tt.want[i])
				}
			}
		})
	}
}

func TestManagedSet(t *testing.T) {
	cfg := &config{Domains: []string{"wagg.ing", "vore.rip"}}
	set := cfg.managedSet()
	if len(set) != 2 || !set["wagg.ing"] || !set["vore.rip"] {
		t.Fatalf("managedSet = %v, want {wagg.ing, vore.rip}", set)
	}
}

func TestLoadConfigReaper(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		wantEnabled  bool
		wantInterval time.Duration
		wantGrace    time.Duration
		wantErr      string
	}{
		{
			name:         "section absent: defaults (enabled, 6h, 30d)",
			content:      `{"domains": ["wagg.ing"]}`,
			wantEnabled:  true,
			wantInterval: defaultReaperInterval,
			wantGrace:    defaultReaperGrace,
		},
		{
			name:         "explicit settings",
			content:      `{"domains": ["wagg.ing"], "reaper": {"enabled": true, "interval": "1h", "grace": "48h"}}`,
			wantEnabled:  true,
			wantInterval: time.Hour,
			wantGrace:    48 * time.Hour,
		},
		{
			name:         "disabled",
			content:      `{"domains": ["wagg.ing"], "reaper": {"enabled": false}}`,
			wantEnabled:  false,
			wantInterval: defaultReaperInterval,
			wantGrace:    defaultReaperGrace,
		},
		{
			name:    "bad interval",
			content: `{"domains": ["wagg.ing"], "reaper": {"interval": "tomorrow"}}`,
			wantErr: "reaper.interval",
		},
		{
			name:    "negative grace",
			content: `{"domains": ["wagg.ing"], "reaper": {"grace": "-1h"}}`,
			wantErr: "reaper.grace",
		},
		{
			// Strict decoding applies inside the section too.
			name:    "unknown field in section",
			content: `{"domains": ["wagg.ing"], "reaper": {"intervalle": "1h"}}`,
			wantErr: "unknown field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(writeConfig(t, tt.content))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.reaperEnabled != tt.wantEnabled {
				t.Errorf("reaperEnabled = %v, want %v", cfg.reaperEnabled, tt.wantEnabled)
			}
			if cfg.reaperInterval != tt.wantInterval {
				t.Errorf("reaperInterval = %v, want %v", cfg.reaperInterval, tt.wantInterval)
			}
			if cfg.reaperGrace != tt.wantGrace {
				t.Errorf("reaperGrace = %v, want %v", cfg.reaperGrace, tt.wantGrace)
			}
		})
	}
}
