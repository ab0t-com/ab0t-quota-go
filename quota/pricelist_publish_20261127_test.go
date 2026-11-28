package quota

// v0.3.0 — Setup publishes the price list to billing in the BACKGROUND: a billing outage is retried
// with capped backoff until acknowledged, 404 / refusals are not retried, and Setup never waits.
// Same behaviour and capability strings as the Python library (price_list.run_price_list_publisher).
// RED before v0.3.0: runPriceListPublisher / preparePriceList do not exist.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
)

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readSetupSource(t *testing.T) string { return readFile(t, "setup.go") }

func httpStatusError(status int) error {
	return &httpx.Error{Status: status, URL: "http://billing.test"}
}

// fakePutter answers from a script: 0 = transport error, else an HTTP status (2xx = nil error).
type fakePutter struct {
	mu      sync.Mutex
	answers []int
	calls   int
	bodies  [][]byte
}

func (f *fakePutter) PublishPriceList(ctx context.Context, service string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bodies = append(f.bodies, append([]byte(nil), body...))
	a := f.answers[0]
	f.answers = f.answers[1:]
	switch {
	case a == 0:
		return errors.New("dial tcp: connection refused")
	case a >= 200 && a < 300:
		return nil
	default:
		return httpStatusError(a)
	}
}

type recorded struct {
	states   []string
	attempts int
	failures int
}

func (r *recorded) report(state string, attempts, failures int) {
	r.states = append(r.states, state)
	r.attempts, r.failures = attempts, failures
}

type sleeps struct{ calls []time.Duration }

func (s *sleeps) sleep(ctx context.Context, d time.Duration) bool {
	s.calls = append(s.calls, d)
	return true
}

func TestPriceListPublisher_BillingDownIsRetriedWithBackoffUntilAcknowledged_20261127(t *testing.T) {
	p, rec, sl := &fakePutter{answers: []int{503, 0, 502, 200}}, &recorded{}, &sleeps{}
	out := runPriceListPublisher(context.Background(), p, "acme-render", []byte(`{}`), "sha256:v", rec.report, sl.sleep)
	if out != "published" || p.calls != 4 {
		t.Fatalf("outcome %q calls %d", out, p.calls)
	}
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	if len(sl.calls) != 3 || sl.calls[0] != want[0] || sl.calls[1] != want[1] || sl.calls[2] != want[2] {
		t.Fatalf("sleeps %v want %v", sl.calls, want)
	}
	if last := rec.states[len(rec.states)-1]; last != "published(sha256:v, after 3 retries)" || rec.failures != 3 || rec.attempts != 4 {
		t.Fatalf("state %q attempts %d failures %d", last, rec.attempts, rec.failures)
	}
}

func TestPriceListPublisher_BackoffIsCapped_20261127(t *testing.T) {
	p, sl := &fakePutter{answers: []int{500, 500, 500, 500, 500, 500, 500, 200}}, &sleeps{}
	runPriceListPublisher(context.Background(), p, "s", []byte(`{}`), "v", (&recorded{}).report, sl.sleep)
	for i, d := range sl.calls {
		if d > 300*time.Second {
			t.Fatalf("sleep %d = %v exceeds the 300 s cap", i, d)
		}
	}
	if len(sl.calls) != 7 || sl.calls[6] != 300*time.Second {
		t.Fatalf("sleeps %v", sl.calls)
	}
}

func TestPriceListPublisher_404AndRefusalsAreNotRetried_20261127(t *testing.T) {
	for _, tc := range []struct {
		status int
		out    string
		state  string
	}{{404, "unsupported", "unsupported("}, {401, "refused", "refused(401)"}, {422, "refused", "refused(422)"}} {
		// v0.3.2: a 403 is no longer "never retried" — it is retried rarely (every 6 h, capped) with a
		// state that says what it means (never_astray_20261128_test.go). 401 keeps the old rule here.
		p, rec, sl := &fakePutter{answers: []int{tc.status, 200}}, &recorded{}, &sleeps{}
		if out := runPriceListPublisher(context.Background(), p, "s", []byte(`{}`), "v", rec.report, sl.sleep); out != tc.out {
			t.Errorf("%d → %q want %q", tc.status, out, tc.out)
		}
		if p.calls != 1 || len(sl.calls) != 0 || !strings.HasPrefix(rec.states[len(rec.states)-1], tc.state) {
			t.Errorf("%d: calls %d sleeps %v state %v", tc.status, p.calls, sl.calls, rec.states)
		}
	}
}

// CONTROL: a retryable answer does retry.
func TestPriceListPublisher_RetryableAnswerRetries_20261127(t *testing.T) {
	p := &fakePutter{answers: []int{503, 200}}
	runPriceListPublisher(context.Background(), p, "s", []byte(`{}`), "v", (&recorded{}).report, (&sleeps{}).sleep)
	if p.calls != 2 {
		t.Fatalf("calls %d", p.calls)
	}
}

func TestPriceListPublisher_StopsWhenClosed_20261127(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &fakePutter{answers: []int{503, 503}}
	if out := runPriceListPublisher(ctx, p, "s", []byte(`{}`), "v", (&recorded{}).report, sleepCtx); out != "" || p.calls != 1 {
		t.Fatalf("after Close: outcome %q calls %d", out, p.calls)
	}
}

func priceCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{ServiceName: "acme-render", Pricing: json.RawMessage(
		`{"products":{"render_job":{"variants":{"cpu":{"price_per_hour":"0.5","allocation_price":"0","default":true}}}}}`)}
}

func TestPreparePriceList_OffReasonsMatchThePythonLibrary_20261127(t *testing.T) {
	t.Setenv("AB0T_SERVICE_NAME", "")
	f := false
	for _, tc := range []struct {
		name                string
		mut                 func(c *config.Config)
		billing, paid, skip bool
		want                string
	}{
		{"no products", func(c *config.Config) { c.Pricing = nil }, true, true, false, "off(no pricing.products declared)"},
		{"switched off", func(c *config.Config) { c.BillingIntegration.PriceList = &f }, true, true, false, "off(config billing_integration.price_list=false)"},
		{"not paid", func(c *config.Config) {}, true, false, false, "off(paid mode disabled)"},
		{"smoke run", func(c *config.Config) {}, true, true, true, "off(smoke run: SkipBackgroundLoops)"},
		{"no service", func(c *config.Config) { c.ServiceName = "" }, true, true, false, "off(no service name: set service_name or AB0T_SERVICE_NAME)"},
		{"no billing", func(c *config.Config) {}, false, true, false, "off(no billing key)"},
		{"invalid prices", func(c *config.Config) {
			c.Pricing = json.RawMessage(`{"products":{"render_job":{"variants":{"cpu":{"price_per_hour":"0"}}}}}`)
		}, true, true, false, "not_published(invalid pricing — see the log)"},
	} {
		cfg := priceCfg(t)
		tc.mut(cfg)
		body, _, _, state := preparePriceList(cfg, tc.billing, tc.paid, tc.skip)
		if body != nil || state != tc.want {
			t.Errorf("%s: body %v state %q want %q", tc.name, body != nil, state, tc.want)
		}
	}
	// CONTROL: everything present → a body and "publishing(<version>)"
	body, service, version, state := preparePriceList(priceCfg(t), true, true, false)
	if body == nil || service != "acme-render" || !strings.HasPrefix(version, "sha256:") || state != "publishing("+version+")" {
		t.Fatalf("publish case: body %v service %q version %q state %q", body != nil, service, version, state)
	}
}

func TestSetup_PublishesAfterCapabilitiesAndNeverWaits_20261127(t *testing.T) {
	src := readSetupSource(t)
	i := strings.Index(src, "preparePriceList(cfg, q.Billing != nil, enablePaid, opts.SkipBackgroundLoops)")
	j := strings.Index(src, "q.startPriceListPublisher(q.Billing, plService, plBody, plVersion)")
	k := strings.Index(src, "q.capability = cap\n\tq.capMu.Unlock()\n\tlogCapabilities(cap)\n\tif plBody != nil {")
	if i < 0 || j < 0 || k < 0 || !(i < k && k < j) {
		t.Fatalf("Setup must decide, publish the capabilities, then start the background publisher (i=%d k=%d j=%d)", i, k, j)
	}
	if !strings.Contains(readFile(t, "pricelist_publish.go"), "go runPriceListPublisher(") {
		t.Fatal("the publisher must run in a goroutine")
	}
}

// v0.3.0 — the member-joined gift handler is registered ONLY when the company opted in, and the
// sign-up handler gets the same switch (one gift per person across both events).
func TestSetup_RegistersTheMemberJoinedHandlerOnlyWhenOptedIn_20261127(t *testing.T) {
	src := readSetupSource(t)
	i := strings.Index(src, "MemberJoinedEnabled: cfg.AuthEvents.GrantOnMemberJoined,")
	j := strings.Index(src, "if cfg.AuthEvents.GrantOnMemberJoined {\n\t\t\tmh, err := authevents.RegisterMemberJoinedCreditGrantHandler(grantDeps)")
	k := strings.Index(src, "authevents.UnregisterHandler(authevents.EventMemberJoined, mh)")
	if i < 0 || j < 0 || k < 0 || !(i < j && j < k) {
		t.Fatalf("member-joined wiring (i=%d j=%d k=%d)", i, j, k)
	}
}
