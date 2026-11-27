package meter_test

// v0.3.0 — the price list a service publishes to billing (pricing.products). The vectors are
// testdata/price_list_golden_20261127.json, a byte-identical copy of the Python library's
// tests/price_list_golden_20261127.json: the same config must give the same canonical bytes and the
// same version from either library. RED before v0.3.0: meter.BuildPriceList does not exist.

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/meter"
)

type priceListGolden struct {
	Schema string `json:"schema"`
	Valid  []struct {
		Name        string          `json:"name"`
		ServiceName string          `json:"service_name"`
		Pricing     json.RawMessage `json:"pricing"`
		Canonical   string          `json:"canonical"`
		Version     string          `json:"version"`
	} `json:"valid"`
	Invalid []struct {
		Name    string          `json:"name"`
		Pricing json.RawMessage `json:"pricing"`
	} `json:"invalid"`
}

func loadPriceListGolden(t *testing.T) priceListGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/price_list_golden_20261127.json")
	if err != nil {
		t.Fatal(err)
	}
	var g priceListGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Valid) == 0 || len(g.Invalid) == 0 {
		t.Fatal("empty golden file")
	}
	return g
}

func TestPriceList_CanonicalBytesAndVersionMatchTheSharedVectors_20261127(t *testing.T) {
	g := loadPriceListGolden(t)
	for _, c := range g.Valid {
		t.Run(c.Name, func(t *testing.T) {
			products, err := meter.CanonicalPriceListProducts(c.Pricing)
			if err != nil {
				t.Fatal(err)
			}
			b, err := meter.CanonicalJSON(products)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != c.Canonical {
				t.Fatalf("canonical bytes differ from the shared vector\n got: %s\nwant: %s", b, c.Canonical)
			}
			pl, err := meter.BuildPriceList(c.ServiceName, c.Pricing, "ab0t-quota-go/test")
			if err != nil {
				t.Fatal(err)
			}
			if pl.Version != c.Version || pl.Schema != g.Schema || pl.Schema != meter.PriceListSchema || pl.ServiceName != c.ServiceName {
				t.Fatalf("body %+v (want version %s schema %s)", pl, c.Version, g.Schema)
			}
		})
	}
}

// CONTROL: a changed price changes the version.
func TestPriceList_ChangedPriceChangesTheVersion_20261127(t *testing.T) {
	g := loadPriceListGolden(t)
	changed := json.RawMessage(strings.Replace(string(g.Valid[0].Pricing), `"3.000000"`, `"3.000001"`, 1))
	pl, err := meter.BuildPriceList("acme-render", changed, "x")
	if err != nil {
		t.Fatal(err)
	}
	if pl.Version == g.Valid[0].Version {
		t.Fatal("a different price produced the same version")
	}
}

func TestPriceList_InvalidPricingIsNeverBuilt_20261127(t *testing.T) {
	g := loadPriceListGolden(t)
	for _, c := range g.Invalid {
		t.Run(c.Name, func(t *testing.T) {
			if _, err := meter.BuildPriceList("acme-render", c.Pricing, "x"); !errors.Is(err, meter.ErrConfig) {
				t.Fatalf("want ErrConfig, got %v", err)
			}
		})
	}
}

func variantsJSON(n int) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"products":{"render_job":{"variants":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"v` + strconv.Itoa(i) + `":{"price_per_hour":"1","allocation_price":"0"}`)
	}
	b.WriteString(`}}}}`)
	return json.RawMessage(b.String())
}

func TestPriceList_MoreVariantsThanBillingAcceptsIsNeverBuilt_20261127(t *testing.T) {
	raw, _ := os.ReadFile("../testdata/price_list_golden_20261127.json")
	var g struct {
		MaxVariants int `json:"max_variants"`
	}
	if err := json.Unmarshal(raw, &g); err != nil || g.MaxVariants != meter.MaxPriceListVariants {
		t.Fatalf("golden max_variants %d vs %d (%v)", g.MaxVariants, meter.MaxPriceListVariants, err)
	}
	if _, err := meter.BuildPriceList("acme-render", variantsJSON(meter.MaxPriceListVariants), "x"); err != nil {
		t.Fatalf("CONTROL: the limit itself must build: %v", err)
	}
	if _, err := meter.BuildPriceList("acme-render", variantsJSON(meter.MaxPriceListVariants+1), "x"); !errors.Is(err, meter.ErrConfig) {
		t.Fatalf("want ErrConfig over the limit, got %v", err)
	}
}

func TestPriceList_BodyIsCanonicalAndUnescaped_20261127(t *testing.T) {
	g := loadPriceListGolden(t)
	pl, err := meter.BuildPriceList("acme-render", g.Valid[2].Pricing, "ab0t-quota-go/test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := meter.CanonicalJSON(pl)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// keys in sorted order at the top level (the Python body is canonical_json(body) too)
	order := []string{`"products":`, `"published_by":`, `"schema":`, `"service_name":`, `"version":`}
	last := -1
	for _, k := range order {
		i := strings.Index(s, k)
		if i <= last {
			t.Fatalf("top-level key %s out of order in %s", k, s)
		}
		last = i
	}
	if !strings.Contains(s, `"price_per_hour":"0.000001"`) || strings.Contains(s, "\n") {
		t.Fatalf("money must be canonical strings on one line: %s", s)
	}
	html, _ := meter.CanonicalJSON(map[string]string{"k": "<&>"})
	if string(html) != `{"k":"<&>"}` {
		t.Fatalf("HTML escaping would change the hashed bytes: %s", html)
	}
}
