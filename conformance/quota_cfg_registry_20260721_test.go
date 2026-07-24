package conformance

// T-G9 — the Go-side sync-check for the QUOTA-CFG code registry (T-25/D-13).
// quota-cfg-registry.json is CANONICAL in the Python repo and mirrored here
// byte-identically; a code is a consumer-facing contract, so the two copies
// must not drift and neither runtime may raise a code the registry does not
// carry. D-14: the literal scanner ships with a planted-offender control.
// STATED LIMIT (mirrors the Python test's): the scan finds LITERALS only —
// a runtime-assembled code string would evade it (none exist; do not add one).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const registryCanonicalPath = "/home/ubuntu/infra/infra/code/shared/ab0t-quota/conformance/quota-cfg-registry.json"

var cfgCodeRe = regexp.MustCompile(`QUOTA-CFG-(\d{3})`)

type registryDoc struct {
	Codes map[string]struct {
		Meaning    string   `json:"meaning"`
		AssignedBy string   `json:"assigned_by"`
		RaisedBy   []string `json:"raised_by"`
	} `json:"codes"`
}

func loadRegistry(t *testing.T) registryDoc {
	t.Helper()
	raw, err := os.ReadFile("quota-cfg-registry.json")
	if err != nil {
		t.Fatalf("read registry mirror: %v", err)
	}
	var doc registryDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	if len(doc.Codes) == 0 {
		t.Fatal("registry carries no codes")
	}
	return doc
}

// TestRegistryMirrorInSyncWithCanonical — same shape as the scenarios sync
// test: on the shared dev box a stale mirror is a FAILURE; in isolated CI
// (canonical invisible) the mirror is pinned by review.
func TestRegistryMirrorInSyncWithCanonical(t *testing.T) {
	canonical, err := os.ReadFile(registryCanonicalPath)
	if err != nil {
		t.Skipf("canonical registry not visible (%v) — sync pinned by review here", err)
	}
	local, err := os.ReadFile("quota-cfg-registry.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, local) {
		t.Fatal("quota-cfg-registry.json is OUT OF SYNC with the canonical copy — run:\n" +
			"  cp " + registryCanonicalPath + " ./quota-cfg-registry.json")
	}
}

// TestRegistryContiguousNoGaps — allocation is append-only: codes run
// 000..max with no gap and no duplicate (duplicates are impossible in a JSON
// object; a gap means someone allocated out-of-band).
func TestRegistryContiguousNoGaps(t *testing.T) {
	doc := loadRegistry(t)
	var nums []int
	for code := range doc.Codes {
		m := cfgCodeRe.FindStringSubmatch(code)
		if m == nil {
			t.Errorf("malformed code key %q", code)
			continue
		}
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for i, n := range nums {
		if n != i {
			t.Fatalf("registry is not contiguous at %03d (found %v) — a skipped code is an "+
				"out-of-band allocation", i, nums)
		}
	}
	if len(nums) < 11 { // 000..010 existed at adoption (T-25)
		t.Fatalf("registry shrank to %d codes — codes are append-only contracts", len(nums))
	}
}

// scanGoLiterals returns every QUOTA-CFG-nnn literal in non-test Go source
// under root (tickets/ and testdata/ excluded), as code → first "file:line".
func scanGoLiterals(root string) (map[string]string, error) {
	found := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
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
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range cfgCodeRe.FindAllString(line, -1) {
				if _, seen := found[m]; !seen {
					found[m] = fmt.Sprintf("%s:%d", path, i+1)
				}
			}
		}
		return nil
	})
	return found, err
}

// TestEveryGoLiteralIsRegisteredAndEveryGoCodeIsRaised — both directions:
// no Go source literal outside the registry (raised_by must include "go"),
// and no registry code claiming raised_by go that Go source never raises.
func TestEveryGoLiteralIsRegisteredAndEveryGoCodeIsRaised(t *testing.T) {
	doc := loadRegistry(t)
	literals, err := scanGoLiterals("..")
	if err != nil {
		t.Fatal(err)
	}
	if len(literals) == 0 {
		t.Fatal("scanner found zero QUOTA-CFG literals in Go source — the scan surface collapsed")
	}
	raisedByGo := func(code string) bool {
		entry, ok := doc.Codes[code]
		if !ok {
			return false
		}
		for _, r := range entry.RaisedBy {
			if r == "go" {
				return true
			}
		}
		return false
	}
	for code, site := range literals {
		if _, ok := doc.Codes[code]; !ok {
			t.Errorf("Go raises UNREGISTERED code %s (%s) — allocate it in the canonical "+
				"registry in the same change (D-13)", code, site)
		} else if !raisedByGo(code) {
			t.Errorf("Go raises %s (%s) but the registry does not list go in raised_by — "+
				"update the canonical entry", code, site)
		}
	}
	for code, entry := range doc.Codes {
		goRaises := false
		for _, r := range entry.RaisedBy {
			if r == "go" {
				goRaises = true
			}
		}
		if goRaises {
			if _, ok := literals[code]; !ok {
				t.Errorf("registry claims go raises %s but no Go source literal exists — "+
					"stale claim or evasive construction", code)
			}
		}
	}
}

// TestRegistryScannerCatchesPlantedOffender — D-14: prove the scanner CAN
// fail, with an unregistered code planted in a scratch Go file.
func TestRegistryScannerCatchesPlantedOffender(t *testing.T) {
	doc := loadRegistry(t)
	dir := t.TempDir()
	planted := `package scratch
// deliberately unregistered:
const bad = "QUOTA-CFG-099"
`
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	literals, err := scanGoLiterals(dir)
	if err != nil {
		t.Fatal(err)
	}
	site, seen := literals["QUOTA-CFG-099"]
	if !seen {
		t.Fatal("planted unregistered code NOT found by the scanner — the control cannot fail (D-14)")
	}
	if _, registered := doc.Codes["QUOTA-CFG-099"]; registered {
		t.Fatal("the plant collided with a real registration — pick a new planted code")
	}
	t.Logf("planted offender caught at %s and is (correctly) unregistered", site)
}
