package quota

// v0.3.2 — `quotactl doctor --money`: the money-safety checklist.
//
// Why: a company can follow the docs, start cleanly, and still lose money without noticing — a sign-up
// gift subscribed to events nobody sends, paid mode switched off, a price list billing refuses, paying
// customers resolved to the free tier. These eight checks ask the questions that catch those, and
// answer each honestly: what was checked is graded; what could not be checked is "not_checked" with
// the reason — never "good" for something unseen.
//
// Same ids, names and grading rules as the Python library (ab0t_quota/money_doctor.py); the shared
// cases are testdata/money_safety_golden_20261128.json. MoneyChecklist is pure. A service can run it
// in-process (q.MoneyChecklist), where the capabilities and the handler ledger are visible; the CLI
// sees only the config, the environment and — with --live — read-only answers from auth and billing.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// MoneyChecks are the checks, in order: id and name.
var MoneyChecks = [][2]string{
	{"M1", "paid_mode"},
	{"M2", "price_list"},
	{"M3", "gift_events"},
	{"M4", "tier_resolution"},
	{"M5", "billing_model_for_runtime"},
	{"M6", "no_dev_escapes"},
	{"M7", "negative_balance"},
	{"M8", "failed_permanent_alert"},
}

// MoneyGrades are the grades a check can get.
var MoneyGrades = []string{"good", "info", "attention", "risk", "not_checked"}

var moneyGradeRank = map[string]int{"good": 0, "info": 1, "not_checked": 2, "attention": 3, "risk": 4}

// TierLookupFact is one tier lookup a doctor run made.
type TierLookupFact struct {
	Org        string `json:"org"`
	Outcome    string `json:"outcome"` // ok | forbidden | unreachable | error
	Tier       string `json:"tier"`
	ExpectTier string `json:"expect_tier"`
	Detail     string `json:"detail,omitempty"`
}

// MoneyFacts are what a doctor run gathered. A nil pointer / nil slice / "" means not checked.
type MoneyFacts struct {
	Runtime               string          `json:"runtime"`
	Environment           string          `json:"environment"`
	Paid                  string          `json:"paid"` // on | off | ""
	PaidDetail            string          `json:"paid_detail"`
	PaidCodeVisible       bool            `json:"paid_code_visible"` // true in-process: the code's EnablePaid is known
	PriceList             string          `json:"price_list"`        // Capabilities().PriceList
	WebhookSecretSet      *bool           `json:"webhook_secret_set"`
	SubscribedEvents      []string        `json:"subscribed_events"` // nil = not checked; [] = no subscription
	GiftEventsSeen        *int            `json:"gift_events_seen"`
	TierLookup            *TierLookupFact `json:"tier_lookup"`
	FailedPermanentGrants *int            `json:"failed_permanent_grants"`
}

// MoneyFinding is one graded check.
type MoneyFinding struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Grade  string `json:"grade"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// ConfigDocument returns cfg as the JSON document MoneyChecklist reads.
func ConfigDocument(cfg *config.Config) map[string]any {
	b, err := json.Marshal(cfg)
	if err != nil {
		return map[string]any{}
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil || doc == nil {
		return map[string]any{}
	}
	return doc
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func docTiers(doc map[string]any) []map[string]any {
	raw, _ := doc["tiers"].([]any)
	var out []map[string]any
	for _, t := range raw {
		if m := obj(t); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}

func docProducts(doc map[string]any) bool {
	return len(obj(obj(doc["pricing"])["products"])) > 0
}

func docAnyGrant(doc map[string]any) bool {
	for _, t := range docTiers(doc) {
		if obj(t["credit_grant"]) != nil || truthy(t["initial_credit"]) {
			return true
		}
	}
	return false
}

func docGiftTiers(doc map[string]any) int {
	n := 0
	for _, t := range docTiers(doc) {
		g := obj(t["credit_grant"])
		if g != nil {
			if trig := str(g["trigger"]); trig == "signup" || trig == "" {
				n++
			}
		} else if truthy(t["initial_credit"]) {
			n++
		}
	}
	return n
}

func docPriced(doc map[string]any) bool {
	if docProducts(doc) || docAnyGrant(doc) {
		return true
	}
	for _, t := range docTiers(doc) {
		switch str(t["billing_model"]) {
		case "consumption_only", "subscription_with_credits", "subscription_unlock_only":
			return true
		}
		if truthy(t["price"]) {
			return true
		}
	}
	return false
}

// RequiredGiftEvents are the delivered events the config's sign-up gift needs subscribed.
func RequiredGiftEvents(doc map[string]any) []string {
	if b, _ := obj(doc["auth_events"])["grant_on_member_joined"].(bool); b {
		return []string{"auth.user.registered", "org.member.added"}
	}
	return []string{"auth.user.registered"}
}

func finding(id, name, grade, detail, remedy string) MoneyFinding {
	return MoneyFinding{ID: id, Name: name, Grade: grade, Detail: detail, Remedy: remedy}
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

var (
	moneyCanonicalModels = map[string]bool{"capacity_only": true, "consumption_only": true,
		"subscription_with_credits": true, "subscription_unlock_only": true, "metered": true, "seat_based": true,
		"subscription_with_overage": true, "one_time_purchase": true}
	moneyExperimentalModels = map[string]bool{"metered": true, "seat_based": true,
		"subscription_with_overage": true, "one_time_purchase": true}
	moneyAliases = map[string]string{"free_tier": "capacity_only", "enterprise": "capacity_only",
		"metered_billing": "metered"}
)

// MoneyChecklist grades the eight money checks (pure).
func MoneyChecklist(doc map[string]any, f MoneyFacts) []MoneyFinding {
	if doc == nil {
		doc = map[string]any{}
	}
	runtime := f.Runtime
	if runtime == "" {
		runtime = "go"
	}
	env := strings.ToLower(f.Environment)
	var out []MoneyFinding

	// M1 — paid mode where money is declared.
	switch {
	case !docPriced(doc):
		out = append(out, finding("M1", "paid_mode", "info", "nothing in this config is priced or gifted; paid mode is not needed", ""))
	case f.Paid == "off":
		out = append(out, finding("M1", "paid_mode", "risk",
			"the config declares money but paid mode is OFF"+paren(f.PaidDetail)+
				": priced usage is not sent to billing and no sign-up gift is granted",
			"set BILLING_SERVICE_URL=https://billing.service.ab0t.com and BILLING_SERVICE_API_KEY (the key "+
				"`authsetup connect billing` prints), and remove any Options.EnablePaid=&false"))
	case f.Paid == "on" && f.PaidCodeVisible:
		out = append(out, finding("M1", "paid_mode", "good", "paid mode is on"+paren(f.PaidDetail), ""))
	case f.Paid == "on":
		out = append(out, finding("M1", "paid_mode", "attention",
			"paid mode is on from this environment"+paren(f.PaidDetail)+"; your code can still switch it off "+
				"(Options.EnablePaid=&false, Python setup_quota(enable_paid=False)) and doctor cannot see code",
			"run the checklist in-process (q.MoneyChecklist) to read the service's own answer"))
	default:
		out = append(out, finding("M1", "paid_mode", "not_checked", "paid mode was not determined", ""))
	}

	// M2 — the price list reached billing.
	pl := f.PriceList
	priceListOff := false
	if v, ok := obj(doc["billing_integration"])["price_list"].(bool); ok && !v {
		priceListOff = true
	}
	switch {
	case !docProducts(doc):
		out = append(out, finding("M2", "price_list", "info", "no pricing.products declared: there is no price list to publish", ""))
	case priceListOff:
		out = append(out, finding("M2", "price_list", "attention",
			"publishing is switched off (billing_integration.price_list=false): billing cannot check the rates "+
				"your usage events carry", ""))
	case pl == "":
		out = append(out, finding("M2", "price_list", "not_checked",
			"the publish outcome is known only to the running service (capability price_list)",
			"read q.Capabilities().PriceList in the service, or run q.MoneyChecklist there"))
	case strings.HasPrefix(pl, "published"):
		out = append(out, finding("M2", "price_list", "good", pl, ""))
	case strings.HasPrefix(pl, "refused(403"):
		out = append(out, finding("M2", "price_list", "risk", pl,
			"billing has not bound this service name to your account: run `authsetup status` and send its "+
				"price-list request (reference PL-…) to ab0t support; the library retries every 6h"))
	case strings.HasPrefix(pl, "refused(") || strings.HasPrefix(pl, "not_published"):
		out = append(out, finding("M2", "price_list", "risk", pl, "see the service log line for the price list"))
	default:
		out = append(out, finding("M2", "price_list", "attention", pl, ""))
	}

	// M3 — the gift's events.
	required := RequiredGiftEvents(doc)
	switch {
	case docGiftTiers(doc) == 0:
		out = append(out, finding("M3", "gift_events", "info", "no sign-up gift declared (no credit_grant with trigger signup)", ""))
	case f.WebhookSecretSet != nil && !*f.WebhookSecretSet:
		out = append(out, finding("M3", "gift_events", "risk",
			"AB0T_AUTH_WEBHOOK_SECRET is not set: the receiver rejects every auth event, so no gift can arrive",
			"set AB0T_AUTH_WEBHOOK_SECRET to the subscription's secret"))
	case f.SubscribedEvents == nil:
		out = append(out, finding("M3", "gift_events", "not_checked",
			"the auth subscription was not checked (the gift needs "+strings.Join(required, ", ")+")",
			"run with --live and AB0T_AUTH_AUTH_URL, AB0T_AUTH_ADMIN_TOKEN, AB0T_AUTH_WEBHOOK_PUBLIC_URL"))
	default:
		have := map[string]bool{}
		for _, e := range f.SubscribedEvents {
			have[e] = true
		}
		var missing []string
		for _, e := range required {
			if !have[e] {
				missing = append(missing, e)
			}
		}
		listens := strings.Join(f.SubscribedEvents, ", ")
		if listens == "" {
			listens = "nothing"
		}
		switch {
		case len(missing) > 0:
			out = append(out, finding("M3", "gift_events", "risk",
				"the subscription listens to "+listens+"; the gift needs "+strings.Join(missing, ", ")+
					" — auth delivers the gift on those events only",
				"re-run `quotactl subscribe-events --rotate-secret` (it subscribes to the gift's events)"))
		case f.GiftEventsSeen == nil:
			out = append(out, finding("M3", "gift_events", "attention",
				"subscribed to the right events; doctor could not see whether one has arrived",
				"run q.MoneyChecklist in the service (it reads the handler ledger)"))
		case *f.GiftEventsSeen == 0:
			out = append(out, finding("M3", "gift_events", "attention",
				"subscribed to the right events, but no gift event has been handled yet",
				"sign up a test user in dev and run again (auth must have webhook delivery on in that environment)"))
		default:
			out = append(out, finding("M3", "gift_events", "good",
				fmt.Sprintf("subscribed to %s; %d gift event(s) handled", strings.Join(required, ", "), *f.GiftEventsSeen), ""))
		}
	}

	// M4 — tier resolution.
	tp := obj(doc["tier_provider"])
	kind := str(tp["type"])
	if kind == "" && runtime == "python" {
		kind = "mesh"
	}
	defaultTier := str(tp["default_tier"])
	if defaultTier == "" && runtime == "python" {
		defaultTier = "free"
	}
	lk := f.TierLookup
	switch {
	case kind == "jwt" && !(lk != nil && lk.Outcome == "ok" && lk.ExpectTier != ""):
		out = append(out, finding("M4", "tier_resolution", "attention",
			"tier_provider jwt: tokens issued by the ab0t auth service (https://auth.service.ab0t.com) carry no "+
				"tier claim, so unless your own issuer adds one every customer resolves to default_tier \""+defaultTier+"\"",
			`use "tier_provider": {"type": "mesh"}`))
	case lk == nil:
		out = append(out, finding("M4", "tier_resolution", "not_checked", "no org was looked up",
			"run with --live --org <an org that pays> --expect-tier <its tier>"))
	case lk.Outcome == "forbidden":
		out = append(out, finding("M4", "tier_resolution", "risk",
			"billing refused to read org "+lk.Org+"'s tier with this service's key (403): every customer falls back "+
				"to default_tier \""+defaultTier+"\", paying or not",
			"ask your provider for the cross-customer tier read on your billing key"))
	case lk.Outcome != "ok":
		out = append(out, finding("M4", "tier_resolution", "not_checked",
			"the tier lookup failed ("+lk.Outcome+": "+lk.Detail+")", ""))
	case lk.ExpectTier != "" && lk.Tier != lk.ExpectTier:
		out = append(out, finding("M4", "tier_resolution", "risk",
			"org "+lk.Org+" resolves to \""+lk.Tier+"\", expected \""+lk.ExpectTier+"\"",
			"check that the payment reached billing's tier (the price → tier map)"))
	case lk.ExpectTier != "":
		out = append(out, finding("M4", "tier_resolution", "good", "org "+lk.Org+" resolves to \""+lk.Tier+"\"", ""))
	case lk.Tier == defaultTier:
		out = append(out, finding("M4", "tier_resolution", "attention",
			"org "+lk.Org+" resolves to the default tier \""+lk.Tier+"\" — if it pays, its payment did not reach billing's tier",
			"re-run with --expect-tier <its paid tier>"))
	default:
		out = append(out, finding("M4", "tier_resolution", "good", "org "+lk.Org+" resolves to \""+lk.Tier+"\"", ""))
	}

	// M5 — billing_model / credit_grant valid for this runtime.
	type issue struct{ grade, detail string }
	var issues []issue
	for _, t := range docTiers(doc) {
		tid := str(t["tier_id"])
		if tid == "" {
			tid = "?"
		}
		bm := str(t["billing_model"])
		if bm != "" {
			canon := bm
			if c, ok := moneyAliases[bm]; ok {
				canon = c
				issues = append(issues, issue{"attention", "tier " + tid + ": billing_model \"" + bm + "\" is an alias of \"" + canon + "\""})
			}
			switch {
			case !moneyCanonicalModels[canon]:
				issues = append(issues, issue{"risk", "tier " + tid + ": billing_model \"" + bm + "\" is not a billing model"})
			case moneyExperimentalModels[canon]:
				issues = append(issues, issue{"risk", "tier " + tid + ": billing_model \"" + bm + "\" is experimental: no runtime in " +
					"either library, the tier runs as limits only"})
			}
		}
		if g := obj(t["credit_grant"]); g != nil {
			trig := str(g["trigger"])
			if trig == "scheduled_period_start" {
				issues = append(issues, issue{"risk", "tier " + tid + ": credit_grant trigger scheduled_period_start is never " +
					"fired: this grant is never given"})
			}
			if runtime == "go" {
				if trig == "subscription_invoice_paid" || bm == "subscription_with_credits" {
					issues = append(issues, issue{"attention", "tier " + tid + ": the Go library has no Stripe invoice path — " +
						"plan credit is not applied by this runtime"})
				}
				switch str(g["lifecycle"]) {
				case "", "persistent", "use_it_or_lose_it", "rollover_capped":
				default:
					issues = append(issues, issue{"risk", "tier " + tid + ": credit_grant.lifecycle \"" + str(g["lifecycle"]) +
						"\" is not known to the Go library (billing refuses the grant)"})
				}
				switch str(g["destination"]) {
				case "", "credit_balance", "subscription_credit":
				default:
					issues = append(issues, issue{"risk", "tier " + tid + ": credit_grant.destination \"" + str(g["destination"]) +
						"\" is not known to the Go library (billing refuses the grant)"})
				}
			}
		}
	}
	if len(issues) > 0 {
		worst := "good"
		var details []string
		for _, is := range issues {
			if moneyGradeRank[is.grade] > moneyGradeRank[worst] {
				worst = is.grade
			}
			details = append(details, is.detail)
		}
		out = append(out, finding("M5", "billing_model_for_runtime", worst, strings.Join(details, "; "), ""))
	} else {
		out = append(out, finding("M5", "billing_model_for_runtime", "good",
			"every tier's billing_model and credit_grant is valid for the "+runtime+" library", ""))
	}

	// M6 — dev escapes.
	var escapes []string
	// The code-level switches (Options.EnablePaid=&false, setup_quota(enable_paid=False)) are not in
	// the config: they show up as paid "off" in-process (M1), and are named below when unseen.
	if b, _ := obj(doc["outbox"])["allow_ephemeral"].(bool); b {
		escapes = append(escapes, "outbox.allow_ephemeral=true (billing is disabled when the outbox is not durable)")
	}
	switch {
	case len(escapes) > 0:
		grade := "attention"
		if env == "prod" || env == "production" {
			grade = "risk"
		} else if env == "dev" || env == "development" || env == "local" || env == "test" {
			grade = "info"
		}
		detail := strings.Join(escapes, "; ")
		if env == "" {
			detail += " — the environment was not named (pass --environment)"
		}
		out = append(out, finding("M6", "no_dev_escapes", grade, detail, "remove them from the production config"))
	case f.PaidCodeVisible:
		out = append(out, finding("M6", "no_dev_escapes", "good", "no dev escape in the config or the running service", ""))
	default:
		out = append(out, finding("M6", "no_dev_escapes", "attention",
			"the config has no dev escape; doctor cannot see code (Options.EnablePaid=&false, Python "+
				"setup_quota(enable_paid=False))", "grep your service for EnablePaid / enable_paid"))
	}

	// M7 — negative balance (billing-side; never visible to the library).
	m7 := "billing lets a prepaid balance go below zero by default (a platform setting this library cannot see): " +
		"usage you record is charged even when the balance is empty"
	if docProducts(doc) {
		m7 = "billing lets a prepaid balance go below zero by default (a platform setting this library cannot see); " +
			"work metered through the meter reserves funds first, which bounds it — usage you record without a " +
			"reservation can still take a balance below zero"
	}
	out = append(out, finding("M7", "negative_balance", "not_checked", m7,
		"gate paid work on a reservation (the meter) or a balance check before it starts"))

	// M8 — failed_permanent grants.
	switch {
	case !docAnyGrant(doc):
		out = append(out, finding("M8", "failed_permanent_alert", "info", "no credit_grant declared", ""))
	case f.FailedPermanentGrants != nil && *f.FailedPermanentGrants > 0:
		out = append(out, finding("M8", "failed_permanent_alert", "risk",
			fmt.Sprintf("%d gift grant(s) failed for good and were never given", *f.FailedPermanentGrants),
			"fix the cause, re-run them (quotactl backfill), and alert on ledger status failed_permanent"))
	default:
		d := "a gift that fails for good is recorded as failed_permanent in the ledger; the library sends no alert for it"
		if f.FailedPermanentGrants != nil {
			d += " (none found now)"
		}
		out = append(out, finding("M8", "failed_permanent_alert", "attention", d,
			"alert on ledger rows with status failed_permanent (q.LedgerStore.QueryByStatus on a schedule)"))
	}
	return out
}

// MoneyFactsInProcess gathers the facts a running service can see about itself: its paid mode (the
// code's EnablePaid included), the price-list state, the receiver's secret, and from the handler
// ledger how many gift events were handled and how many failed for good. Subscription and tier
// lookups need auth / billing round-trips: pass them in via extra (nil = not checked).
func (q *Quota) MoneyFactsInProcess(ctx context.Context, extra MoneyFacts) MoneyFacts {
	c := q.Capabilities()
	f := extra
	f.Runtime = "go"
	if f.Environment == "" {
		f.Environment = firstSet(os.Getenv("AB0T_ENVIRONMENT"), os.Getenv("ENVIRONMENT"))
	}
	f.PaidCodeVisible = true
	switch {
	case strings.HasPrefix(c.BillingStatus, "ON"):
		f.Paid, f.PaidDetail = "on", c.BillingStatus
	default:
		f.Paid, f.PaidDetail = "off", c.BillingStatus
	}
	if f.PriceList == "" {
		f.PriceList = c.PriceList
	}
	set := os.Getenv("AB0T_AUTH_WEBHOOK_SECRET") != ""
	f.WebhookSecretSet = &set
	if q.LedgerStore != nil {
		gift := map[string]bool{}
		for _, e := range RequiredGiftEvents(ConfigDocument(q.Cfg)) {
			gift[e] = true
		}
		count := func(st handlerledger.LedgerStatus) (int, bool) {
			rows, err := q.LedgerStore.QueryByStatus(ctx, st, handlerledger.QueryOptions{Limit: 500})
			if err != nil {
				return 0, false
			}
			n := 0
			for _, r := range rows {
				if gift[r.EventType] {
					n++
				}
			}
			return n, true
		}
		if s, ok1 := count(handlerledger.StatusSuccess); ok1 {
			if k, ok2 := count(handlerledger.StatusSkipped); ok2 {
				seen := s + k
				f.GiftEventsSeen = &seen
			}
		}
		if n, ok := count(handlerledger.StatusFailedPermanent); ok {
			f.FailedPermanentGrants = &n
		}
	}
	return f
}

// MoneyChecklist runs the checklist in-process (see MoneyFactsInProcess).
func (q *Quota) MoneyChecklist(ctx context.Context, extra MoneyFacts) []MoneyFinding {
	return MoneyChecklist(ConfigDocument(q.Cfg), q.MoneyFactsInProcess(ctx, extra))
}

// RenderMoneyChecklist is the human report.
func RenderMoneyChecklist(findings []MoneyFinding) string {
	var b strings.Builder
	b.WriteString("DOCTOR --money — the money-safety checklist (not_checked = doctor could not see it; never a pass)\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "  %s %-12s %-26s %s\n", f.ID, strings.ToUpper(f.Grade), f.Name, f.Detail)
		if f.Remedy != "" && (f.Grade == "risk" || f.Grade == "attention" || f.Grade == "not_checked") {
			fmt.Fprintf(&b, "     remedy: %s\n", f.Remedy)
		}
	}
	return b.String()
}
