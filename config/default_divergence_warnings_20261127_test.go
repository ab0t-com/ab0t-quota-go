package config

// v0.3.0 — the ONE breaking default change of this minor: a config FILE that omits enforcement.enabled
// now runs with it TRUE, and a credit_grant that omits reset_on_downgrade runs with it TRUE (Python
// parity; both were Go's zero value, false, before v0.3.0). Warnings names each omitted key with the text
// the Python library prints too (testdata/config_default_warnings_golden_20261127.json, byte-identical to
// its tests/ copy). A Config built in Go code keeps exactly the values it was given.
// RED before v0.3.0: the message constants do not exist; a file omitting both keys loaded false/false.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type divergenceGolden struct {
	EnforcementEnabledUnset string `json:"enforcement_enabled_unset"`
	ResetTemplate           string `json:"reset_on_downgrade_unset_template"`
	Cases                   []struct {
		Name      string          `json:"name"`
		Config    json.RawMessage `json:"config"`
		Warnings  []string        `json:"warnings"`
		Effective struct {
			EnforcementEnabled bool            `json:"enforcement_enabled"`
			ResetOnDowngrade   map[string]bool `json:"reset_on_downgrade"`
		} `json:"effective"`
	} `json:"cases"`
}

func loadDivergenceGolden(t *testing.T) divergenceGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/config_default_warnings_golden_20261127.json")
	if err != nil {
		t.Fatal(err)
	}
	var g divergenceGolden
	if err := json.Unmarshal(raw, &g); err != nil || len(g.Cases) == 0 {
		t.Fatalf("golden: %v", err)
	}
	return g
}

func loadDoc(t *testing.T, doc json.RawMessage) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "quota-config.json")
	if err := os.WriteFile(p, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func divergenceOnly(w []string) []string {
	var out []string
	for _, s := range w {
		if s == EnforcementEnabledUnsetWarning || strings.Contains(s, "credit_grant.reset_on_downgrade is not set") {
			out = append(out, s)
		}
	}
	return out
}

func TestDefaultDivergence_TextsAreTheSharedOnes_20261127(t *testing.T) {
	g := loadDivergenceGolden(t)
	if EnforcementEnabledUnsetWarning != g.EnforcementEnabledUnset {
		t.Fatalf("enforcement text differs from the Python library's:\n go: %s\npy: %s", EnforcementEnabledUnsetWarning, g.EnforcementEnabledUnset)
	}
	if got, want := ResetOnDowngradeUnsetWarning("pro"), strings.ReplaceAll(g.ResetTemplate, "{tier_id}", "pro"); got != want {
		t.Fatalf("reset_on_downgrade text differs:\n go: %s\npy: %s", got, want)
	}
}

func TestDefaultDivergence_WarningsMatchTheSharedVectors_20261127(t *testing.T) {
	g := loadDivergenceGolden(t)
	for _, c := range g.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var want []string
			for _, tok := range c.Warnings {
				if tok == "enforcement" {
					want = append(want, g.EnforcementEnabledUnset)
				} else {
					want = append(want, strings.ReplaceAll(g.ResetTemplate, "{tier_id}", strings.TrimPrefix(tok, "tier:")))
				}
			}
			got := divergenceOnly(loadDoc(t, c.Config).Warnings())
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("warnings\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// The values a config FILE runs with — the same as the Python library's for every vector.
func TestDefaultDivergence_FileDefaultsAreTrueTrue_20261127(t *testing.T) {
	g := loadDivergenceGolden(t)
	for _, c := range g.Cases {
		t.Run(c.Name, func(t *testing.T) {
			cfg := loadDoc(t, c.Config)
			if cfg.Enforcement.Enabled != c.Effective.EnforcementEnabled {
				t.Fatalf("enforcement.enabled = %v, want %v", cfg.Enforcement.Enabled, c.Effective.EnforcementEnabled)
			}
			for _, tier := range cfg.Tiers {
				want, declared := c.Effective.ResetOnDowngrade[tier.TierID]
				if tier.CreditGrant == nil {
					if declared {
						t.Fatalf("tier %s: no credit_grant loaded", tier.TierID)
					}
					continue
				}
				if tier.CreditGrant.ResetOnDowngrade != want {
					t.Fatalf("tier %s: reset_on_downgrade = %v, want %v", tier.TierID, tier.CreditGrant.ResetOnDowngrade, want)
				}
			}
		})
	}
}

// CONTROL: a Config built in Go code keeps its explicit (zero) values — only a FILE gets the new defaults.
func TestDefaultDivergence_GoLiteralsKeepTheirValues_20261127(t *testing.T) {
	c := &Config{Tiers: []Tier{{TierID: "free", CreditGrant: &CreditGrant{Trigger: CreditTriggerSignup}}}}
	if c.Enforcement.Enabled || c.Tiers[0].CreditGrant.ResetOnDowngrade {
		t.Fatal("a Go literal must keep false/false")
	}
	if got := divergenceOnly(c.Warnings()); len(got) != 0 {
		t.Fatalf("a Go literal must not get the omitted-key warnings: %q", got)
	}
}

// An omitted key is reported once, with the shared text — not also as the explicit-false message.
func TestDefaultDivergence_OmittedKeyIsReportedOnce_20261127(t *testing.T) {
	g := loadDivergenceGolden(t)
	for _, w := range loadDoc(t, g.Cases[0].Config).Warnings() {
		if strings.HasPrefix(w, "enforcement.enabled is false") {
			t.Fatalf("omitted key also reported as explicit false: %s", w)
		}
	}
}

// The explicit-false message says what an omitted key now means.
func TestDefaultDivergence_ExplicitFalseExplainsTheNewDefault_20261127(t *testing.T) {
	c := &Config{Enforcement: EnforcementConfig{Enabled: false}}
	if got := strings.Join(c.Warnings(), "\n"); !strings.Contains(got, "Since v0.3.0 a config file that omits the key defaults to true") {
		t.Fatalf("explicit-false warning must explain the v0.3.0 default: %s", got)
	}
}
