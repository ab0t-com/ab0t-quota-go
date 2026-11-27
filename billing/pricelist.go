package billing

import (
	"context"
	"errors"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
)

// What one price-list publish answer means (v0.3.0) — the same outcomes as the Python library's
// price_list.classify_status; shared vectors testdata/price_list_golden_20261127.json.
const (
	PriceListPublished   = "published"   // 2xx: billing has it
	PriceListRetry       = "retry"       // 5xx, 408, 429 or a transport error: billing is down — try later
	PriceListUnsupported = "unsupported" // 404: this billing has no price-list route yet (do not retry)
	PriceListRefused     = "refused"     // any other 4xx (do not retry: it would be refused the same way)
)

// ClassifyPriceListStatus maps an HTTP status to a price-list publish outcome.
func ClassifyPriceListStatus(status int) string {
	switch {
	case status >= 200 && status < 300:
		return PriceListPublished
	case status == 404:
		return PriceListUnsupported
	case status >= 500 || status == 429 || status == 408:
		return PriceListRetry
	default:
		return PriceListRefused
	}
}

// PublishPriceList (v0.3.0) PUTs a price list to /billing/price-list/{service_name} with the
// service's billing key. body is the canonical JSON meter.BuildPriceList + meter.CanonicalJSON
// produce, sent byte-for-byte. Use PriceListOutcome on the error.
func (c *Client) PublishPriceList(ctx context.Context, serviceName string, body []byte) error {
	svc, err := seg("PublishPriceList", "service name", serviceName)
	if err != nil {
		return err
	}
	return c.http.PUTRaw(ctx, "/billing/price-list/"+svc, body, nil)
}

// PriceListOutcome classifies PublishPriceList's result: nil → published; an HTTP error → by its
// status; anything else (timeout, connection refused) → retry. status is 0 when there was no answer.
func PriceListOutcome(err error) (outcome string, status int) {
	if err == nil {
		return PriceListPublished, 0
	}
	var he *httpx.Error
	if errors.As(err, &he) {
		return ClassifyPriceListStatus(he.Status), he.Status
	}
	return PriceListRetry, 0
}
