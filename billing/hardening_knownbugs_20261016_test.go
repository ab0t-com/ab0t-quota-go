package billing

// Known-bug tests (2026-10-16) — FIXED in v0.1.11, so they always run now (the
// QUOTA_RUN_KNOWN_BUGS gate is gone). Each FAILED on v0.1.10.
// Ticket: .private/tickets/20261016_test_hardening_findings/TICKET.md (Q-3)

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/shopspring/decimal"
)

// Q-3: seg() was url.PathEscape, which leaves "." and ".." untouched, so an org id
// of ".." produced /billing/../promotional-credit — a path any normalising proxy
// (and RFC 3986 §5.2.4) resolves to /promotional-credit. v0.1.11 refuses a blank,
// "." or ".." id before any request is sent.
func TestKnownBug_DotSegmentOrgIDEscapesItsSegment(t *testing.T) {
	c, f := moneyClient(t)
	for _, org := range []string{"..", "."} {
		f.mu.Lock()
		before := len(f.paths)
		f.mu.Unlock()
		_, err := c.GrantPromotionalCredit(context.Background(), org, PromotionalCreditRequest{Amount: decimal.NewFromInt(1), IdempotencyKey: "k" + org})
		f.mu.Lock()
		sent := f.paths[before:]
		f.mu.Unlock()
		for _, p := range sent {
			segs := strings.Split(strings.Trim(p, "/"), "/")
			if len(segs) > 1 && (segs[1] == ".." || segs[1] == ".") {
				t.Errorf("org id %q sent as path %q — a dot segment addresses another route after normalisation", org, p)
			}
		}
		if len(sent) != 0 || !errors.Is(err, httpx.ErrBadPathSegment) {
			t.Errorf("org id %q: err=%v, %d request(s) sent — want ErrBadPathSegment and nothing sent", org, err, len(sent))
		}
	}
}

// Every billing method that puts an id in the path refuses a blank, "." or ".."
// id with ErrBadPathSegment and sends nothing (RecordUsage is best-effort: it
// returns nil, but still sends nothing).
func TestEveryPathIDRefusesDotSegments_20261016(t *testing.T) {
	c, f := moneyClient(t)
	ctx := context.Background()
	one := decimal.NewFromInt(1)
	for _, id := range []string{"", " ", ".", "..", " .. "} {
		calls := map[string]func() error{
			"GetUsageSummary": func() error { _, err := c.GetUsageSummary(ctx, id); return err },
			"GrantPromotionalCredit": func() error {
				_, err := c.GrantPromotionalCredit(ctx, id, PromotionalCreditRequest{Amount: one, IdempotencyKey: "k"})
				return err
			},
			"ApplyCreditGrant": func() error {
				_, err := c.ApplyCreditGrant(ctx, id, ApplyCreditGrantRequest{Amount: one, IdempotencyKey: "k",
					Destination: "credit_balance", Lifecycle: "persistent"})
				return err
			},
			"GetSubscription":        func() error { _, err := c.GetSubscription(ctx, id); return err },
			"CancelSubscription/org": func() error { return c.CancelSubscription(ctx, id, "sub-1") },
			"CancelSubscription/sub": func() error { return c.CancelSubscription(ctx, "o1", id) },
			"SettleActivation": func() error {
				_, err := c.SettleActivation(ctx, id, SettleActivationRequest{SettlementKey: "r1"})
				return err
			},
		}
		for name, call := range calls {
			f.mu.Lock()
			before := len(f.paths)
			f.mu.Unlock()
			err := call()
			f.mu.Lock()
			sent := len(f.paths) - before
			f.mu.Unlock()
			// GrantPromotionalCredit / ApplyCreditGrant refuse a BLANK org with their
			// older "org id is required" message first; both are refusals.
			blankGrant := strings.TrimSpace(id) == "" && strings.Contains(name, "Credit")
			if sent != 0 || err == nil || (!blankGrant && !errors.Is(err, httpx.ErrBadPathSegment)) {
				t.Errorf("%s(%q): err=%v, %d request(s) sent — want a refusal and nothing sent", name, id, err, sent)
			}
		}
		f.mu.Lock()
		before := len(f.paths)
		f.mu.Unlock()
		if err := c.RecordUsage(ctx, RecordUsageRequest{OrgID: id}); err != nil {
			t.Errorf("RecordUsage(%q) returned %v; it is best-effort and returns nil", id, err)
		}
		f.mu.Lock()
		if n := len(f.paths) - before; n != 0 {
			t.Errorf("RecordUsage(%q) sent %d request(s); want none", id, n)
		}
		f.mu.Unlock()
	}
}
