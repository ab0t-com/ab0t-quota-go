package quota

// v0.3.0 — publish this service's price list (pricing.products) to billing at startup, in the
// BACKGROUND (Setup never waits for billing). Same rules and outcomes as the Python library
// (ab0t_quota.price_list): 2xx done; 5xx / 408 / 429 / transport error retried with capped backoff
// (30, 60, 120, 300 s, then every 300 s) until acknowledged or Close; 404 = billing has no route yet
// (logged once, no retry); other 4xx = refused (logged once, no retry). Visible as
// Capabilities().PriceList (+ PriceListAttempts / PriceListFailures). Off switch:
// "billing_integration": {"price_list": false}.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/meter"
)

// PriceListRetryBackoff is the wait between publish attempts (the last entry repeats). A variable
// so tests can shrink it.
var PriceListRetryBackoff = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second}

// priceListPutter is the one call the publisher needs (*billing.Client implements it).
type priceListPutter interface {
	PublishPriceList(ctx context.Context, serviceName string, body []byte) error
}

// priceListServiceName: AB0T_SERVICE_NAME, else service_name, else the first resource's service —
// the Python library's order (setup._resolve_service_name).
func priceListServiceName(cfg *config.Config) string {
	if v := strings.TrimSpace(os.Getenv("AB0T_SERVICE_NAME")); v != "" {
		return v
	}
	if cfg.ServiceName != "" {
		return cfg.ServiceName
	}
	if len(cfg.Resources) > 0 {
		return cfg.Resources[0].Service
	}
	return ""
}

// pricingDeclared reports whether the config declares a non-empty pricing.products object.
func pricingDeclared(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return false
	}
	var doc struct {
		Products map[string]json.RawMessage `json:"products"`
	}
	return json.Unmarshal(t, &doc) == nil && len(doc.Products) > 0
}

// libraryVersion is this module's version as the consumer's build recorded it ("(devel)" in-tree).
func libraryVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/ab0t-com/ab0t-quota-go" {
				return strings.TrimPrefix(d.Version, "v")
			}
		}
		if bi.Main.Path == "github.com/ab0t-com/ab0t-quota-go" && bi.Main.Version != "" {
			return strings.TrimPrefix(bi.Main.Version, "v")
		}
	}
	return "unknown"
}

// preparePriceList decides whether to publish and builds the canonical body. It never fails Setup:
// every "no" is a capability string (the same strings as the Python library where they apply).
func preparePriceList(cfg *config.Config, billingWired, enablePaid, skipLoops bool) (body []byte, service, version, state string) {
	switch {
	case !pricingDeclared(cfg.Pricing):
		return nil, "", "", "off(no pricing.products declared)"
	case !cfg.BillingIntegration.PriceListEnabled():
		return nil, "", "", "off(config billing_integration.price_list=false)"
	case !enablePaid:
		return nil, "", "", "off(paid mode disabled)"
	case skipLoops:
		return nil, "", "", "off(smoke run: SkipBackgroundLoops)"
	}
	service = priceListServiceName(cfg)
	if service == "" {
		slog.Warn("price list not published: no service name (set service_name in quota-config.json)")
		return nil, "", "", "off(no service name: set service_name or AB0T_SERVICE_NAME)"
	}
	if !billingWired {
		return nil, "", "", "off(no billing key)"
	}
	pl, err := meter.BuildPriceList(service, cfg.Pricing, "ab0t-quota-go/"+libraryVersion())
	if err != nil {
		slog.Error("price list NOT published", "service", service, "err", err)
		return nil, "", "", "not_published(invalid pricing — see the log)"
	}
	b, err := meter.CanonicalJSON(pl)
	if err != nil {
		slog.Error("price list NOT published: encode", "service", service, "err", err)
		return nil, "", "", "not_published(invalid pricing — see the log)"
	}
	return b, service, pl.Version, "publishing(" + pl.Version + ")"
}

// priceListReport receives the publisher's progress (state, attempts, failures).
type priceListReport func(state string, attempts, failures int)

// runPriceListPublisher publishes once and keeps retrying a retryable answer until acknowledged or
// ctx is done. Returns the final outcome ("" when ctx ended first).
func runPriceListPublisher(ctx context.Context, putter priceListPutter, service string, body []byte,
	version string, report priceListReport, sleep func(context.Context, time.Duration) bool) string {
	attempts, failures := 0, 0
	for {
		attempts++
		outcome, status := billing.PriceListOutcome(putter.PublishPriceList(ctx, service, body))
		switch outcome {
		case billing.PriceListPublished:
			state := "published(" + version + ")"
			if attempts > 1 {
				state = fmt.Sprintf("published(%s, after %d retries)", version, attempts-1)
			}
			report(state, attempts, failures)
			slog.Info("price list published", "service", service, "version", version, "attempt", attempts)
			return outcome
		case billing.PriceListUnsupported:
			failures++
			report("unsupported(404: this billing has no price-list route)", attempts, failures)
			slog.Warn("price list NOT published: billing answered 404 (no price-list route yet) — not retried; "+
				"usage is billed at the rates your events send", "service", service)
			return outcome
		case billing.PriceListRefused:
			failures++
			report(fmt.Sprintf("refused(%d)", status), attempts, failures)
			slog.Error("price list REFUSED by billing — not retried (a retry would be refused the same way); "+
				"check the service's billing key and its permissions", "service", service, "status", status)
			return outcome
		}
		failures++
		i := attempts - 1
		if i >= len(PriceListRetryBackoff) {
			i = len(PriceListRetryBackoff) - 1
		}
		delay := PriceListRetryBackoff[i]
		report(fmt.Sprintf("retrying(attempt %d)", attempts), attempts, failures)
		slog.Warn("price list not yet published — billing cannot check usage rates for this service until it lands",
			"service", service, "attempt", attempts, "status", status, "next_retry", delay)
		if !sleep(ctx, delay) {
			return ""
		}
	}
}

// sleepCtx waits d or until ctx is done (false = ctx done).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// startPriceListPublisher runs the publisher in a goroutine; Close stops it.
func (q *Quota) startPriceListPublisher(putter priceListPutter, service string, body []byte, version string) {
	ctx, cancel := context.WithCancel(context.Background())
	q.closeFns = append(q.closeFns, func() error { cancel(); return nil })
	go runPriceListPublisher(ctx, putter, service, body, version, func(state string, attempts, failures int) {
		q.capMu.Lock()
		q.capability.PriceList, q.capability.PriceListAttempts, q.capability.PriceListFailures = state, attempts, failures
		q.capMu.Unlock()
	}, sleepCtx)
}
