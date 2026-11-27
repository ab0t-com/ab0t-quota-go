package config

// pricing.products.<product>.variants.<variant>.{price_per_hour, allocation_price} (v0.2.0) — money
// is exact: a price with more than 6 decimal places, or above $9,007,199,254.740991, is refused at
// config load, like the Python runtime's config schema. Presence and positivity stay
// meter.ParsePricing's to judge. The same rule is billing.MoneyFromDecimal (this package cannot
// import billing); keep the two identical.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// maxMoneyMicros mirrors billing.MaxMoneyMicros (2^53 - 1 micro-units).
const maxMoneyMicros int64 = 1<<53 - 1

func validatePricingPrices(raw json.RawMessage) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil
	}
	var doc struct {
		Products map[string]struct {
			Variants map[string]map[string]any `json:"variants"`
		} `json:"products"`
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber() // a JSON number's exact text, never float64
	if err := dec.Decode(&doc); err != nil {
		return nil // the shape is meter.ParsePricing's to judge
	}
	pids := make([]string, 0, len(doc.Products))
	for id := range doc.Products {
		pids = append(pids, id)
	}
	sort.Strings(pids)
	for _, pid := range pids {
		variants := doc.Products[pid].Variants
		vids := make([]string, 0, len(variants))
		for id := range variants {
			vids = append(vids, id)
		}
		sort.Strings(vids)
		for _, vid := range vids {
			for _, key := range []string{"price_per_hour", "allocation_price"} {
				var s string
				switch v := variants[vid][key].(type) {
				case json.Number:
					s = v.String()
				case string:
					s = strings.TrimSpace(v)
				default:
					continue
				}
				d, err := decimal.NewFromString(s)
				if err != nil {
					continue // not a number: meter.ParsePricing reports it
				}
				where := "pricing.products." + pid + ".variants." + vid + "." + key
				// Edge guards (v0.2.0, = billing.MoneyFromDecimal): an absurd exponent ("1e999999999")
				// is judged from the digit count, never by rescaling (that builds 10^exponent — a hang).
				if d.IsZero() {
					continue
				}
				shift := int64(d.Exponent()) + 6
				digits := int64(len(new(big.Int).Abs(d.Coefficient()).String()))
				if shift >= 0 && digits+shift > 17 {
					return fmt.Errorf("config: %s is out of range (at most 9007199254.740991)", where)
				}
				if shift < 0 && -shift > digits {
					return fmt.Errorf("config: %s has more than 6 decimal places (money is exact to 0.000001)", where)
				}
				scaled := d.Shift(6)
				if !scaled.Equal(scaled.Truncate(0)) {
					return fmt.Errorf("config: %s=%s has more than 6 decimal places (money is exact to 0.000001)", where, s)
				}
				if scaled.Abs().GreaterThan(decimal.NewFromInt(maxMoneyMicros)) {
					return fmt.Errorf("config: %s=%s is out of range (at most 9007199254.740991)", where, s)
				}
			}
		}
	}
	return nil
}
