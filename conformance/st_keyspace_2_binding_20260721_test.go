package conformance

// K-8 (D-KS-10) — Go binding for ST-KEYSPACE-2 (keyspace migration
// semantics), declared for go in the SAME change that built the mechanism
// (the K-7 no-orphan rule). The clauses execute in the K-8 suites; this test
// pins that mapping so retiring either suite breaks the BINDING, not just
// coverage — the same discipline as Python's
// tests/test_st_keyspace_bindings_20260721.py.
//
// Clause → executing test:
//   seed-if-absent atomicity      → engine TestKS4_SeedIfAbsentNeverZeroNeverDoubles
//                                   + counters TestMigrationFullStateMachine (backfill)
//   dual-claimed idem latches     → engine TestKS6_IdemLatchDualClaimedAcrossFlip
//   dual-read fallback order      → engine TestKS5_DualReadFallbackOrder
//   gated flip (idem TTL + rate)  → counters TestMigrationFullStateMachine ("too young")
//   guarded reap (marker-first)   → counters TestMigrationFullStateMachine (reap refusals)
//   broken-dual plant caught      → counters TestPlantBrokenDualWriteIsCaughtByVerify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSTKeyspace2GoBindingMapsToRunningSuites(t *testing.T) {
	raw, err := os.ReadFile("scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Structural []struct {
			ID       string   `json:"id"`
			Runtimes []string `json:"runtimes"`
			Contract []string `json:"contract"`
		} `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range doc.Structural {
		if item.ID != "ST-KEYSPACE-2" {
			continue
		}
		found = true
		goDeclared := false
		for _, r := range item.Runtimes {
			if r == "go" {
				goDeclared = true
			}
		}
		if !goDeclared {
			t.Fatal("ST-KEYSPACE-2 must declare runtime go now the Go mechanism exists (D-KS-10)")
		}
		if len(item.Contract) != 5 {
			t.Fatalf("ST-KEYSPACE-2 contract has %d clauses, binding maps 5", len(item.Contract))
		}
	}
	if !found {
		t.Fatal("ST-KEYSPACE-2 not declared in scenarios.json")
	}

	// Pin the clause → suite mapping: the named tests must still exist.
	for file, needles := range map[string][]string{
		"../engine/keyspace_dual_20260721_test.go": {
			"TestKS4_SeedIfAbsentNeverZeroNeverDoubles",
			"TestKS5_DualReadFallbackOrder",
			"TestKS6_IdemLatchDualClaimedAcrossFlip",
		},
		"../counters/keyspace_migration_20260721_test.go": {
			"TestMigrationFullStateMachine",
			"TestPlantBrokenDualWriteIsCaughtByVerify",
			"too young",
			"reap without the scope confirmation must refuse",
		},
	} {
		src, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("binding suite missing: %v", err)
		}
		for _, needle := range needles {
			if !strings.Contains(string(src), needle) {
				t.Errorf("%s no longer binds ST-KEYSPACE-2 (%s)", file, needle)
			}
		}
	}
}
