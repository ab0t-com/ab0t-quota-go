package billing

// Typed funds events + handler (v0.1.12, additive). Same contract as the Python ab0t-quota
// funds-events tests.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

const hbExhausted = `{"event_type":"billing.funds_exhausted","trigger":"heartbeat","org_id":"org-1",` +
	`"timestamp":"2026-11-23T10:00:00+00:00","reservation_id":"reservation_1","resource_id":"box-1",` +
	`"state":"exhausted","accrued":"1.000000","hold_amount":"1.000000","available":"0","hourly_rate":"0.25",` +
	`"started_at":"2026-11-23T06:00:00+00:00","runway_seconds":0}`

const hbLow = `{"event_type":"billing.funds_low","trigger":"heartbeat","org_id":"org-1",` +
	`"timestamp":"2026-11-23T09:35:00+00:00","reservation_id":"reservation_1","resource_id":"box-1",` +
	`"state":"low","accrued":"0.90","hold_amount":"1.0","available":"0","hourly_rate":"0.25",` +
	`"started_at":"2026-11-23T06:00:00+00:00","runway_seconds":1440}`

const commitOverrun = `{"event_type":"billing.funds_exhausted","trigger":"commit_overrun","org_id":"org-1",` +
	`"timestamp":"2026-11-23T10:20:00Z","reservation_id":"reservation_1","actual_cost":"1.083334",` +
	`"reserved_amount":"1.000000","unfunded_overrun":"0.083334","new_balance":"-0.083334"}`

func snsEnvelope(inner string) []byte {
	b, _ := json.Marshal(map[string]string{"Type": "Notification", "Message": inner})
	return b
}

func sqsEntry(inner string) []byte {
	b, _ := json.Marshal(map[string]string{"MessageId": "m1", "Body": string(snsEnvelope(inner))})
	return b
}

func TestParseFundsEvent_EveryShape_20261123(t *testing.T) {
	quoted, _ := json.Marshal(string(snsEnvelope(hbExhausted)))
	for name, msg := range map[string][]byte{
		"raw":           []byte(hbExhausted),
		"sns envelope":  snsEnvelope(hbExhausted),
		"sqs entry":     sqsEntry(hbExhausted),
		"quoted string": quoted,
	} {
		ev, err := ParseFundsEvent(msg)
		if err != nil || ev == nil {
			t.Fatalf("%s: %v %v", name, ev, err)
		}
		if ev.Kind != FundsExhausted || ev.EventType != "billing.funds_exhausted" || ev.Trigger != TriggerHeartbeat ||
			ev.OrgID != "org-1" || ev.ReservationID != "reservation_1" || ev.ResourceID != "box-1" {
			t.Fatalf("%s: %+v", name, ev)
		}
		if !ev.Accrued.Equal(decimal.RequireFromString("1")) || !ev.HourlyRate.Equal(decimal.RequireFromString("0.25")) {
			t.Fatalf("%s money: %+v", name, ev)
		}
		if ev.RunwaySeconds == nil || *ev.RunwaySeconds != 0 || !ev.StartedAt.Equal(time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)) {
			t.Fatalf("%s runway/start: %+v", name, ev)
		}
		if !ev.IsRunningResource() || !ev.IsDebt() {
			t.Fatalf("%s: heartbeat exhausted is running + debt", name)
		}
	}
}

func TestFundsEventKind_WireNames_20261123(t *testing.T) {
	if FundsLow.EventType() != "billing.funds_low" || FundsExhausted.EventType() != "billing.funds_exhausted" {
		t.Fatal("wire names drifted from billing's")
	}
}

func TestParseFundsEvent_AfterStopTriggers_20261123(t *testing.T) {
	ev, err := ParseFundsEvent([]byte(commitOverrun))
	if err != nil {
		t.Fatal(err)
	}
	if ev.IsRunningResource() || !ev.IsDebt() || !ev.UnfundedOverrun.Equal(decimal.RequireFromString("0.083334")) {
		t.Fatalf("%+v", ev)
	}
	if ev.Accrued != nil || ev.HoldAmount != nil {
		t.Fatal("absent money must be nil, never 0")
	}
	if ev.Timestamp.IsZero() {
		t.Fatal("Z timestamp must parse")
	}
	if ev.DedupKey() != "reservation_1|billing.funds_exhausted|commit_overrun" {
		t.Fatalf("dedup key %q", ev.DedupKey())
	}
}

func TestParseFundsEvent_NotOursIsNil_20261123(t *testing.T) {
	for _, msg := range []string{
		`{"event_type":"tier.changed","org_id":"o"}`,
		`{"event_type":"billing.hold_expired","org_id":"o"}`,
		`{"event_type":"resource.heartbeat","org_id":"o"}`,
		`{"no_event_type":true}`,
		`["a"]`,
	} {
		ev, err := ParseFundsEvent([]byte(msg))
		if ev != nil || err != nil {
			t.Fatalf("%s: want (nil, nil), got %v %v", msg, ev, err)
		}
	}
}

func TestParseFundsEvent_Malformed_20261123(t *testing.T) {
	if _, err := ParseFundsEvent([]byte(`{nope`)); !errors.Is(err, ErrMalformedFundsEvent) {
		t.Fatalf("garbage: %v", err)
	}
	if _, err := ParseFundsEvent([]byte(`{"event_type":"billing.funds_low","org_id":""}`)); !errors.Is(err, ErrMalformedFundsEvent) {
		t.Fatalf("no org: %v", err)
	}
}

func TestFundsEventHandler_RoutesAndDedups_20261123(t *testing.T) {
	var seen []string
	h := &FundsEventHandler{
		OnLow:       func(_ context.Context, ev FundsEvent) error { seen = append(seen, "low:"+ev.ReservationID); return nil },
		OnExhausted: func(_ context.Context, ev FundsEvent) error { seen = append(seen, "exhausted:"+ev.Trigger); return nil },
	}
	ctx := context.Background()
	steps := []struct {
		msg  []byte
		want DispatchResult
	}{
		{sqsEntry(hbLow), DispatchHandled},
		{sqsEntry(hbLow), DispatchDuplicate},
		{sqsEntry(hbExhausted), DispatchHandled},
		{sqsEntry(commitOverrun), DispatchHandled},
		{[]byte(`{"event_type":"tier.changed","org_id":"o"}`), DispatchIgnored},
		{[]byte(`{nope`), DispatchMalformed},
	}
	for i, s := range steps {
		got, err := h.Handle(ctx, s.msg)
		if err != nil || got != s.want {
			t.Fatalf("step %d: got %s %v want %s", i, got, err, s.want)
		}
	}
	if len(seen) != 3 || seen[0] != "low:reservation_1" || seen[1] != "exhausted:heartbeat" || seen[2] != "exhausted:commit_overrun" {
		t.Fatalf("seen %v", seen)
	}
}

func TestFundsEventHandler_RearmAfterWindow_20261123(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	dd := &MemoryDedup{TTL: time.Minute, Now: func() time.Time { return now }}
	h := &FundsEventHandler{OnLow: func(context.Context, FundsEvent) error { return nil }, Dedup: dd}
	ctx := context.Background()
	mustResult := func(want DispatchResult) {
		t.Helper()
		if got, err := h.Handle(ctx, []byte(hbLow)); err != nil || got != want {
			t.Fatalf("got %s %v want %s", got, err, want)
		}
	}
	mustResult(DispatchHandled)
	now = now.Add(59 * time.Second)
	mustResult(DispatchDuplicate)
	now = now.Add(2 * time.Second)
	mustResult(DispatchHandled)
}

func TestFundsEventHandler_HookFailureReleases_20261123(t *testing.T) {
	calls := 0
	h := &FundsEventHandler{OnExhausted: func(context.Context, FundsEvent) error {
		calls++
		if calls == 1 {
			return errors.New("could not stop the box")
		}
		return nil
	}}
	ctx := context.Background()
	if _, err := h.Handle(ctx, []byte(hbExhausted)); err == nil {
		t.Fatal("hook error must be returned (do not ack)")
	}
	if got, err := h.Handle(ctx, []byte(hbExhausted)); err != nil || got != DispatchHandled || calls != 2 {
		t.Fatalf("redelivery must reach the hook: %s %v calls=%d", got, err, calls)
	}
}

func TestFundsEventHandler_NoHookAndDisabledDedup_20261123(t *testing.T) {
	h := &FundsEventHandler{}
	if got, _ := h.Handle(context.Background(), []byte(hbExhausted)); got != DispatchNoHook {
		t.Fatalf("got %s", got)
	}
	n := 0
	h2 := &FundsEventHandler{OnLow: func(context.Context, FundsEvent) error { n++; return nil }, DisableDedup: true}
	_, _ = h2.Handle(context.Background(), []byte(hbLow))
	_, _ = h2.Handle(context.Background(), []byte(hbLow))
	if n != 2 {
		t.Fatalf("DisableDedup must call the hook every time, got %d", n)
	}
}

func TestMemoryDedup_Bounded_20261123(t *testing.T) {
	dd := &MemoryDedup{TTL: time.Hour, Max: 3}
	for i := 0; i < 10; i++ {
		if ok, _ := dd.Claim(context.Background(), string(rune('a'+i))); !ok {
			t.Fatalf("claim %d refused", i)
		}
	}
	if dd.Len() > 3 {
		t.Fatalf("len %d > max", dd.Len())
	}
}
