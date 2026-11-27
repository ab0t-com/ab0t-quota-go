package authevents

// v0.3.1 — the keys that keep the sign-up gift once per person, per company, whatever the rule
// (SignupGiftOnceKeys). Vectors: testdata/signup_gift_key_golden_20261127.json, a byte-identical copy
// of the Python library's (signup_gift_keys). RED before v0.3.1: SignupGiftOnceKeys does not exist.
// The end-to-end scenarios from the same file: signup_gift_once_per_person_20261127_test.go.

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type signupGiftGolden struct {
	Keys []struct {
		Name         string         `json:"name"`
		Policy       string         `json:"policy"`
		UserID       string         `json:"user_id"`
		OrgID        string         `json:"org_id"`
		TierID       string         `json:"tier_id"`
		CompanyOrgID string         `json:"company_org_id"`
		Want         SignupGiftKeys `json:"want"`
	} `json:"keys"`
}

func loadSignupGiftGolden(t *testing.T) signupGiftGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/signup_gift_key_golden_20261127.json")
	if err != nil {
		t.Fatal(err)
	}
	var g signupGiftGolden
	if err := json.Unmarshal(raw, &g); err != nil || len(g.Keys) == 0 {
		t.Fatalf("golden: %v", err)
	}
	return g
}

func TestSignupGiftOnceKeys_MatchTheSharedVectors_20261127(t *testing.T) {
	for _, c := range loadSignupGiftGolden(t).Keys {
		t.Run(c.Name, func(t *testing.T) {
			got := SignupGiftOnceKeys(c.Policy, c.UserID, c.OrgID, c.TierID, c.CompanyOrgID)
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("keys\n got %+v\nwant %+v", got, c.Want)
			}
		})
	}
}

func TestSignupGiftOnceKeys_OnceKeyIsTheSameUnderEveryRuleAndPlan_20261127(t *testing.T) {
	want := MemberJoinedGrantKey("org_acme", "u_leo")
	for _, rule := range []string{"per_user_per_tier", "per_org_per_tier", "per_user_global", "per_org_global"} {
		for _, tier := range []string{"free", "pro"} {
			if got := SignupGiftOnceKeys(rule, "u_leo", "org_ws_leo_acme", tier, "org_acme").OnceKey; got != want {
				t.Fatalf("%s/%s: once key %q, want %q", rule, tier, got, want)
			}
		}
	}
}

// Billing receives exactly what v0.3.0 sent (billing dedups per org on it).
func TestSignupGiftOnceKeys_BillingKeyIsTheRulesKeyAsBefore_20261127(t *testing.T) {
	want := map[string]string{
		"per_user_per_tier": "user:u:initial_credit:t", "per_org_per_tier": "org:o:initial_credit:t",
		"per_user_global": "user:u:initial_credit", "per_org_global": "org:o:initial_credit",
		"something_else": "user:u:initial_credit:t",
	}
	for rule, key := range want {
		if got := SignupGiftOnceKeys(rule, "u", "o", "t", "").BillingIdempotencyKey; got != key || BillingIdempotencyKey(rule, "u", "o", "t") != key {
			t.Errorf("%s: billing key %q, want %q", rule, got, key)
		}
	}
}

// CONTROL: a per-ORG rule's marker names an org, not a person — after a switch to a per-person rule, a
// new person in that org is NOT treated as already gifted.
func TestSignupGiftOnceKeys_PerOrgMarkerDoesNotBlockANewPerson_20261127(t *testing.T) {
	keys := SignupGiftOnceKeys("per_user_global", "u_new", "org_shared", "free", "").AlreadyGiftedKeys
	for _, k := range keys {
		if strings.Contains(k, "org:org_shared") {
			t.Fatalf("a per-org key %q would deny the gift to every new person of the org", k)
		}
	}
}
