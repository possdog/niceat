package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// defaultConfigPath is where loadConfig looks when -config is not given.
const defaultConfigPath = "niceat.json"

// defaultListenAddr is used when the config file omits "addr".
const defaultListenAddr = ":8080"

// config is the on-disk deployment configuration, e.g.:
//
//	{
//	  "domains": ["wagg.ing", "yeen.world"],
//	  "addr": ":8080"
//	}
//
// JSON is used because it is the one format Go's standard library parses
// natively; a persistent service should not need CLI flags to remember its
// setup between restarts.
type config struct {
	// Domains lists the apex domains this instance manages.  Required and
	// non-empty; entries are normalized (lowercase, FQDN dot stripped) and
	// de-duplicated by loadConfig.
	Domains []string `json:"domains"`
	// Addr is the HTTP listen address.  Optional; defaults to
	// defaultListenAddr when omitted or empty.
	Addr string `json:"addr"`
}

// loadConfig reads, parses, and validates the config file at path.
//
// Decoding is strict: unknown top-level fields are rejected so a typo like
// "domain" instead of "domains" fails loudly at startup instead of silently
// producing an empty domain set.  Domain entries are validated with
// parseDomain (so a malformed entry also fails at startup, with the
// offending value named), normalized, and de-duplicated; Addr falls back to
// the default when absent or empty.
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if len(cfg.Domains) == 0 {
		return nil, fmt.Errorf("%s: \"domains\" must list at least one domain", path)
	}

	seen := make(map[string]bool, len(cfg.Domains))
	domains := make([]string, 0, len(cfg.Domains))
	for _, raw := range cfg.Domains {
		d, err := parseDomain(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid domain %q: %v", path, raw, err)
		}
		if seen[d] {
			continue // duplicate after normalization; keep one copy
		}
		seen[d] = true
		domains = append(domains, d)
	}
	cfg.Domains = domains

	if cfg.Addr == "" {
		cfg.Addr = defaultListenAddr
	}
	return &cfg, nil
}

// managedSet returns the config's normalized domains as the lookup set the
// app uses for Host resolution.
func (c *config) managedSet() map[string]bool {
	set := make(map[string]bool, len(c.Domains))
	for _, d := range c.Domains {
		set[d] = true
	}
	return set
}
