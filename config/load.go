package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SearchPaths is consulted in order when ConfigPath is empty.
// Mirrors Python's order; built-in defaults removed (review rec).
var SearchPaths = []string{
	"./quota-config.json",
	"/etc/ab0t/quota-config.json",
}

// LoadConfig reads, interpolates, parses, and validates the config.
// If path is empty, $QUOTA_CONFIG_PATH and SearchPaths are tried.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("QUOTA_CONFIG_PATH")
	}
	if path == "" {
		for _, p := range SearchPaths {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return nil, fmt.Errorf("config: no config file found (set QUOTA_CONFIG_PATH or place quota-config.json in CWD)")
	}
	abs, _ := filepath.Abs(path)
	slog.Info("loading quota config", "path", abs)

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	expanded := interpolateEnv(string(raw))

	// Two-pass parse: first into a map to capture extras + drop $-keys,
	// then into the typed struct.
	var loose map[string]json.RawMessage
	if err := json.Unmarshal([]byte(expanded), &loose); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	for k := range loose {
		if strings.HasPrefix(k, "$") {
			delete(loose, k)
		}
	}

	// Rebuild canonical JSON without `$` keys + decode into typed.
	canonical, _ := json.Marshal(loose)
	var cfg Config
	if err := json.Unmarshal(canonical, &cfg); err != nil {
		return nil, fmt.Errorf("config: %s decode: %w", path, err)
	}

	// Record unknown top-level keys.
	known := knownTopLevelKeys()
	cfg.Extra = map[string]json.RawMessage{}
	for k, v := range loose {
		if !known[k] {
			cfg.Extra[k] = v
			slog.Debug("config: unknown top-level key (preserved for forward-compat)", "key", k)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// MustLoadConfig wraps LoadConfig and panics on error. Convenience for
// program init where there's nothing useful to do but die.
func MustLoadConfig(path string) *Config {
	c, err := LoadConfig(path)
	if err != nil {
		panic(err)
	}
	return c
}

// envRefRE matches ${VAR} and ${VAR:-default}.
var envRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// interpolateEnv expands ${VAR} / ${VAR:-default} references.
// Only QUOTA_-prefixed vars are honored; others get the default and a warning.
func interpolateEnv(s string) string {
	return envRefRE.ReplaceAllStringFunc(s, func(match string) string {
		groups := envRefRE.FindStringSubmatch(match)
		varname, defaultVal := groups[1], groups[2]
		if !strings.HasPrefix(varname, "QUOTA_") {
			slog.Warn("config: ignoring non-QUOTA_-prefixed env ref; using default",
				"var", varname)
			return defaultVal
		}
		if v, ok := os.LookupEnv(varname); ok && v != "" {
			return v
		}
		return defaultVal
	})
}

// knownTopLevelKeys returns the set of recognized top-level keys.
// Anything else lands in Extra for forward-compat.
func knownTopLevelKeys() map[string]bool {
	return map[string]bool{
		"$schema": true, "service_name": true, "engine_mode": true,
		"tiers": true, "resources": true, "resource_bundles": true,
		"tier_provider": true, "storage": true, "alerts": true,
		"enforcement": true, "billing_integration": true, "reconciliation": true,
		"pricing": true, "bridge_cache": true,
	}
}
