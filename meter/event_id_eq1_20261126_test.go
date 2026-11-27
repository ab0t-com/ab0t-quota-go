package meter_test

// Edge cases (v0.2.0): the meter builds a one-shot lifecycle event (start / stop) with the DETERMINISTIC id
// the Python emitter uses — billing.LifecycleEventID, pinned by the shared event-id golden — so a
// re-emitted stop is a replay to billing; heartbeats stay fresh. RED before: every id was random.

import (
	"context"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/ab0t-com/ab0t-quota-go/meter/metertest"
)

func TestMeterEventIDs_DeterministicForOneShotEvents_EQ1(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, func(c *meter.Config) { c.HeartbeatInterval = time.Hour })
	run, err := m.Start(context.Background(), in(nil), t0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = run.HeartbeatNow(context.Background())
	_, _ = run.HeartbeatNow(context.Background())
	_, _ = run.Stop(context.Background(), meter.StopInput{})
	var beats []string
	for _, ev := range f.Events("", run.ReservationID) {
		switch ev.EventType {
		case meter.EventStarted, meter.EventStopped:
			if want := billing.LifecycleEventID(ev.EventType, run.ReservationID); ev.EventID != want {
				t.Errorf("%s: event_id %s; want the deterministic %s", ev.EventType, ev.EventID, want)
			}
		case meter.EventHeartbeat:
			beats = append(beats, ev.EventID)
		}
	}
	if len(beats) != 2 || beats[0] == beats[1] {
		t.Fatalf("heartbeat ids %v — two beats are two facts (fresh ids)", beats)
	}
}
