package quota

// Ticket 20261016 review B-04 (lane B, re-verified): the settlement fallback
// classified 401, 403 and EVERY 404 as permanent, and a permanent outcome VOIDS
// the money event — irreversibly. A rotated/expired billing key (401), a
// permission change (403), or a wrong base URL / proxy path (FastAPI's route
// 404 {"detail":"Not Found"}) are configuration faults that heal when fixed; they
// must keep the event PENDING (retry), never void it. Only billing's real
// "no billing account for this org" 404 (app/core/reservation.py:1629-1631) is
// permanent. RED on v0.1.9.

import (
	"errors"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

func TestClassifySettleError_ConfigFaultsRetryNeverVoid_20261016(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"detail":"Not authenticated"}`, outbox.ErrSettleTransient},
		{403, `{"detail":"Insufficient permissions"}`, outbox.ErrSettleTransient},
		{404, `{"detail":"Not Found"}`, outbox.ErrSettleTransient},                 // route / proxy path
		{404, `{"detail":"Billing account not found"}`, outbox.ErrSettlePermanent}, // billing's real answer
		{400, `{"detail":"Settlement cost cannot be negative"}`, outbox.ErrSettlePermanent},
		{422, `{"detail":"validation error"}`, outbox.ErrSettlePermanent},
		{409, `{"detail":"not eligible"}`, outbox.ErrSettleRefused409},
		{503, ``, outbox.ErrSettleTransient},
	}
	for _, c := range cases {
		got := classifySettleError(&httpx.Error{Status: c.status, Body: c.body})
		if !errors.Is(got, c.want) {
			t.Errorf("HTTP %d %s → %v, want %v", c.status, c.body, got, c.want)
		}
	}
}
