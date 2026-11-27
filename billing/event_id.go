package billing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// LifecycleHeartbeat is the lifecycle event type that is a NEW fact every time it is sent.
const LifecycleHeartbeat = "resource.heartbeat"

// LifecycleEventID (v0.2.0) is the event_id a lifecycle event is built with — the same rule as the
// Python library (LifecycleEmitter._event_id; shared golden testdata/event_id_golden_20261126.json):
//
//   - a one-shot event (resource.started / stopped / deleted) WITH a reservation id is
//     DETERMINISTIC: "evt_" + uuid5(<library namespace>, "lifecycle:{reservationID}:{eventType}") —
//     re-emitting it (a retried Stop, a restarted process) is a replay billing recognises, never a
//     second event;
//   - a heartbeat, or an event without a reservation id, is a fresh random "evt_" + 32 hex.
func LifecycleEventID(eventType, reservationID string) string {
	if reservationID != "" && eventType != LifecycleHeartbeat {
		return "evt_" + uuid5EventID("lifecycle:"+reservationID+":"+eventType)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("evt_%032x", time.Now().UnixNano()) // never happens on a working OS; still unique
	}
	return "evt_" + hex.EncodeToString(b[:])
}

// legacyEventID is the id of an outbox intent written WITHOUT an event_id (by an older process):
// "evt_" + uuid5(<library namespace>, "{key}:{emittedAt}"), an absent emitted_at read as "". One rule
// for every event type, shared with the Python library (LifecycleEmitter._legacy_event_id).
func legacyEventID(key, emittedAt string) string {
	return "evt_" + uuid5EventID(key+":"+emittedAt)
}
