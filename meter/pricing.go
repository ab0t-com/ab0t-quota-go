package meter

// Strict prices from quota-config.json's `pricing` section (v0.1.12, additive). Same rules as the
// Python ab0t_quota.billing.meter.PriceBook: every declared variant states price_per_hour (> 0) and
// allocation_price (>= 0; write 0 for none), refused at construction otherwise; a product with
// several variants needs the variant named or exactly one `default: true` — never a silent pick.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/shopspring/decimal"
)

// Price is one variant's customer price.
type Price struct {
	HourlyRate    decimal.Decimal
	AllocationFee decimal.Decimal
	Default       bool
	// FundingSources (v0.2.0) is the product's pricing.products.<product>.funding_sources — which of
	// billing's buckets may fund a hold for it. nil = every bucket (the field was not declared).
	FundingSources []string
}

// PriceBook is a validated price table: product → variant → Price.
type PriceBook struct {
	products map[string]map[string]Price
	funding  map[string][]string // product → declared funding_sources (absent = every bucket)
}

// ParsePricing validates and indexes the raw `pricing` JSON (config.Config.Pricing). An empty or
// incomplete section is an ErrConfig naming every problem.
func ParsePricing(raw json.RawMessage) (*PriceBook, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, fmt.Errorf("%w: pricing has no products — declare pricing.products.<product>.variants.<variant>."+
			"{price_per_hour, allocation_price} in quota-config.json", ErrConfig)
	}
	var doc struct {
		Products map[string]struct {
			Variants map[string]map[string]any `json:"variants"`
			// v0.2.0: optional; absent/null = every bucket. Raw so a wrong type is reported, not dropped.
			FundingSources json.RawMessage `json:"funding_sources"`
		} `json:"products"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: pricing is not valid JSON: %v", ErrConfig, err)
	}
	if len(doc.Products) == 0 {
		return nil, fmt.Errorf("%w: pricing has no products — declare pricing.products in quota-config.json", ErrConfig)
	}
	var problems []string
	book := &PriceBook{products: map[string]map[string]Price{}, funding: map[string][]string{}}
	for _, pid := range sortedKeys(doc.Products) {
		if fs, problem := fundingSources(doc.Products[pid].FundingSources); problem != "" {
			problems = append(problems, pid+": "+problem)
		} else if fs != nil {
			book.funding[pid] = fs
		}
		variants := doc.Products[pid].Variants
		if len(variants) == 0 {
			problems = append(problems, pid+": no variants")
			continue
		}
		book.products[pid] = map[string]Price{}
		for _, vid := range sortedKeys(variants) {
			v := variants[vid]
			rate, rp := priceField(v, "price_per_hour", true)
			fee, fp := priceField(v, "allocation_price", false)
			if rp != "" || fp != "" {
				for _, p := range []string{rp, fp} {
					if p != "" {
						problems = append(problems, pid+":"+vid+" "+p)
					}
				}
				continue
			}
			isDefault, _ := v["default"].(bool)
			book.products[pid][vid] = Price{HourlyRate: rate, AllocationFee: fee, Default: isDefault}
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: pricing.products is incomplete — a variant without a price would be reserved and "+
			"billed at a number nobody agreed, and a funding_sources entry billing does not know would be "+
			"refused at every reserve: %s", ErrConfig, strings.Join(problems, "; "))
	}
	return book, nil
}

// fundingSources reads a product's optional funding_sources (v0.2.0). Absent or null → (nil, ""):
// every bucket. Otherwise a non-empty list of billing's bucket names, each at most once.
func fundingSources(raw json.RawMessage) ([]string, string) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil, ""
	}
	var list []string
	if err := json.Unmarshal(t, &list); err != nil {
		return nil, "funding_sources must be a list of billing bucket names (" +
			billing.FundingBalance + ", " + billing.FundingSubscriptionCredit + ", " + billing.FundingCreditBalance + ")"
	}
	if list == nil {
		list = []string{}
	}
	if err := billing.ValidateFundingSources(list); err != nil {
		return nil, err.Error()
	}
	return list, ""
}

// FundingSources (v0.2.0) is the product's declared funding_sources (a copy), nil when the product
// does not declare it (every bucket) or is not declared at all.
func (b *PriceBook) FundingSources(product string) []string {
	fs, ok := b.funding[product]
	if !ok {
		return nil
	}
	return append([]string(nil), fs...)
}

func priceField(v map[string]any, name string, positive bool) (decimal.Decimal, string) {
	raw, ok := v[name]
	if !ok || raw == nil {
		return decimal.Zero, "missing " + name
	}
	var s string
	switch t := raw.(type) {
	case json.Number:
		s = t.String()
	case string:
		s = strings.TrimSpace(t)
	default:
		return decimal.Zero, fmt.Sprintf("%s=%v is not a number", name, raw)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Sprintf("%s=%q is not a number", name, s)
	}
	if d.Sign() < 0 || (positive && d.Sign() == 0) {
		return decimal.Zero, fmt.Sprintf("%s=%s is not allowed", name, s)
	}
	// v0.2.0: money is exact — more than 6 decimal places (or out of range) is a load error, never a
	// silently rounded price. (The JSON number's exact text is read: the decoder uses UseNumber.)
	if _, err := billing.MoneyFromDecimal(d); err != nil {
		var me *billing.MoneyError
		if errors.As(err, &me) && me.Kind == billing.MoneyRange {
			return decimal.Zero, fmt.Sprintf("%s=%s is out of range (at most 9007199254.740991)", name, s)
		}
		return decimal.Zero, fmt.Sprintf("%s=%s has more than 6 decimal places (money is exact to 0.000001)", name, s)
	}
	return d, ""
}

// Products lists the declared product ids.
func (b *PriceBook) Products() []string { return sortedKeys(b.products) }

// Lookup returns the price for product (+ variant, "" = the product's only or default variant),
// with the product's FundingSources (v0.2.0).
func (b *PriceBook) Lookup(product, variant string) (Price, error) {
	p, err := b.lookupVariant(product, variant)
	if err != nil {
		return Price{}, err
	}
	p.FundingSources = b.FundingSources(product)
	return p, nil
}

func (b *PriceBook) lookupVariant(product, variant string) (Price, error) {
	variants, ok := b.products[product]
	if !ok {
		return Price{}, fmt.Errorf("%w: pricing product %q is not declared in pricing.products (declared: %v)",
			ErrConfig, product, b.Products())
	}
	if variant != "" {
		p, ok := variants[variant]
		if !ok {
			return Price{}, fmt.Errorf("%w: pricing variant %s:%s is not declared (declared: %v)",
				ErrConfig, product, variant, sortedKeys(variants))
		}
		return p, nil
	}
	if len(variants) == 1 {
		for _, p := range variants {
			return p, nil
		}
	}
	var defaults []Price
	for _, vid := range sortedKeys(variants) {
		if variants[vid].Default {
			defaults = append(defaults, variants[vid])
		}
	}
	if len(defaults) != 1 {
		return Price{}, fmt.Errorf("%w: pricing product %q has %d variants and %d marked default: true — name the "+
			"variant, or mark exactly one default", ErrConfig, product, len(variants), len(defaults))
	}
	return defaults[0], nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
