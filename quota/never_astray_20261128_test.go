package quota

// v0.3.2 — "never lead a company astray": the new contract surface (capability fields, shared 403
// constants, MoneyChecklist). Behaviour pins that compile against v0.3.1 are in
// never_astray_behaviour_20261128_test.go. Shared vectors with the Python library:
// testdata/money_safety_golden_20261128.json (byte-identical to its tests/ copy).
//
// Pinned:
//   - Capabilities().CreditGrant is OFF (with the cause) when no gift can arrive, and names the events;
//   - the price-list 403 policy and strings are the Python library's;
//   - MoneyChecklist (doctor --money) grades the shared cases exactly; unknown is never "good";
//   - in-process, the checklist sees the service's own paid mode and its handler ledger.
//
// RED before v0.3.2: CreditGrantEvents / CreditGrantSubscription / PriceList403* / MoneyChecklist do not
// exist (build failure).
// Run: go test ./quota -run NeverAstray -count=1

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
)

func TestNeverAstray_CapabilitiesNameTheGiftsEvents_20261128(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	clearMoneyEnv(t)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "s")
	q, err := Setup(context.Background(), Options{ConfigOverride: giftCfg(), SkipBackgroundLoops: true,
		CreditGranter: granterFunc(func(context.Context, authevents.CreditGrantRequest) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	c := q.Capabilities()
	if !c.CreditGrant || strings.Join(c.CreditGrantEvents, ",") != "auth.user.registered" ||
		!strings.HasPrefix(c.CreditGrantSubscription, "not verified") {
		t.Fatalf("credit_grant=%v events=%v subscription=%q", c.CreditGrant, c.CreditGrantEvents, c.CreditGrantSubscription)
	}
}

func TestNeverAstray_NoSecretMeansNoGiftCanArrive_20261128(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	clearMoneyEnv(t)
	cfg := giftCfg()
	cfg.AuthEvents.GrantOnMemberJoined = true
	q, err := Setup(context.Background(), Options{ConfigOverride: cfg, SkipBackgroundLoops: true,
		CreditGranter: granterFunc(func(context.Context, authevents.CreditGrantRequest) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	c := q.Capabilities()
	if c.CreditGrant || !strings.Contains(c.WhyOff["credit_grant"], "AB0T_AUTH_WEBHOOK_SECRET not set") {
		t.Fatalf("credit_grant=%v why=%q — a receiver that rejects every event is not a working gift", c.CreditGrant, c.WhyOff["credit_grant"])
	}
	if strings.Join(c.CreditGrantEvents, ",") != "auth.user.registered,org.member.added" {
		t.Fatalf("events %v", c.CreditGrantEvents)
	}
}

type safety403 struct {
	P struct {
		Interval   int    `json:"interval_seconds"`
		MaxRetries int    `json:"max_retries"`
		Retrying   string `json:"retrying_state"`
		GaveUp     string `json:"gave_up_state"`
	} `json:"price_list_403"`
}

func TestNeverAstray_403PolicyIsTheSharedOne_20261128(t *testing.T) {
	raw, err := os.ReadFile("../testdata/money_safety_golden_20261128.json")
	if err != nil {
		t.Fatal(err)
	}
	var g safety403
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if PriceList403RetryInterval != time.Duration(g.P.Interval)*time.Second || PriceList403MaxRetries != g.P.MaxRetries ||
		PriceList403RetryingState != g.P.Retrying || PriceList403GaveUpState != g.P.GaveUp {
		t.Fatalf("403 policy differs from the Python library's:\n%v %d\n%s\n%s", PriceList403RetryInterval,
			PriceList403MaxRetries, PriceList403RetryingState, PriceList403GaveUpState)
	}
}

type moneyGolden struct {
	MoneyDoctor struct {
		Grades []string `json:"grades"`
		Checks []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"checks"`
		Cases []struct {
			Name   string            `json:"name"`
			Config map[string]any    `json:"config"`
			Facts  MoneyFacts        `json:"facts"`
			Grades map[string]string `json:"grades"`
		} `json:"cases"`
	} `json:"money_doctor"`
}

func TestNeverAstray_MoneyChecklistGradesTheSharedCases_20261128(t *testing.T) {
	raw, err := os.ReadFile("../testdata/money_safety_golden_20261128.json")
	if err != nil {
		t.Fatal(err)
	}
	var g moneyGolden
	if err := json.Unmarshal(raw, &g); err != nil || len(g.MoneyDoctor.Cases) == 0 {
		t.Fatalf("golden: %v", err)
	}
	if len(g.MoneyDoctor.Checks) != len(MoneyChecks) || strings.Join(g.MoneyDoctor.Grades, ",") != strings.Join(MoneyGrades, ",") {
		t.Fatalf("checks/grades differ from the Python library's")
	}
	for i, c := range g.MoneyDoctor.Checks {
		if MoneyChecks[i] != [2]string{c.ID, c.Name} {
			t.Fatalf("check %d = %v, golden %s/%s", i, MoneyChecks[i], c.ID, c.Name)
		}
	}
	for _, c := range g.MoneyDoctor.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := map[string]string{}
			for _, f := range MoneyChecklist(c.Config, c.Facts) {
				got[f.ID] = f.Grade
			}
			for id, want := range c.Grades {
				if got[id] != want {
					t.Errorf("%s = %q, want %q", id, got[id], want)
				}
			}
			if len(got) != len(c.Grades) {
				t.Errorf("checks %v", got)
			}
		})
	}
}

// CONTROL: with nothing known, no check but M5 (pure config) may read "good".
func TestNeverAstray_NothingKnownIsNeverGood_20261128(t *testing.T) {
	doc := ConfigDocument(giftCfg())
	if len(docTiers(doc)) != 1 {
		t.Fatalf("ConfigDocument lost the tiers: %v", doc)
	}
	for _, f := range MoneyChecklist(doc, MoneyFacts{}) {
		if f.Grade == "good" && f.ID != "M5" {
			t.Fatalf("%s graded good with no facts: %s", f.ID, f.Detail)
		}
	}
}

// In-process, the service sees its own paid mode and its ledger.
func TestNeverAstray_InProcessFactsAreSeen_20261128(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	clearMoneyEnv(t)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "s")
	q, err := Setup(context.Background(), Options{ConfigOverride: giftCfg(), SkipBackgroundLoops: true,
		CreditGranter: granterFunc(func(context.Context, authevents.CreditGrantRequest) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	ev := []byte(`{"event_type":"auth.user.registered","event_id":"evt_seen","data":{"user_id":"u1","org_id":"o1"}}`)
	_ = postSigned(t, q.WebhookHandler(), authevents.WebhookPath, ev, "s")
	f := q.MoneyFactsInProcess(context.Background(), MoneyFacts{})
	if !f.PaidCodeVisible || f.WebhookSecretSet == nil || !*f.WebhookSecretSet || f.GiftEventsSeen == nil || *f.GiftEventsSeen < 1 {
		t.Fatalf("facts %+v", f)
	}
}
