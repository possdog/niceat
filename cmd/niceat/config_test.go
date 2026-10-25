package main

import (
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
