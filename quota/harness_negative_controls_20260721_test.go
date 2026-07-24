package quota

// NC-8 — permanent negative control (pack 20260721, harness design §1.3).
// Feeds the declared-store contract helper a stub FROZEN at today's shipped
// defect ("" / null / absent redis_url ⇒ silent in-memory counter, GO-01)
// and asserts the helper CATCHES it. A harness that cannot fail proves
// nothing; this runs in every CI pass, forever.

import (
	"context"
	"fmt"
	"testing"
)

// recordingTB captures Errorf/Fatalf so a deliberate contract violation can
// be asserted CAUGHT without failing the real test run.
type recordingTB struct {
	testing.TB
	failed bool
	logs   []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.failed = true
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

// brokenSetup freezes the PRE-T-G1 shipped behaviour as a stub: an
// undeclared store (absent or null redis_url) silently becomes an in-memory,
// per-process counter and Setup reports success. It is never wired into
// production code — it exists so NC-8 can prove the contract helper fails
// when handed the defect this program removed (setup.go:245 as of GO-01).
func brokenSetup(_ context.Context, opts Options) (*Quota, error) {
	if url, ok := opts.ConfigOverride.Storage.RedisURL.Get(); ok && url != "" {
		return nil, fmt.Errorf("brokenSetup stub models only the ''-means-memory defect, got %q", url)
	}
	return &Quota{
		Cfg: opts.ConfigOverride,
		capability: Capabilities{
			Engine:        true,
			FloatStore:    "memory", // the silent downgrade, frozen
			RedisTopology: TopologyNA,
			WhyOff:        map[string]string{},
		},
	}, nil
}

func TestNC8_ContractHelperCatchesBrokenSetup(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")
	rec := &recordingTB{TB: t}
	assertDeclaredStoreContract(rec, brokenSetup)
	if !rec.failed {
		t.Fatal("NC-8: the declared-store contract helper PASSED today's shipped defect " +
			"('' ⇒ silent in-memory) — the harness cannot catch the failure it exists to catch")
	}
	t.Logf("NC-8: helper correctly caught brokenSetup with %d finding(s); first: %s",
		len(rec.logs), rec.logs[0])
}
