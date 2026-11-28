package config

// v0.3.2 — one billing_model name set for both libraries (testdata/billing_models_golden_20261128.json,
// byte-identical to the Python library's tests/ copy): the Python library's names load here, the shared
// aliases print the shared warning, a Python-only experimental name keeps booting with a warning until
// the next minor, and a credit_grant whose trigger no runtime fires is named.
// RED before v0.3.2: capacity_only / seat_based / metered load with "unknown billing_model"; the alias
// and never-fires texts (BillingModelAliasWarning, ScheduledGrantNeverFiresWarning) do not exist.
// Run: go test ./config -run NeverAstray -count=1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type modelsGolden struct {
	ExperimentalEnv string `json:"experimental_env"`
	Default         string `json:"default"`
	Canonical       []struct {
		Name         string `json:"name"`
		Experimental bool   `json:"experimental"`
		ShortMeaning string `json:"short_meaning"`
	} `json:"canonical"`
	Aliases []struct {
		Name      string `json:"name"`
		Canonical string `json:"canonical"`
	} `json:"aliases"`
	AliasTemplate string `json:"alias_warning_template"`
	GoGrace       struct {
		Names    []string `json:"names"`
		Template string   `json:"warning_template"`
	} `json:"go_grace"`
	NeverFiresTemplate string `json:"never_fires_warning_template"`
	Cases              []struct {
		Name       string          `json:"name"`
		OptIn      bool            `json:"opt_in"`
		Tier       json.RawMessage `json:"tier"`
		Canonical  string          `json:"canonical"`
		Go         string          `json:"go"`
		WarningsGo []string        `json:"warnings_go"`
	} `json:"cases"`
}

func loadModelsGolden(t *testing.T) modelsGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/billing_models_golden_20261128.json")
	if err != nil {
		t.Fatal(err)
	}
	var g modelsGolden
	if err := json.Unmarshal(raw, &g); err != nil || len(g.Cases) == 0 {
		t.Fatalf("golden: %v", err)
	}
	return g
}

func sharedModelWarnings(w []string) []string {
	var out []string
	for _, s := range w {
		if strings.Contains(s, "is another name for") || strings.Contains(s, "will too from its next minor release") ||
			strings.Contains(s, `"scheduled_period_start" is accepted but neither library fires it`) {
			out = append(out, s)
		}
	}
	return out
}

func TestNeverAstray_CanonicalSetIsKnownHere_20261128(t *testing.T) {
	g := loadModelsGolden(t)
	for _, c := range g.Canonical {
		m := BillingModel(c.Name)
		if !m.IsKnown() || m.Canonical() != m {
			t.Errorf("canonical %q: known=%v canonical=%q", c.Name, m.IsKnown(), m.Canonical())
		}
		if billingModelShortMeaning[m] != c.ShortMeaning {
			t.Errorf("%q meaning %q, golden %q", c.Name, billingModelShortMeaning[m], c.ShortMeaning)
		}
	}
	if len(BillingModelAliases) != len(g.Aliases) {
		t.Fatalf("aliases %v, golden %v", BillingModelAliases, g.Aliases)
	}
	for _, a := range g.Aliases {
		if got := BillingModel(a.Name).Canonical(); string(got) != a.Canonical {
			t.Errorf("alias %q → %q, golden %q", a.Name, got, a.Canonical)
		}
	}
	if BillingModel("free").IsKnown() { // CONTROL: a name that means nothing is not known
		t.Fatal("\"free\" must not be a known billing model")
	}
}

func TestNeverAstray_BillingModelCasesMatchTheSharedVectors_20261128(t *testing.T) {
	g := loadModelsGolden(t)
	for _, c := range g.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.OptIn {
				t.Setenv(g.ExperimentalEnv, "true")
			} else {
				t.Setenv(g.ExperimentalEnv, "")
			}
			doc := []byte(`{"storage": {"redis_url": "memory://"}, "enforcement": {"enabled": true}, "tiers": [` + string(c.Tier) + `]}`)
			p := filepath.Join(t.TempDir(), "quota-config.json")
			if err := os.WriteFile(p, doc, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(p)
			if c.Go == "refused" {
				if err == nil {
					t.Fatal("loaded; the shared vector says refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("refused (%v); the shared vector says ok", err)
			}
			got := cfg.Tiers[0].BillingModel.Canonical()
			if got == "" {
				got = BillingModel(g.Default)
			}
			if string(got) != c.Canonical {
				t.Fatalf("canonical %q, want %q", got, c.Canonical)
			}
			var want []string
			var tier struct {
				TierID       string `json:"tier_id"`
				BillingModel string `json:"billing_model"`
			}
			_ = json.Unmarshal(c.Tier, &tier)
			for _, tok := range c.WarningsGo {
				r := strings.NewReplacer("{tier_id}", tier.TierID, "{name}", tier.BillingModel,
					"{canonical}", c.Canonical, "{short_meaning}", billingModelShortMeaning[BillingModel(c.Canonical)])
				switch tok {
				case "alias":
					want = append(want, r.Replace(g.AliasTemplate))
				case "go_grace":
					want = append(want, r.Replace(g.GoGrace.Template))
				case "never_fires":
					want = append(want, r.Replace(g.NeverFiresTemplate))
				}
			}
			if got := sharedModelWarnings(cfg.Warnings()); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("warnings\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// CONTROL: the texts the code prints are the golden's templates (not just "a warning").
func TestNeverAstray_TextsAreTheSharedOnes_20261128(t *testing.T) {
	g := loadModelsGolden(t)
	want := strings.NewReplacer("{tier_id}", "free", "{name}", "free_tier", "{canonical}", "capacity_only",
		"{short_meaning}", "limits only: the tier itself moves no money").Replace(g.AliasTemplate)
	if got := BillingModelAliasWarning("free", BillingModelFreeTier); got != want {
		t.Fatalf("alias text\n go: %s\npy: %s", got, want)
	}
	if got := ScheduledGrantNeverFiresWarning("free"); got != strings.ReplaceAll(g.NeverFiresTemplate, "{tier_id}", "free") {
		t.Fatalf("never-fires text: %s", got)
	}
	if got := BillingModelGoGraceWarning("team", BillingModelSeatBased); got != strings.NewReplacer(
		"{tier_id}", "team", "{name}", "seat_based").Replace(g.GoGrace.Template) {
		t.Fatalf("grace text: %s", got)
	}
	for _, n := range g.GoGrace.Names {
		if !BillingModel(n).pythonOnlyExperimental() {
			t.Errorf("%q should be in the Go grace set", n)
		}
	}
}
