// Package mesh resolves service URLs from env vars. Lives between
// consumer code and the billing/payment clients.
//
// v0.2.0: the canonical names are the ones `authsetup connect billing` /
// `authsetup connect payment` print — BILLING_SERVICE_URL / BILLING_SERVICE_API_KEY
// and PAYMENT_SERVICE_URL / PAYMENT_SERVICE_API_KEY — the same family in every
// ab0t-quota SDK. The pre-v0.2.0 per-service AB0T_QUOTA_* names still work as a
// fallback (the canonical name wins when both are set) and log ONE deprecation
// warning per process naming every old name in use. AB0T_QUOTA_SERVICE_TOKEN stays
// a supported last fallback for both keys (not deprecated — the Python SDK keeps its
// shared mesh key the same way).
package mesh

import (
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
)

// Canonical env-var names (v0.2.0) — what `authsetup connect <provider>` prints.
const (
	EnvBillingServiceURL    = "BILLING_SERVICE_URL"
	EnvBillingServiceAPIKey = "BILLING_SERVICE_API_KEY"
	EnvPaymentServiceURL    = "PAYMENT_SERVICE_URL"
	EnvPaymentServiceAPIKey = "PAYMENT_SERVICE_API_KEY"
)

// Deprecated env-var names (read as a fallback since v0.2.0; prefer the canonical names above).
const (
	// Deprecated: use EnvBillingServiceURL.
	EnvBillingURL = "AB0T_QUOTA_BILLING_URL"
	// Deprecated: use EnvPaymentServiceURL.
	EnvPaymentURL = "AB0T_QUOTA_PAYMENT_URL"
	// EnvServiceTok is the shared last-fallback key for billing AND payment (not deprecated).
	// Prefer EnvBillingServiceAPIKey / EnvPaymentServiceAPIKey: the mesh mints a SEPARATE
	// X-API-Key per provider org — billing's key is not valid at payment and vice-versa.
	EnvServiceTok = "AB0T_QUOTA_SERVICE_TOKEN"
	// Deprecated: use EnvBillingServiceAPIKey.
	EnvBillingTok = "AB0T_QUOTA_BILLING_TOKEN"
	// Deprecated: use EnvPaymentServiceAPIKey.
	EnvPaymentTok = "AB0T_QUOTA_PAYMENT_TOKEN"
)

// URLs is the resolved mesh-side URLs.
type URLs struct {
	Billing string
	Payment string
	Token   string // shared/fallback token (AB0T_QUOTA_SERVICE_TOKEN)
	// Per-service tokens; each falls back to Token. The billing/payment clients
	// use these so a per-org mesh key reaches the right provider.
	BillingToken string
	PaymentToken string
	// BillingSource / PaymentSource (v0.2.0) name the env var each URL was read from ("" when
	// unset) — for capability reports and error messages.
	BillingSource string
	PaymentSource string
}

// ErrMissing is returned when a required URL isn't set.
var ErrMissing = errors.New("mesh: required URL env var not set")

// Resolve reads URLs from env. Both billing and payment are optional —
// quota.Setup turns capabilities off when they're absent.
//
// Precedence (v0.2.0; the first non-empty value wins): URL = the canonical *_SERVICE_URL, else the
// deprecated AB0T_QUOTA_*_URL; key = the canonical *_SERVICE_API_KEY, else the deprecated
// AB0T_QUOTA_*_TOKEN, else the shared AB0T_QUOTA_SERVICE_TOKEN. A deprecated name that supplies a
// value is reported in a deprecation warning; so is one that is set to a DIFFERENT value than the
// canonical name that won (two keys for one service is almost always a leftover). Each such name is
// reported ONCE per process — the Python SDK's cadence (see warnDeprecated).
func Resolve() URLs {
	var used []string
	pick := func(canonical, deprecated string) (string, string) {
		cv, dv := os.Getenv(canonical), os.Getenv(deprecated)
		switch {
		case cv != "":
			if dv != "" && dv != cv {
				used = append(used, deprecated+" (ignored: "+canonical+" is set to a different value)")
			}
			return cv, canonical
		case dv != "":
			used = append(used, deprecated+" (use "+canonical+")")
			return dv, deprecated
		}
		return "", ""
	}
	shared := os.Getenv(EnvServiceTok)
	billingURL, billingSrc := pick(EnvBillingServiceURL, EnvBillingURL)
	paymentURL, paymentSrc := pick(EnvPaymentServiceURL, EnvPaymentURL)
	billingTok, _ := pick(EnvBillingServiceAPIKey, EnvBillingTok)
	paymentTok, _ := pick(EnvPaymentServiceAPIKey, EnvPaymentTok)
	warnDeprecated(used)
	return URLs{
		Billing:       strings.TrimRight(billingURL, "/"),
		Payment:       strings.TrimRight(paymentURL, "/"),
		Token:         shared,
		BillingToken:  firstNonEmpty(billingTok, shared),
		PaymentToken:  firstNonEmpty(paymentTok, shared),
		BillingSource: billingSrc,
		PaymentSource: paymentSrc,
	}
}

var (
	deprecationMu sync.Mutex
	// deprecationLogged is false until the first report in this process; while it is false the
	// reported set below starts empty (so resetting it re-arms every name).
	deprecationLogged bool
	// deprecationWarned (v0.2.0) holds every deprecated-name entry already reported.
	deprecationWarned map[string]bool
)

// warnDeprecated reports each deprecated name ONCE per process — the Python SDK's cadence (one
// warning per name). A Resolve that sees names not reported yet logs ONE line naming exactly those;
// a Resolve that sees only reported names logs nothing; a deprecated name that appears later (e.g. a
// second service's old variable set after startup) is still reported, once. (Before v0.2.0's
// unification: one line per process in total, so a name that appeared later was never reported.)
func warnDeprecated(used []string) {
	if len(used) == 0 {
		return
	}
	deprecationMu.Lock()
	defer deprecationMu.Unlock()
	if !deprecationLogged || deprecationWarned == nil {
		deprecationWarned = map[string]bool{}
	}
	uniq := map[string]bool{}
	for _, u := range used {
		if !deprecationWarned[u] {
			uniq[u] = true
		}
	}
	if len(uniq) == 0 {
		return
	}
	deprecationLogged = true
	names := make([]string, 0, len(uniq))
	for u := range uniq {
		deprecationWarned[u] = true
		names = append(names, u)
	}
	sort.Strings(names)
	slog.Warn("ab0t-quota: deprecated env var names in use — rename them to the names `authsetup connect` prints "+
		"("+EnvBillingServiceURL+", "+EnvBillingServiceAPIKey+", "+EnvPaymentServiceURL+", "+EnvPaymentServiceAPIKey+"); "+
		"the old names still work for now", "deprecated", strings.Join(names, "; "))
}

// firstNonEmpty returns the first non-empty string (per-service token, then the
// shared fallback).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
