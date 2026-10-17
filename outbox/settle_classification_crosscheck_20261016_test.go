package outbox_test

// T-Q1 (ticket .private/tickets/20261016_test_hardening_findings): the settlement
// test double (httpSettler, settle_test.go) must classify billing's answers exactly
// as PRODUCTION does (quota/settler.go classifySettleError). Until v0.1.11 the double
// still voided 401/403/every 404 — the pre-v0.1.10 mapping — and treated statuses it
// did not list (402, 405, 410, ...) as a SUCCESS; its comment cited a cross-check test
// that did not exist.
//
// The cross-check is one table, testdata/settle_classification_20261016.json, driven
// twice over HTTP: here through the double, and in
// quota/settle_classification_crosscheck_20261016_test.go through the REAL billing
// client + billingSettler. Both must equal the table on every row, so production ==
// double. (outbox_test cannot reach quota's unexported classifier, hence the table.)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/observation"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

type settleClassRow struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
	Want   string `json:"want"`
}

func loadSettleClassTable(t *testing.T) []settleClassRow {
	t.Helper()
	b, err := os.ReadFile("testdata/settle_classification_20261016.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Rows []settleClassRow `json:"rows"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Rows) < 40 {
		t.Fatalf("the table has %d rows — it must cover every defined 4xx/5xx", len(f.Rows))
	}
	return f.Rows
}

func settleOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, outbox.ErrSettleRefused409):
		return "refused_409"
	case errors.Is(err, outbox.ErrSettlePermanent):
		return "permanent"
	case errors.Is(err, outbox.ErrSettleTransient):
		return "transient"
	}
	return "unclassified: " + err.Error()
}

func TestSettleDoubleMatchesTheProductionTable_20261016(t *testing.T) {
	obs, err := observation.Observe(observation.Event{OrgID: "o1", ResourceID: "r", ReservationID: "res-1",
		StartedAt: "2026-10-16T00:00:00Z", StoppedAt: "2026-10-16T01:00:00Z", HourlyRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	in := outbox.SettleRequest{OrgID: "o1", SettlementKey: "res-1", Observation: obs, ReservationID: "res-1"}
	for _, r := range loadSettleClassTable(t) {
		t.Run(fmt.Sprintf("%d %s", r.Status, r.Body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(r.Status)
				_, _ = w.Write([]byte(r.Body))
			}))
			url := srv.URL
			client := srv.Client()
			if r.Status == 0 { // no response at all
				srv.Close()
				client = &http.Client{Timeout: 2 * time.Second}
			} else {
				defer srv.Close()
			}
			_, _, err := httpSettler{baseURL: url, http: client}.SettleActivation(context.Background(), in)
			if got := settleOutcome(err); got != r.Want {
				t.Fatalf("double: HTTP %d %q → %s, production (the table) → %s", r.Status, r.Body, got, r.Want)
			}
		})
	}
}
