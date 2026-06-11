package counters

import (
	"context"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func TestInMemoryStore_IncrAndGet(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	v, err := s.IncrByFloat(ctx, "k", 1.5)
	if err != nil || v != 1.5 {
		t.Fatalf("v=%v err=%v", v, err)
	}
	v, err = s.IncrByFloat(ctx, "k", 2.25)
	if err != nil || v != 3.75 {
		t.Fatalf("v=%v err=%v", v, err)
	}
	got, found, err := s.GetFloat(ctx, "k")
	if err != nil || !found || got != 3.75 {
		t.Fatalf("got=%v found=%v err=%v", got, found, err)
	}
}

func TestInMemoryStore_Float64Precision(t *testing.T) {
	// Spec C2: float64 throughout. Verify INCRBYFLOAT-like semantics on
	// a typical USD-spend additions sequence (no int rounding).
	s := NewInMemoryStore()
	ctx := context.Background()
	for _, d := range []float64{0.1, 0.2, 0.3} {
		if _, err := s.IncrByFloat(ctx, "spend", d); err != nil {
			t.Fatal(err)
		}
	}
	v, _, _ := s.GetFloat(ctx, "spend")
	if v < 0.59 || v > 0.61 {
		t.Errorf("expected ~0.6, got %v", v)
	}
}

func TestInMemoryStore_TTLExpiry(t *testing.T) {
	s := NewInMemoryStore()
	t0 := time.Now()
	s.SetClock(func() time.Time { return t0 })
	ctx := context.Background()
	_ = s.Set(ctx, "k", 1.0, 1*time.Second)
	_, found, _ := s.GetFloat(ctx, "k")
	if !found {
		t.Fatal("expected found before expiry")
	}
	s.SetClock(func() time.Time { return t0.Add(2 * time.Second) })
	_, found, _ = s.GetFloat(ctx, "k")
	if found {
		t.Error("expected expired")
	}
}

func TestSetIfAbsent_IdempotencyRace(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	set, _ := s.SetIfAbsent(ctx, "k", "v1", 0)
	if !set {
		t.Fatal("first claim should win")
	}
	set, _ = s.SetIfAbsent(ctx, "k", "v2", 0)
	if set {
		t.Error("second claim must lose")
	}
}

func TestCurrentPeriod_Shapes(t *testing.T) {
	now := time.Date(2026, 6, 11, 15, 30, 45, 0, time.UTC)
	tests := []struct {
		reset config.ResetPeriod
		want  string
	}{
		{config.ResetHourly, "2026-06-11T15"},
		{config.ResetDaily, "2026-06-11"},
		{config.ResetMonthly, "2026-06"},
		{"unknown", "all"},
	}
	for _, tc := range tests {
		got := CurrentPeriod(tc.reset, now)
		if got != tc.want {
			t.Errorf("%q: got %q want %q", tc.reset, got, tc.want)
		}
	}
}

func TestCurrentPeriod_WeeklyISO(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // Mon — ISO week 2
	got := CurrentPeriod(config.ResetWeekly, now)
	if got != "2026-W02" {
		t.Errorf("got %q", got)
	}
}

func TestCounter_KeyShape(t *testing.T) {
	c := Counter{Prefix: "quota", ResourceKey: "spend"}
	got := c.PeriodKey("org-abc", "2026-06")
	if got != "quota:counter:spend:org-abc:2026-06" {
		t.Errorf("got %q", got)
	}
	// Empty prefix
	c2 := Counter{Prefix: "", ResourceKey: "spend"}
	got2 := c2.PeriodKey("org-abc", "2026-06")
	if got2 != "counter:spend:org-abc:2026-06" {
		t.Errorf("got %q", got2)
	}
}

func TestGauge_KeyShape(t *testing.T) {
	g := Gauge{Prefix: "quota", ResourceKey: "sandbox.concurrent"}
	got := g.Key("org-1")
	if got != "quota:gauge:sandbox.concurrent:org-1" {
		t.Errorf("got %q", got)
	}
}

func TestAccumulator_KeyAndExpiry(t *testing.T) {
	s := NewInMemoryStore()
	t0 := time.Date(2026, 6, 11, 15, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return t0 })
	a := Accumulator{
		Store:       s,
		Prefix:      "quota",
		ResourceKey: "spend",
		Reset:       config.ResetMonthly,
	}
	v, err := a.Add(context.Background(), "org-x", t0, 5.50)
	if err != nil || v != 5.50 {
		t.Fatalf("v=%v err=%v", v, err)
	}
	want := "quota:accumulator:spend:org-x:2026-06"
	got := a.PeriodKey("org-x", t0)
	if got != want {
		t.Errorf("key got %q want %q", got, want)
	}
}

func TestRate_RecordAndCount(t *testing.T) {
	rs := NewMemoryRateStore()
	r := Rate{
		Store:       rs,
		Prefix:      "quota",
		ResourceKey: "api.calls",
		Window:      60 * time.Second,
	}
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 3; i++ {
		_ = r.Record(ctx, "org-x", now.Add(time.Duration(i)*time.Second), "m")
	}
	count, _ := r.Count(ctx, "org-x", now.Add(3*time.Second))
	if count != 3 {
		t.Errorf("got %d", count)
	}
	// Past the window — old entries trimmed.
	count, _ = r.Count(ctx, "org-x", now.Add(2*time.Minute))
	if count != 0 {
		t.Errorf("expected 0 after window, got %d", count)
	}
}

func TestIdempotency_HashAndClaim(t *testing.T) {
	is := IdempotencyStore{Store: NewInMemoryStore(), Prefix: "quota"}
	ctx := context.Background()
	ok, _ := is.Claim(ctx, "req-1", "ok", time.Hour)
	if !ok {
		t.Fatal("first claim should win")
	}
	ok, _ = is.Claim(ctx, "req-1", "ok", time.Hour)
	if ok {
		t.Error("dup claim must lose")
	}
	// Hash determinism
	if HashKey("req-1") != HashKey("req-1") {
		t.Error("hash must be deterministic")
	}
	if HashKey("") != "" {
		t.Error("empty key → empty hash")
	}
}

func TestFactory_BuildAllTypes(t *testing.T) {
	f := NewMemoryFactory("quota")
	if f.Counter("api.calls").PeriodKey("org", "p") != "quota:counter:api.calls:org:p" {
		t.Error("counter key wrong")
	}
	if f.Gauge("sandbox.concurrent").Key("org") != "quota:gauge:sandbox.concurrent:org" {
		t.Error("gauge key wrong")
	}
	r := f.Rate(config.ResourceDef{ResourceKey: "api.calls", WindowSeconds: 60})
	if r.Window != 60*time.Second {
		t.Error("rate window wrong")
	}
	a := f.Accumulator("spend", config.ResetMonthly)
	if a.Reset != config.ResetMonthly {
		t.Error("accumulator reset wrong")
	}
	if (f.Idempotency().Prefix) != "quota" {
		t.Error("idempotency prefix wrong")
	}
}

func TestRedisStore_StubReturnsTypedError(t *testing.T) {
	if _, err := NewRedisStore(nil); err == nil {
		t.Error("expected typed error from stub")
	}
	if _, err := NewRedisRateStore(nil); err == nil {
		t.Error("expected typed error from stub")
	}
}
