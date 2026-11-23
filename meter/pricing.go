package meter

// Strict prices from quota-config.json's `pricing` section (v0.1.12, additive). Same rules as the
// Python ab0t_quota.billing.meter.PriceBook: every declared variant states price_per_hour (> 0) and
// allocation_price (>= 0; write 0 for none), refused at construction otherwise; a product with
// several variants needs the variant named or exactly one `default: true` — never a silent pick.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// Price is one variant's customer price.
type Price struct {
	HourlyRate    decimal.Decimal
	AllocationFee decimal.Decimal
	Default       bool
}

// PriceBook is a validated price table: product → variant → Price.
type PriceBook struct {
	products map[string]map[string]Price
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
	book := &PriceBook{products: map[string]map[string]Price{}}
	for _, pid := range sortedKeys(doc.Products) {
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
			"billed at a number nobody agreed: %s", ErrConfig, strings.Join(problems, "; "))
	}
	return book, nil
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
	return d, ""
}

// Products lists the declared product ids.
func (b *PriceBook) Products() []string { return sortedKeys(b.products) }

// Lookup returns the price for product (+ variant, "" = the product's only or default variant).
func (b *PriceBook) Lookup(product, variant string) (Price, error) {
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
