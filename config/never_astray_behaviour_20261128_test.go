package config

// v0.3.2 — behaviour pins that use only the pre-v0.3.2 API (they compile against v0.3.1 and go RED there
// by assertion): the Python library's capacity_only is known (no "unknown billing_model"), a Go alias
// is named with its canonical name, and a scheduled_period_start grant is said to never fire.
// Run: go test ./config -run NeverAstrayBehaviour -count=1

import (
	"strings"
	"testing"
)

func TestNeverAstrayBehaviour_ModelNamesAndANeverFiringGrant_20261128(t *testing.T) {
	t.Setenv("AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS", "")
	cfg := loadDoc(t, []byte(`{"storage": {"redis_url": "memory://"}, "enforcement": {"enabled": true}, "tiers": [
	  {"tier_id": "a", "display_name": "A", "billing_model": "capacity_only"},
	  {"tier_id": "b", "display_name": "B", "billing_model": "free_tier"},
	  {"tier_id": "c", "display_name": "C",
	   "credit_grant": {"trigger": "scheduled_period_start", "amount_per_period": "2", "lifecycle": "use_it_or_lose_it",
	                    "destination": "credit_balance", "reset_on_downgrade": false}}]}`))
	w := strings.Join(cfg.Warnings(), "\n")
	if strings.Contains(w, `unknown billing_model "capacity_only"`) {
		t.Errorf("capacity_only is the Python library's default name; it must be known here:\n%s", w)
	}
	if !strings.Contains(w, `billing_model "free_tier" is another name for "capacity_only"`) {
		t.Errorf("the alias must be named with its canonical name:\n%s", w)
	}
	if !strings.Contains(w, `tier "c": credit_grant.trigger "scheduled_period_start" is accepted but neither library fires it`) {
		t.Errorf("a grant nothing fires must be said at startup:\n%s", w)
	}
	// CONTROL: a name that means nothing is still "unknown".
	bad := loadDoc(t, []byte(`{"storage": {"redis_url": "memory://"}, "enforcement": {"enabled": true},
	  "tiers": [{"tier_id": "x", "display_name": "X", "billing_model": "free"}]}`))
	if !strings.Contains(strings.Join(bad.Warnings(), "\n"), `unknown billing_model "free"`) {
		t.Errorf("an unknown name must stay unknown")
	}
}
