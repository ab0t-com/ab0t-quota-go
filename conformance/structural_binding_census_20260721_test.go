package conformance

// T-G9 — the Go-side structural binding census (harness design §5.3; the
// Python twin is tests/test_conformance_binding_census_20260721.py). A
// scenario declared in scenarios.json but bound by no test RUNS NOTHING
// while reading as coverage — the orphan lesson. Every declared item whose
// runtimes include "go" must be referenced by at least one live Go test
// file, or carried in the EXPLICIT, per-item, justified allowlist below.
// No silent grandfathering (D-14 rule 3's census cousin).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// knownUnboundGo — the explicit orphan allowlist. Each entry carries its
// justification; an entry that BECOMES bound must be removed (asserted
// below), so the list can only shrink honestly.
var knownUnboundGo = map[string]string{
	"ST-EFFECT-1": "declared-unbound in BOTH runtimes since birth (harness §5.3, proven live R-6); " +
		"Python carries it as a strict-xfail RED; bind-or-retire assigned to the Python lane by the " +
		"coordinator 2026-07-21 — the disposition is cross-runtime and not lane G's to decide.",
	"ST-WORKING-1": "same standing and same assignment as ST-EFFECT-1.",
}

func goTestFilesReferencing(t *testing.T, id string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "tickets", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") ||
			d.Name() == "structural_binding_census_20260721_test.go" { // the census cannot bind by naming things itself
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(raw), id) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestEveryDeclaredGoScenarioIsBoundOrExplicitlyCarried(t *testing.T) {
	raw, err := os.ReadFile("scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Structural []struct {
			ID       string   `json:"id"`
			Runtimes []string `json:"runtimes"`
		} `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Structural) < 7 {
		t.Fatalf("only %d structural items declared — an emptied JSON cannot pass as coverage "+
			"(rule C, harness §5.3)", len(doc.Structural))
	}
	for _, item := range doc.Structural {
		goDeclared := false
		for _, r := range item.Runtimes {
			if r == "go" {
				goDeclared = true
			}
		}
		if !goDeclared {
			continue
		}
		hits := goTestFilesReferencing(t, item.ID)
		why, allowlisted := knownUnboundGo[item.ID]
		switch {
		case len(hits) == 0 && !allowlisted:
			t.Errorf("ORPHAN: %s is declared for go but bound by ZERO Go tests — a scenario that "+
				"runs nothing while reading as coverage. Bind it in the same change that declared "+
				"it, or carry it here with a justification (never silently)", item.ID)
		case len(hits) > 0 && allowlisted:
			t.Errorf("%s is allowlisted as unbound (%q) but IS now referenced by %v — remove the "+
				"allowlist entry so the list only shrinks honestly", item.ID, why, hits)
		case len(hits) == 0 && allowlisted:
			t.Logf("carried orphan %s — %s", item.ID, why)
		}
	}
}

// TestCensusCatchesPlantedUnboundScenario — D-14: the census logic itself
// must be able to fail. A synthetic declared-for-go ID that no test file
// references must be reported unbound.
func TestCensusCatchesPlantedUnboundScenario(t *testing.T) {
	const planted = "ST-PLANTED-NEVER-BOUND-1"
	if hits := goTestFilesReferencing(t, planted); len(hits) != 0 {
		// This file itself is excluded from the walk, so any hit is real.
		t.Fatalf("planted ID unexpectedly referenced by %v — pick a new plant", hits)
	}
	if _, allowlisted := knownUnboundGo[planted]; allowlisted {
		t.Fatal("the plant leaked into the allowlist")
	}
	// zero hits + not allowlisted is exactly the state the census flags as
	// ORPHAN — the detection path is therefore live for a real new scenario.
}
