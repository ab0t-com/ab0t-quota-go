package quota

// T-Q1 (ticket .private/tickets/20261016_test_hardening_findings): the cross-check
// that outbox/settle_test.go's comment promised and that did not exist. PRODUCTION —
// the real billing.Client + billingSettler (classifySettleError), over real HTTP — is
// driven over the SAME table the outbox test double is driven over
// (outbox/settle_classification_crosscheck_20261016_test.go). Both must equal the
// table on every row, so production == double.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/observation"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

func TestSettleProductionMatchesTheSharedTable_20261016(t *testing.T) {
	b, err := os.ReadFile("../outbox/testdata/settle_classification_20261016.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Rows []struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
			Want   string `json:"want"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) < 40 {
		t.Fatalf("the table has %d rows — it must cover every defined 4xx/5xx", len(table.Rows))
	}
	// Every status Go defines in 400..599 has a row (a new status is not silently uncovered).
	have := map[int]bool{}
	for _, r := range table.Rows {
		have[r.Status] = true
	}
	for s := 400; s <= 599; s++ {
		if http.StatusText(s) != "" && !have[s] {
			t.Errorf("status %d (%s) has no row in the shared table", s, http.StatusText(s))
		}
	}

	obs, err := observation.Observe(observation.Event{OrgID: "o1", ResourceID: "r", ReservationID: "res-1",
		StartedAt: "2026-10-16T00:00:00Z", StoppedAt: "2026-10-16T01:00:00Z", HourlyRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	in := outbox.SettleRequest{OrgID: "o1", SettlementKey: "res-1", Observation: obs, ReservationID: "res-1"}
	for _, r := range table.Rows {
		t.Run(fmt.Sprintf("%d %s", r.Status, r.Body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(r.Status)
				_, _ = w.Write([]byte(r.Body))
			}))
			url := srv.URL
			if r.Status == 0 { // no response at all
				srv.Close()
			} else {
				defer srv.Close()
			}
			bc, err := billing.New(mesh.URLs{Billing: url, BillingToken: "ab0t_sk_x"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = billingSettler{c: bc}.SettleActivation(context.Background(), in)
			got := "unclassified"
			switch {
			case err == nil:
				got = "success"
			case errors.Is(err, outbox.ErrSettleRefused409):
				got = "refused_409"
			case errors.Is(err, outbox.ErrSettlePermanent):
				got = "permanent"
			case errors.Is(err, outbox.ErrSettleTransient):
				got = "transient"
			}
			if got != r.Want {
				t.Fatalf("production: HTTP %d %q → %s (%v), table → %s", r.Status, r.Body, got, err, r.Want)
			}
		})
	}
}
