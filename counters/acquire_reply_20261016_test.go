package counters

// Ticket 20261016 review A-28 (lane A, re-verified at acquire.go:239-242): an
// acquire script reply of an unexpected shape became AcquireOutcome{} with a nil
// error — a silent DENY (the caller answers 429 "over quota") instead of an
// engine error (503). A reply we cannot read is "could not evaluate", never a
// domain answer. RED on v0.1.9.

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAtomicAcquire_MalformedReplyIsAnError_20261016(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	rs, err := NewRedisStore(client)
	if err != nil {
		t.Fatal(err)
	}
	orig := acquireScript
	acquireScript = redis.NewScript(`return 1`) // a reply the parser does not understand
	defer func() { acquireScript = orig }()

	out, err := rs.(GaugeAcquirer).AtomicAcquire(context.Background(), "quota:o1:x:idem:k", false, time.Hour,
		[]AcquireSpec{{OrgKey: "quota:o1:x:gauge", Delta: 1}})
	if err == nil {
		t.Fatalf("malformed script reply returned (%+v, nil) — a silent deny; want an error", out)
	}
}
