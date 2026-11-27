package billing

// Typed commit (v0.2.0, additive). Same contract as the Python client's
// commit_reservation_result / CommitOutcome: the classification table entry for entry, the local
// validation (record_debt only on the billing-priced form, refused before any network call), the
// wire body, and "a billing answer or a transport failure is a result, never an error".

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

type commitSrv struct {
	hits   int32
	path   string
	body   map[string]any
	status int
	reply  string
	noBody bool
}

func newCommitClient(t *testing.T, s *commitSrv) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.hits, 1)
		s.path = r.Method + " " + r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		s.body = map[string]any{}
		_ = json.Unmarshal(raw, &s.body)
		if s.status == 0 {
			s.status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		if !s.noBody {
			_, _ = io.WriteString(w, s.reply)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var (
	cStart = time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	cStop  = time.Date(2026, 11, 23, 7, 30, 0, 0, time.UTC)
)

func priced() CommitRequest {
	rate := decimal.RequireFromString("0.25")
	fee := decimal.RequireFromString("0.01")
	return CommitRequest{StartedAt: cStart, StoppedAt: cStop, HourlyRate: &rate, AllocationFee: &fee}
}

// Every row of the Python classifier, plus the status-only rules (402, >= 500) and the FAILED tail.
func TestClassifyCommitError_PythonTable_20261123(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   CommitOutcome
	}{
		{409, "RESERVATION_ALREADY_SETTLED", CommitAlreadySettled},
		{409, "RESERVATION_NOT_ACTIVE", CommitNotActive},
		{409, "CONCURRENT_MODIFICATION", CommitConflict},
		{409, "RESERVATION_COMMIT_CONFLICT", CommitConflict},
		{404, "RESERVATION_NOT_FOUND", CommitNotFound},
		{404, "", CommitNotFound}, // billing's uncoded ownership 404
		{402, "", CommitInsufficientFunds},
		{402, "INSUFFICIENT_BALANCE", CommitInsufficientFunds},
		{500, "", CommitUnavailable},
		{503, "RESERVATION_NOT_FOUND", CommitUnavailable}, // >= 500 wins over any code
		{504, "", CommitUnavailable},
		{409, "", CommitFailed}, // an uncoded 409 is not a conflict
		{409, "SOMETHING_NEW", CommitFailed},
		{404, "ORG_NOT_FOUND", CommitFailed}, // a coded 404 that is not the reservation's
		{400, "", CommitFailed},
		{401, "", CommitFailed},
		{403, "", CommitFailed},
		{422, "RECORD_DEBT_REQUIRES_BILLING_PRICING", CommitFailed},
	}
	for _, c := range cases {
		if got := ClassifyCommitError(c.status, c.code); got != c.want {
			t.Errorf("ClassifyCommitError(%d, %q) = %s, want %s", c.status, c.code, got, c.want)
		}
	}
	if len(commitCodeOutcomes) != 6 {
		t.Fatalf("the code table has %d rows; the Python table has 6 — keep them identical", len(commitCodeOutcomes))
	}
}

// The helpers' truth table is the Python properties', outcome for outcome.
func TestCommitOutcomeHelpers_20261123(t *testing.T) {
	type row struct{ accounted, holdMayRemain, retryable bool }
	want := map[CommitOutcome]row{
		CommitCommitted:         {true, false, false},
		CommitRecordedDebt:      {true, false, false},
		CommitAlreadySettled:    {true, false, false},
		CommitInsufficientFunds: {false, true, false},
		CommitNotActive:         {false, false, false},
		CommitNotFound:          {false, false, false},
		CommitConflict:          {false, true, true},
		CommitUnavailable:       {false, true, true},
		CommitFailed:            {false, true, false},
	}
	for o, w := range want {
		got := row{o.Accounted(), o.HoldMayRemain(), o.Retryable()}
		if got != w {
			t.Errorf("%s: (accounted, hold_may_remain, retryable) = %v, want %v", o, got, w)
		}
		r := &CommitResult{Outcome: o}
		if r.Accounted() != w.accounted || r.HoldMayRemain() != w.holdMayRemain || r.Retryable() != w.retryable {
			t.Errorf("%s: CommitResult helpers disagree with the outcome's", o)
		}
	}
	var nilRes *CommitResult
	if nilRes.Accounted() || !nilRes.HoldMayRemain() || nilRes.Retryable() {
		t.Error("a nil result must read as 'not accounted, a hold may remain'")
	}
}

func TestCommitReservationResult_BillingPriced_Committed_20261123(t *testing.T) {
	t.Setenv("AB0T_SERVICE_NAME", "")
	s := &commitSrv{reply: `{"status":"committed","reservation_id":"res-1","reserved_amount":"0.26","actual_cost":"0.385","refunded_amount":"0","message":"ok"}`}
	c := newCommitClient(t, s)
	r, err := c.CommitReservationResult(context.Background(), "org-1", "res-1", priced())
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != CommitCommitted || !r.Accounted() || r.HoldMayRemain() {
		t.Fatalf("got %+v", r)
	}
	if !r.ActualCost.Equal(decimal.RequireFromString("0.385")) || !r.ReservedAmount.Equal(decimal.RequireFromString("0.26")) {
		t.Fatalf("money: %+v", r)
	}
	if r.UnfundedOverrun != nil || r.NewBalance != nil || r.CommitMode != "" {
		t.Fatalf("absent fields must stay nil/empty, never 0: %+v", r)
	}
	if s.path != "POST /billing/org-1/commit" {
		t.Fatalf("path %q", s.path)
	}
	b := s.body
	if b["reservation_id"] != "res-1" || b["started_at"] != "2026-11-23T06:00:00.000000Z" ||
		b["stopped_at"] != "2026-11-23T07:30:00.000000Z" || b["hourly_rate"] != "0.25" || b["allocation_fee"] != "0.01" {
		t.Fatalf("body %v", b)
	}
	if _, ok := b["on_insufficient_funds"]; ok {
		t.Fatalf("on_insufficient_funds must be absent when not chosen (billing's default): %v", b)
	}
	usage, _ := b["actual_usage"].(map[string]any)
	if usage["cost"] != "0" || usage["platform_fee"] != "0" || usage["tool_id"] != "default" ||
		usage["org_id"] != "org-1" || usage["reservation_id"] != "res-1" {
		t.Fatalf("billing-priced commit must send the zero-cost usage-record shape: %v", usage)
	}
}

func TestCommitReservationResult_ToolIDFromEnvAndInput_20261123(t *testing.T) {
	t.Setenv("AB0T_SERVICE_NAME", "svc-env")
	body, err := BuildCommitBody("org-1", "res-1", priced())
	if err != nil {
		t.Fatal(err)
	}
	if body["actual_usage"].(map[string]any)["tool_id"] != "svc-env" {
		t.Fatalf("tool_id should fall back to AB0T_SERVICE_NAME: %v", body)
	}
	in := priced()
	in.ToolID = "svc-explicit"
	body, _ = BuildCommitBody("org-1", "res-1", in)
	if body["actual_usage"].(map[string]any)["tool_id"] != "svc-explicit" {
		t.Fatalf("an explicit ToolID wins: %v", body)
	}
}

func TestCommitReservationResult_RecordDebt_20261123(t *testing.T) {
	s := &commitSrv{reply: `{"status":"committed","reservation_id":"res-1","reserved_amount":"1.00","actual_cost":"3.50","refunded_amount":"0","message":"ok","commit_mode":"record_debt","unfunded_overrun":"2.50","new_balance":"-2.50"}`}
	c := newCommitClient(t, s)
	in := priced()
	in.OnInsufficientFunds = OnInsufficientFundsRecordDebt
	r, err := c.CommitReservationResult(context.Background(), "org-1", "res-1", in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != CommitRecordedDebt || !r.Accounted() || r.CommitMode != "record_debt" {
		t.Fatalf("got %+v", r)
	}
	if !r.UnfundedOverrun.Equal(decimal.RequireFromString("2.50")) || !r.NewBalance.Equal(decimal.RequireFromString("-2.50")) {
		t.Fatalf("money: %+v", r)
	}
	if s.body["on_insufficient_funds"] != "record_debt" {
		t.Fatalf("body %v", s.body)
	}
	// record_debt honoured with an affordable overrun: unfunded 0 → COMMITTED (not RECORDED_DEBT).
	s.reply = `{"status":"committed","reservation_id":"res-1","reserved_amount":"1","actual_cost":"0.5","refunded_amount":"0.5","message":"ok","commit_mode":"record_debt","unfunded_overrun":"0","new_balance":"4"}`
	r, _ = c.CommitReservationResult(context.Background(), "org-1", "res-1", in)
	if r.Outcome != CommitCommitted || r.CommitMode != "record_debt" || r.UnfundedOverrun == nil || !r.UnfundedOverrun.IsZero() {
		t.Fatalf("an unfunded_overrun of 0 is a plain commit: %+v", r)
	}
}

func TestCommitReservationResult_ErrorAnswersAreResults_20261123(t *testing.T) {
	cases := []struct {
		status int
		reply  string
		want   CommitOutcome
	}{
		{402, `{"detail":"Insufficient balance","code":"INSUFFICIENT_BALANCE"}`, CommitInsufficientFunds},
		{409, `{"detail":"x","code":"RESERVATION_ALREADY_SETTLED"}`, CommitAlreadySettled},
		{409, `{"detail":"x","code":" RESERVATION_NOT_ACTIVE "}`, CommitNotActive}, // code is trimmed
		{409, `{"detail":"x","code":"CONCURRENT_MODIFICATION"}`, CommitConflict},
		{404, `{"detail":"Reservation not found"}`, CommitNotFound},
		{404, `not json`, CommitNotFound},
		{503, `{"detail":"Service temporarily unavailable"}`, CommitUnavailable},
		{422, `{"detail":"x","code":"RECORD_DEBT_REQUIRES_BILLING_PRICING"}`, CommitFailed},
		{409, `{"detail":"x"}`, CommitFailed},
	}
	for _, tc := range cases {
		s := &commitSrv{status: tc.status, reply: tc.reply}
		c := newCommitClient(t, s)
		r, err := c.CommitReservationResult(context.Background(), "org-1", "res-1", priced())
		if err != nil {
			t.Fatalf("%d %s: a billing answer must be a result, got error %v", tc.status, tc.reply, err)
		}
		if r.Outcome != tc.want || r.StatusCode != tc.status {
			t.Errorf("%d %s: got %s (status %d), want %s", tc.status, tc.reply, r.Outcome, r.StatusCode, tc.want)
		}
	}
}

// A 2xx whose answer cannot be read, and a transport failure, are UNAVAILABLE — never "committed".
func TestCommitReservationResult_UnknownAnswerIsUnavailable_20261123(t *testing.T) {
	for name, s := range map[string]*commitSrv{
		"empty 2xx":    {noBody: true},
		"non-json 2xx": {reply: `<html>ok</html>`},
	} {
		c := newCommitClient(t, s)
		r, err := c.CommitReservationResult(context.Background(), "org-1", "res-1", priced())
		if err != nil || r.Outcome != CommitUnavailable || r.Accounted() || !r.Retryable() {
			t.Errorf("%s: got %+v, %v — an unreadable success must not read as committed", name, r, err)
		}
	}
	// valid JSON that is not an object: Python reads it as an empty answer → COMMITTED.
	s := &commitSrv{reply: `[]`}
	r, _ := newCommitClient(t, s).CommitReservationResult(context.Background(), "org-1", "res-1", priced())
	if r.Outcome != CommitCommitted || r.ReservationID != "res-1" {
		t.Errorf("non-object JSON 2xx: got %+v", r)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	c, _ := New(mesh.URLs{Billing: url})
	r, err := c.CommitReservationResult(context.Background(), "org-1", "res-1", priced())
	if err != nil || r.Outcome != CommitUnavailable || !r.HoldMayRemain() {
		t.Fatalf("transport failure: got %+v, %v", r, err)
	}
}

// Local validation: refused with ErrInvalidRequest BEFORE any network call.
func TestCommitReservationResult_LocalValidation_NoNetwork_20261123(t *testing.T) {
	s := &commitSrv{reply: `{}`}
	c := newCommitClient(t, s)
	callerPriced := CommitRequest{ActualUsage: map[string]any{"cost": "1.00"}}
	recordDebtCallerPriced := callerPriced
	recordDebtCallerPriced.OnInsufficientFunds = OnInsufficientFundsRecordDebt
	onlyStart := CommitRequest{StartedAt: cStart}
	badOption := priced()
	badOption.OnInsufficientFunds = "charge_anyway"
	cases := map[string]struct {
		org, res string
		in       CommitRequest
	}{
		"record_debt caller-priced": {"org-1", "res-1", recordDebtCallerPriced},
		"started without stopped":   {"org-1", "res-1", onlyStart},
		"unknown option":            {"org-1", "res-1", badOption},
		"nothing to price":          {"org-1", "res-1", CommitRequest{}},
		"blank reservation":         {"org-1", " ", priced()},
		"blank org":                 {"", "res-1", priced()},
		"dot-dot org":               {"..", "res-1", priced()},
	}
	for name, tc := range cases {
		r, err := c.CommitReservationResult(context.Background(), tc.org, tc.res, tc.in)
		if !errors.Is(err, ErrInvalidRequest) || r != nil {
			t.Errorf("%s: got (%+v, %v), want ErrInvalidRequest", name, r, err)
		}
	}
	if n := atomic.LoadInt32(&s.hits); n != 0 {
		t.Fatalf("a refused request reached billing %d time(s) — validation must run before any network call", n)
	}
	// Control: the same client DOES reach billing for valid inputs (the zero above is meaningful),
	// including the deprecated caller-priced form with an explicit refuse.
	callerPriced.OnInsufficientFunds = OnInsufficientFundsRefuse
	if _, err := c.CommitReservationResult(context.Background(), "org-1", "res-1", callerPriced); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&s.hits) != 1 || s.body["on_insufficient_funds"] != "refuse" {
		t.Fatalf("control: hits=%d body=%v", s.hits, s.body)
	}
	if u, _ := s.body["actual_usage"].(map[string]any); u["cost"] != "1.00" {
		t.Fatalf("caller-priced actual_usage must be sent as given: %v", s.body)
	}
	if _, ok := s.body["started_at"]; ok {
		t.Fatalf("caller-priced commit must not carry started_at: %v", s.body)
	}
}
