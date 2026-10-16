package config

// Ticket 20261016 finding 5: examples/basic/main.go told users to run with
// AB0T_QUOTA_CONFIG_PATH, which nothing reads. The variable is QUOTA_CONFIG_PATH
// (Python parity); the wrong name must be loud, never silently ignored. RED on
// v0.1.9 (no warning).

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_WarnsOnIgnoredAB0TConfigPath_20261016(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "q.json")
	if err := os.WriteFile(p, []byte(`{"tiers":[{"tier_id":"free","limits":{}}],"resources":[],"storage":{"redis_url":"memory://"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	t.Setenv("QUOTA_CONFIG_PATH", p)
	t.Setenv("AB0T_QUOTA_CONFIG_PATH", filepath.Join(dir, "other.json"))
	if _, err := LoadConfig(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "AB0T_QUOTA_CONFIG_PATH is set but IGNORED") {
		t.Fatalf("no warning for the ignored variable; log:\n%s", buf.String())
	}
}
