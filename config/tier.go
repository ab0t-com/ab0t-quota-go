package config

import (
	"encoding/json"
	"fmt"
	"math"
)

// CreditTrigger enumerates when a CreditGrant fires.
type CreditTrigger string

const (
	CreditTriggerSignup                  CreditTrigger = "signup"
	CreditTriggerSubscriptionInvoicePaid CreditTrigger = "subscription_invoice_paid"
	CreditTriggerScheduledPeriodStart    CreditTrigger = "scheduled_period_start"
	CreditTriggerManual                  CreditTrigger = "manual"
	CreditTriggerWebhookAdmin            CreditTrigger = "webhook_admin"
)

// CreditLifecycle controls what happens to unused balance at each new grant.
type CreditLifecycle string

const (
	CreditLifecyclePersistent     CreditLifecycle = "persistent"
	CreditLifecycleUseItOrLoseIt  CreditLifecycle = "use_it_or_lose_it"
	CreditLifecycleRolloverCapped CreditLifecycle = "rollover_capped"
)

// CreditDestination is the balance bucket the grant lands in.
type CreditDestination string

const (
	CreditDestCreditBalance      CreditDestination = "credit_balance"
	CreditDestSubscriptionCredit CreditDestination = "subscription_credit"
)

// DedupPolicy controls how the default credit-grant handler keys its
// business dedup. See PRODUCT_SPEC.md §13.5 for the key shapes.
type DedupPolicy string

const (
	DedupPerUserPerTier DedupPolicy = "per_user_per_tier" // default — anti-farming
	DedupPerOrgPerTier  DedupPolicy = "per_org_per_tier"  // B2B
	DedupPerUserGlobal  DedupPolicy = "per_user_global"
	DedupPerOrgGlobal   DedupPolicy = "per_org_global"
)

// BillingModel describes how a tier is billed. Some values are gated by
// AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS — load-time validation
// enforces the gate.
type BillingModel string

const (
	BillingModelConsumptionOnly         BillingModel = "consumption_only"
	BillingModelSubscriptionUnlockOnly  BillingModel = "subscription_unlock_only"
	BillingModelSubscriptionWithCredits BillingModel = "subscription_with_credits"
	BillingModelMeteredBilling          BillingModel = "metered_billing"
	BillingModelOneTimePurchase         BillingModel = "one_time_purchase"
	BillingModelFreeTier                BillingModel = "free_tier"
	BillingModelEnterprise              BillingModel = "enterprise"

	// v0.3.2 — one billing_model name set for both libraries (testdata/billing_models_golden_20261128.json).
	// The Python library's names: capacity_only is what an absent billing_model (and free_tier /
	// enterprise, which have no rule of their own here) always meant — limits only; the three
	// experimental ones have no runtime in either library.
	BillingModelCapacityOnly            BillingModel = "capacity_only"
	BillingModelMetered                 BillingModel = "metered"
	BillingModelSeatBased               BillingModel = "seat_based"
	BillingModelSubscriptionWithOverage BillingModel = "subscription_with_overage"
)

// BillingModelAliases (v0.3.2) maps a name to the canonical name with the SAME meaning in both
// libraries. Both libraries accept an alias and print BillingModelAliasWarning; nothing is ever
// silently treated as a different model.
var BillingModelAliases = map[BillingModel]BillingModel{
	BillingModelFreeTier:       BillingModelCapacityOnly,
	BillingModelEnterprise:     BillingModelCapacityOnly,
	BillingModelMeteredBilling: BillingModelMetered,
}

// billingModelShortMeaning is the shared one-line meaning of each canonical name.
var billingModelShortMeaning = map[BillingModel]string{
	BillingModelCapacityOnly:            "limits only: the tier itself moves no money",
	BillingModelConsumptionOnly:         "pay as you go from a prepaid balance",
	BillingModelSubscriptionWithCredits: "a recurring price that includes credit",
	BillingModelSubscriptionUnlockOnly:  "a recurring price that unlocks limits, no included credit",
	BillingModelMetered:                 "experimental: no runtime in either library yet",
	BillingModelSeatBased:               "experimental: no runtime in either library yet",
	BillingModelSubscriptionWithOverage: "experimental: no runtime in either library yet",
	BillingModelOneTimePurchase:         "experimental: no runtime in either library yet",
}

// Canonical (v0.3.2) returns the canonical name for m: the alias target, else m itself ("" stays "",
// which means capacity_only).
func (m BillingModel) Canonical() BillingModel {
	if c, ok := BillingModelAliases[m]; ok {
		return c
	}
	return m
}

// IsKnown (v0.3.2) reports whether m is a canonical name, an alias, or absent.
func (m BillingModel) IsKnown() bool {
	if m == "" {
		return true
	}
	_, ok := billingModelShortMeaning[m.Canonical()]
	return ok
}

// pythonOnlyExperimental (v0.3.2) — experimental names this library used to accept as "unknown"
// (a warning, booting as limits only). The Python library refuses them without the opt-in; this
// library warns (BillingModelGoGraceWarning) until its next minor release, because refusing a config
// that started yesterday is a breaking change.
func (m BillingModel) pythonOnlyExperimental() bool {
	switch m {
	case BillingModelMetered, BillingModelSeatBased, BillingModelSubscriptionWithOverage:
		return true
	}
	return false
}

// BillingModelAliasWarning is the text both libraries print for a tier whose billing_model is an alias.
func BillingModelAliasWarning(tierID string, name BillingModel) string {
	c := name.Canonical()
	return `tier "` + tierID + `": billing_model "` + string(name) + `" is another name for "` + string(c) +
		`" (` + billingModelShortMeaning[c] + `); both libraries accept it — write "` + string(c) +
		`" to silence this warning`
}

// BillingModelGoGraceWarning is the text for a Python-only experimental name without the opt-in.
func BillingModelGoGraceWarning(tierID string, name BillingModel) string {
	return `tier "` + tierID + `": billing_model "` + string(name) + `" is experimental and has no runtime in ` +
		`either library yet; the Python library refuses it unless AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS=true, ` +
		`and this library will too from its next minor release — until then this tier runs as limits only`
}

// ScheduledGrantNeverFiresWarning (v0.3.2) is the text both libraries print for a credit_grant whose
// trigger no runtime fires.
func ScheduledGrantNeverFiresWarning(tierID string) string {
	return `tier "` + tierID + `": credit_grant.trigger "scheduled_period_start" is accepted but neither library ` +
		`fires it yet — this grant is never given (no periodic refill happens); use "signup", or grant the ` +
		`credit yourself through billing`
}

// IsExperimental reports whether the model needs the experimental gate.
func (m BillingModel) IsExperimental() bool {
	switch m {
	case BillingModelMeteredBilling, BillingModelOneTimePurchase:
		return true
	}
	return false
}

// BillingPeriod is how often a recurring price bills.
type BillingPeriod string

const (
	BillingPeriodMonth BillingPeriod = "month"
	BillingPeriodYear  BillingPeriod = "year"
	BillingPeriodWeek  BillingPeriod = "week"
	BillingPeriodDay   BillingPeriod = "day"
)

// Price is the tier's recurring price.
type Price struct {
	AmountPerPeriod Decimal       `json:"amount_per_period"`
	Currency        string        `json:"currency,omitempty"`
	Period          BillingPeriod `json:"period,omitempty"`
}

// CreditGrant is a per-tier grant declaration.
type CreditGrant struct {
	Trigger            CreditTrigger     `json:"trigger"`
	AmountPerPeriod    Decimal           `json:"amount_per_period"`
	Currency           string            `json:"currency,omitempty"`
	Lifecycle          CreditLifecycle   `json:"lifecycle,omitempty"`
	RolloverMaxPeriods *int              `json:"rollover_max_periods,omitempty"`
	Destination        CreditDestination `json:"destination,omitempty"`
	ResetOnDowngrade   bool              `json:"reset_on_downgrade"`
	ResetOnUpgrade     bool              `json:"reset_on_upgrade"`
	Dedup              DedupPolicy       `json:"dedup,omitempty"`

	// resetOnDowngradeUnset (v0.3.0) is true when LoadConfig read this credit_grant from a config
	// file WITHOUT a reset_on_downgrade key (LoadConfig then set ResetOnDowngrade = true): Warnings()
	// names it. Go-code literals never set it.
	resetOnDowngradeUnset bool
}

// Validate enforces cross-field rules.
func (c *CreditGrant) Validate() error {
	if c.Lifecycle == CreditLifecycleRolloverCapped && c.RolloverMaxPeriods == nil {
		return fmt.Errorf("credit_grant.rollover_max_periods required when lifecycle is %q",
			c.Lifecycle)
	}
	if c.Lifecycle != CreditLifecycleRolloverCapped && c.RolloverMaxPeriods != nil {
		return fmt.Errorf("credit_grant.rollover_max_periods only valid with lifecycle %q",
			CreditLifecycleRolloverCapped)
	}
	if c.AmountPerPeriod.Decimal.Sign() <= 0 {
		return fmt.Errorf("credit_grant.amount_per_period must be > 0")
	}
	return nil
}

// applyDefaults sets schema-level defaults if absent.
func (c *CreditGrant) applyDefaults() {
	if c.Currency == "" {
		c.Currency = "USD"
	}
	if c.Lifecycle == "" {
		c.Lifecycle = CreditLifecycleUseItOrLoseIt
	}
	if c.Destination == "" {
		c.Destination = CreditDestSubscriptionCredit
	}
	if c.Dedup == "" {
		c.Dedup = DedupPerUserPerTier
	}
	// ResetOnDowngrade / ResetOnUpgrade: no default is applied HERE. Since v0.3.0 LoadConfig sets
	// reset_on_downgrade to TRUE when a config file's credit_grant omits it (markUnsetDefaults; Python
	// parity) and warns (ResetOnDowngradeUnsetWarning); before v0.3.0 the zero value false ran (this
	// comment used to claim true). A CreditGrant built in Go code keeps the value it was given (zero =
	// false). ResetOnUpgrade has no default other than false (Python: false too).
}

// TierLimit holds one resource's tier-level limit + optional thresholds.
// Accepts JSON shapes:
//   - bare number: {"sandbox.concurrent": 25}
//   - null: {"resource.cpu_cores": null} → unlimited
//   - object: {"...": {"limit": 25, "warning_threshold": 0.8, "burst_allowance": 5}}
type TierLimit struct {
	Limit             *float64 `json:"limit"`
	WarningThreshold  float64  `json:"warning_threshold,omitempty"`
	CriticalThreshold float64  `json:"critical_threshold,omitempty"`
	BurstAllowance    float64  `json:"burst_allowance,omitempty"`
	PerUserLimit      *float64 `json:"per_user_limit,omitempty"`
}

// IsUnlimited reports whether the limit is nil (no cap).
func (t TierLimit) IsUnlimited() bool { return t.Limit == nil }

// UnmarshalJSON accepts the three shapes documented on TierLimit.
func (t *TierLimit) UnmarshalJSON(b []byte) error {
	t.WarningThreshold = 0.80
	t.CriticalThreshold = 0.95
	if len(b) == 0 || string(b) == "null" {
		t.Limit = nil
		return nil
	}
	// Number form
	if b[0] != '{' {
		var f float64
		if err := json.Unmarshal(b, &f); err != nil {
			return err
		}
		t.Limit = &f
		return nil
	}
	// Object form — use an alias to avoid recursion.
	type alias TierLimit
	a := alias(*t) // carries the defaults we set above
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*t = TierLimit(a)
	return nil
}

// DerivePerUserLimit returns the resolved per-user limit for a tier+resource:
// explicit per_user_limit wins; else ceil(limit × fraction) floored at 1.0;
// returns nil for unlimited or when fraction is unset.
func (t TierLimit) DerivePerUserLimit(fraction float64) *float64 {
	if t.PerUserLimit != nil {
		return t.PerUserLimit
	}
	if t.Limit == nil || fraction <= 0 {
		return nil
	}
	v := math.Ceil(*t.Limit * fraction)
	if v < 1.0 {
		v = 1.0
	}
	return &v
}

// Tier is one row of the .tiers[] array.
type Tier struct {
	TierID                 string               `json:"tier_id"`
	DisplayName            string               `json:"display_name"`
	Description            string               `json:"description,omitempty"`
	SortOrder              int                  `json:"sort_order"`
	Features               []string             `json:"features,omitempty"`
	Limits                 map[string]TierLimit `json:"limits"`
	UpgradeURL             string               `json:"upgrade_url,omitempty"`
	InitialCredit          *Decimal             `json:"initial_credit,omitempty"` // legacy — synthesizes a signup grant
	CreditGrant            *CreditGrant         `json:"credit_grant,omitempty"`
	DefaultPerUserFraction float64              `json:"default_per_user_fraction,omitempty"`
	BillingModel           BillingModel         `json:"billing_model,omitempty"`
	Price                  *Price               `json:"price,omitempty"`
}

// Validate enforces tier-level cross-field rules.
func (t *Tier) Validate(experimentalAllowed bool) error {
	if t.TierID == "" {
		return fmt.Errorf("tier: tier_id required")
	}
	if t.DisplayName == "" {
		t.DisplayName = t.TierID // graceful default
	}

	// Experimental billing-model gate.
	if t.BillingModel != "" && t.BillingModel.IsExperimental() && !experimentalAllowed {
		return fmt.Errorf("tier %q: billing_model %q is experimental — set AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS=true to enable",
			t.TierID, t.BillingModel)
	}

	// Billing model + price + credit_grant cross-field rules.
	switch t.BillingModel {
	case BillingModelSubscriptionWithCredits:
		if t.Price == nil {
			return fmt.Errorf("tier %q: billing_model %q requires price",
				t.TierID, t.BillingModel)
		}
		if t.CreditGrant == nil ||
			t.CreditGrant.Trigger != CreditTriggerSubscriptionInvoicePaid {
			return fmt.Errorf("tier %q: billing_model %q requires credit_grant with trigger %q",
				t.TierID, t.BillingModel, CreditTriggerSubscriptionInvoicePaid)
		}
	case BillingModelSubscriptionUnlockOnly:
		if t.CreditGrant != nil {
			return fmt.Errorf("tier %q: billing_model %q forbids credit_grant",
				t.TierID, t.BillingModel)
		}
	case BillingModelConsumptionOnly:
		if t.Price != nil {
			return fmt.Errorf("tier %q: billing_model %q forbids price",
				t.TierID, t.BillingModel)
		}
	}

	// initial_credit + credit_grant synthesis (legacy → new schema).
	if t.InitialCredit != nil && t.CreditGrant == nil {
		t.CreditGrant = &CreditGrant{
			Trigger:          CreditTriggerSignup,
			AmountPerPeriod:  *t.InitialCredit,
			Lifecycle:        CreditLifecyclePersistent,
			Destination:      CreditDestCreditBalance,
			Dedup:            DedupPerUserPerTier,
			ResetOnDowngrade: false,
			ResetOnUpgrade:   false,
		}
	}

	if t.CreditGrant != nil {
		t.CreditGrant.applyDefaults()
		if err := t.CreditGrant.Validate(); err != nil {
			return fmt.Errorf("tier %q: %w", t.TierID, err)
		}
	}
	return nil
}
