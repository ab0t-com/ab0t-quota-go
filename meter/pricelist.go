package meter

// The price list a service publishes to billing at startup (v0.3.0, additive). Same wire, same
// canonical bytes and the same version as the Python library (ab0t_quota.price_list); shared vectors:
// testdata/price_list_golden_20261127.json.
//
//	PUT {billing}/billing/price-list/{service_name}
//	{"products": {<product>: {["funding_sources": [...],] "variants": {<variant>:
//	   {"allocation_price": "<money>", "default": <bool>, "price_per_hour": "<money>"}}}},
//	 "published_by": "ab0t-quota-go/<version>", "schema": "ab0t.price_list.v1",
//	 "service_name": "...", "version": "sha256:<64 hex>"}
//
// Money is a JSON string in the canonical text (billing.Money.String(): no exponent, no trailing
// zeros, "0"). version = "sha256:" + hex(sha256(canonical JSON of products)) — keys sorted at every
// level, no whitespace, no HTML escaping. The rules are ParsePricing's: a price list that breaks
// them is an ErrConfig and is never published.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/billing"
)

// PriceListSchema is the body's schema id.
const PriceListSchema = "ab0t.price_list.v1"

// MaxPriceListVariants is the most variants billing accepts in one price list.
const MaxPriceListVariants = 500

// PriceListVariant is one variant on the wire (fields in key order: the JSON is canonical).
type PriceListVariant struct {
	AllocationPrice string `json:"allocation_price"`
	Default         bool   `json:"default"`
	PricePerHour    string `json:"price_per_hour"`
}

// PriceListProduct is one product on the wire. FundingSources is present only when declared.
type PriceListProduct struct {
	FundingSources []string                    `json:"funding_sources,omitempty"`
	Variants       map[string]PriceListVariant `json:"variants"`
}

// PriceList is the PUT body (fields in key order, so the body is canonical too).
type PriceList struct {
	Products    map[string]PriceListProduct `json:"products"`
	PublishedBy string                      `json:"published_by"`
	Schema      string                      `json:"schema"`
	ServiceName string                      `json:"service_name"`
	Version     string                      `json:"version"`
}

// CanonicalJSON encodes v with sorted map keys, no whitespace and no HTML escaping (the bytes the
// Python library's canonical_json writes for the same value).
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// CanonicalPriceListProducts validates the raw `pricing` section (ParsePricing's rules) and returns
// the product table as it goes on the wire.
func CanonicalPriceListProducts(pricing json.RawMessage) (map[string]PriceListProduct, error) {
	book, err := ParsePricing(pricing)
	if err != nil {
		return nil, err
	}
	// Billing refuses a product or variant id containing '#' (it keys its rows with '#') and more than
	// MaxPriceListVariants variants — refused here first, the same rule as the Python library.
	var problems []string
	total := 0
	for _, pid := range book.Products() {
		if strings.Contains(pid, "#") {
			problems = append(problems, pid+": a product id may not contain '#'")
		}
		for _, vid := range sortedKeys(book.products[pid]) {
			total++
			if strings.Contains(vid, "#") {
				problems = append(problems, pid+":"+vid+": a variant id may not contain '#'")
			}
		}
	}
	if total > MaxPriceListVariants {
		problems = append(problems, fmt.Sprintf("%d variants in total (billing accepts at most %d)", total, MaxPriceListVariants))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: pricing.products cannot be published: %s", ErrConfig, strings.Join(problems, "; "))
	}
	out := make(map[string]PriceListProduct, len(book.products))
	for pid, variants := range book.products {
		p := PriceListProduct{FundingSources: book.FundingSources(pid), Variants: map[string]PriceListVariant{}}
		for vid, price := range variants {
			rate, err := billing.MoneyFromDecimal(price.HourlyRate)
			if err != nil {
				return nil, errors.Join(ErrConfig, err)
			}
			fee, err := billing.MoneyFromDecimal(price.AllocationFee)
			if err != nil {
				return nil, errors.Join(ErrConfig, err)
			}
			p.Variants[vid] = PriceListVariant{AllocationPrice: fee.String(), Default: price.Default, PricePerHour: rate.String()}
		}
		out[pid] = p
	}
	return out, nil
}

// PriceListVersion is "sha256:" + hex(sha256(CanonicalJSON(products))).
func PriceListVersion(products map[string]PriceListProduct) (string, error) {
	b, err := CanonicalJSON(products)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// BuildPriceList builds the PUT body for serviceName from the raw `pricing` section.
func BuildPriceList(serviceName string, pricing json.RawMessage, publishedBy string) (*PriceList, error) {
	name := strings.TrimSpace(serviceName)
	if name == "" {
		return nil, errors.Join(ErrConfig, errors.New("a service name is required to publish a price list"))
	}
	products, err := CanonicalPriceListProducts(pricing)
	if err != nil {
		return nil, err
	}
	version, err := PriceListVersion(products)
	if err != nil {
		return nil, err
	}
	return &PriceList{Products: products, PublishedBy: publishedBy, Schema: PriceListSchema,
		ServiceName: name, Version: version}, nil
}
