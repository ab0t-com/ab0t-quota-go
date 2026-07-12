package handlerledger

// Red test for finding QG-02 (evidence E-D2/E-D3) — ticket
// sandbox-platform/tickets/20260709_ab0t_quota_systemic_integrity_redesign.
//
// Contract under test: the backend AutoSelectStore REPORTS must be the
// backend it actually RETURNS. Today it logs "handler ledger backend: DDB"
// / "handler ledger backend: Redis (72h TTL)" (autoselect.go:24-31) while
// newDDBLedgerStore/newRedisLedgerStore silently hand back
// NewInMemoryLedgerStore (dynamodb.go:15-18, redis.go:20-24). An operator
// reading the log believes they have durable idempotency; they have none.
//
// EXPECTED RED until TASK P1.7 (truthful degradation logs). It must STAY
// green through TASK P5.1: when real backends exist the affirmative log
// becomes true and the degradation branch stops firing.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestAutoSelectStore_ReportedBackendMatchesActual_QG02(t *testing.T) {
	cases := []struct {
		name           string
		opts           AutoSelectOptions
		falseAffirming string // the log line that would claim a durable backend is active
	}{
		{"ddb_requested", AutoSelectOptions{DDBClient: struct{}{}}, "handler ledger backend: DDB"},
		{"redis_requested", AutoSelectOptions{Redis: struct{}{}}, "handler ledger backend: Redis"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			store := AutoSelectStore(tc.opts)
			logged := buf.String()
			_, isMemory := store.(*InMemoryLedgerStore)

			if !isMemory {
				// Real durable backend returned (post-P5.1): the affirmative
				// log is then truthful — nothing more to assert here.
				return
			}
			// Actual backend is memory: the log must not affirm a durable
			// backend, and the degradation must be loud.
			if strings.Contains(logged, tc.falseAffirming) {
				t.Errorf("QG-02: AutoSelectStore returned *InMemoryLedgerStore but logged a durable backend as active.\nlog: %s", logged)
			}
			if !strings.Contains(logged, "DEGRADED") {
				t.Errorf("QG-02: silent degradation — a requested durable backend fell back to memory without a loud DEGRADED warning.\nlog: %s", logged)
			}
			if !strings.Contains(logged, "memory") && !strings.Contains(logged, "InMemoryLedgerStore") {
				t.Errorf("QG-02: degradation log does not name the backend actually in use (memory).\nlog: %s", logged)
			}
		})
	}
}

// TestAutoSelectStore_NoClients_MemoryIsHonest pins the already-correct
// branch: with no durable client supplied, memory is reported as memory
// with a loud warning (autoselect.go:32-34). Guards against regression
// while P1.7/P5.1 rework the other branches.
func TestAutoSelectStore_NoClients_MemoryIsHonest(t *testing.T) {
	buf := captureSlog(t)
	store := AutoSelectStore(AutoSelectOptions{})
	if _, ok := store.(*InMemoryLedgerStore); !ok {
		t.Fatal("no clients → expected *InMemoryLedgerStore")
	}
	logged := buf.String()
	if strings.Contains(logged, "backend: DDB") || strings.Contains(logged, "backend: Redis") {
		t.Errorf("no clients supplied but log claims a durable backend:\n%s", logged)
	}
	if !strings.Contains(logged, "InMemoryLedgerStore") {
		t.Errorf("memory fallback must be loud and name the store:\n%s", logged)
	}
}
