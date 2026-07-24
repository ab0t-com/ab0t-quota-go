package doclint

// T-G4 + T-G6 + T-G7 (GO-03/GO-04, the Gate-E runbook defect, and the
// exemption-freeze gap) — the doc-lint's own controls. D-14 binds this file:
//   - the planted-offender table spans FORMS (content classes × markup
//     shapes × file types × case/quoting variants), and it must be at least
//     as strong as the ad-hoc attack that last found a miss (T-G7 note);
//   - coverage is bound to EFFECTIVE BEHAVIOUR: TestScanSetBindsBehaviour
//     recomputes the expected file set independently and compares it to what
//     Lint actually visited, so exemptions, skippedDirs, extension filters
//     or glob changes ALL turn it red — not just edits to one frozen list;
//   - the exemption list stays frozen as the human-readable record; stated
//     limits live in the package docstring.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDocLintRepoClean(t *testing.T) {
	violations, scanned, err := Lint("../..")
	if err != nil {
		t.Fatalf("lint walk: %v", err)
	}
	if len(scanned) < 10 {
		t.Fatalf("only %d files scanned — the exclusion surface has eaten the repo", len(scanned))
	}
	for _, v := range violations {
		t.Errorf("doc-lint [%s] %s:%d: %s", v.Pattern, v.Path, v.Line, v.Text)
	}
	if len(violations) > 0 {
		t.Errorf("%d violation(s): the instruction surface contradicts the library "+
			"(GO-03/GO-04/T-G6 class) — fix the doc, never the linter", len(violations))
	}
	t.Logf("doc-lint: %d files scanned, %d violations", len(scanned), len(violations))
}

// plant is one seeded offender: the exact text a doc author might write, and
// the pattern that must catch it. Line numbers are computed, not hand-counted.
type plant struct {
	text    string
	pattern string // "" = negative control (must NOT be flagged)
}

// The permanent plant table. T-G7 rule: every form the ad-hoc Gate-E attack
// used lives here, so the permanent control is never weaker than the attack
// that last found a miss. Grouped by axis.
var plants = []plant{
	// -- fallback-default forms (content × case × scheme × spacing) --
	{`"redis_url": "${QUOTA_REDIS_URL:-redis://localhost:6379/0}"`, "invented-redis-default-in-template"},
	{`"redis_url": "${QUOTA_REDIS_URL:- redis://redis-prod:6379/0}"`, "invented-redis-default-in-template"},
	{`"redis_url": "${QUOTA_REDIS_URL:-rediss://cache.internal:6380/0}"`, "invented-redis-default-in-template"},
	{`"redis_url": "${QUOTA_REDIS_URL:-Rediss://Cache.internal:6380/0}"`, "invented-redis-default-in-template"}, // case variant
	// -- local-endpoint forms (spelling × case × markup shape) --
	{`The library defaults to redis://localhost:6379/0 when unset.`, "localhost-redis-endpoint"},
	{"Use `rediss://127.0.0.1:6380/0` for TLS testing.", "localhost-redis-endpoint"},
	{`connect to Redis://LOCALHOST:6379 first`, "localhost-redis-endpoint"}, // lowercase-prose/case form
	{`"redis_url": "redis://[::1]:6379/0"`, "localhost-redis-endpoint"},    // IPv6 loopback
	{`"redis_url": "redis://host.docker.internal:6379/0"`, "localhost-redis-endpoint"},
	// -- generic env names (markup shapes) --
	{`| REDIS_URL | the redis endpoint |`, "generic-REDIS_URL"},
	{`Set the REDIS_PASSWORD variable before boot.`, "generic-REDIS_PASSWORD"},
	{`auth_url: $AUTH_SERVICE_URL`, "generic-AUTH_SERVICE_URL"},
	{`export STRIPE_WEBHOOK_SECRET=whsec_x`, "generic-STRIPE_WEBHOOK_SECRET"},
	{`AB0T_QUOTA_STRIPE_WEBHOOK_SECRET is the namespaced name and stays legal`, ""},
	// -- false claims --
	{`v0.1.0 ships in-memory backends only.`, "false-claim-in-memory-only"},
	// -- refused key-prefix values (quoting variants) --
	{`"redis_key_prefix": "my-app"`, "non-quota-redis_key_prefix"},
	{`redis_key_prefix = "gateway"`, "non-quota-redis_key_prefix"},
	{`redis_key_prefix: 'my-svc'`, "non-quota-redis_key_prefix"}, // single-quoted
	{`redis_key_prefix: gateway2`, "non-quota-redis_key_prefix"}, // bare YAML value
	// -- the refused store SHAPE that contains the key (T-G7's closest real miss) --
	{`"redis_url": null`, "undeclared-redis_url-in-example"},
	{`"redis_url": ""`, "undeclared-redis_url-in-example"},
	// -- negatives: must stay silent --
	{`QUOTA_REDIS_URL and AB0T_QUOTA_TEST_REDIS_ADDR at redis://localhost:6379 stay legal here`, ""},
	{`"redis_key_prefix": "quota"`, ""},
	{`"_comment_redis_key_prefix": "only quota boots"`, ""},
}

// multi-line block plants (fence-shape axis), appended after the table above.
var blockPlants = []struct {
	lines   []string
	pattern string
	offset  int // violation's line offset from the block's first line (fences report at the fence line: 0)
}{
	{[]string{"```json", `{`, `  "service_name": "seeded1",`, `  "tier_provider": {"type": "static"},`, `  "storage": {"redis_key_prefix": "quota"}`, `}`, "```"},
		"config-example-without-declared-store", 0},
	{[]string{"```jsonc", `{`, `  "service_name": "seeded2",`, `  "tiers": []`, `}`, "```"},
		"config-example-without-declared-store", 0}, // non-json fence label
	{[]string{"```", `{`, `  "enforcement": {"enabled": true},`, `  "resources": []`, `}`, "```"},
		"config-example-without-declared-store", 0}, // bare fence
	{[]string{"    {", `      "service_name": "seeded3",`, `      "tiers": []`, "    }"},
		"config-example-without-declared-store", 0}, // indented code run
}

func TestDocLintCatchesSeededOffenders(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	lines = append(lines, "# seeded offenders (T-G6/T-G7, D-14 plant table)")
	type expect struct {
		pattern string
		line    int
	}
	var wants []expect
	var negatives []int
	for _, p := range plants {
		lines = append(lines, p.text)
		if p.pattern == "" {
			negatives = append(negatives, len(lines))
		} else {
			wants = append(wants, expect{p.pattern, len(lines)})
		}
	}
	for _, bp := range blockPlants {
		lines = append(lines, "") // separator ends any indented run
		start := len(lines) + 1
		lines = append(lines, bp.lines...)
		wants = append(wants, expect{bp.pattern, start + bp.offset})
	}
	seeded := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "seeded.md"), []byte(seeded), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-markdown legs: YAML line rules; raw config templates — BOTH the
	// quota-config* name and an arbitrary name (the rename evasion).
	if err := os.WriteFile(filepath.Join(dir, "seeded.yaml"),
		[]byte("# seeded\nauth_url: ${AUTH_SERVICE_URL}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quota-config.seeded.json"),
		[]byte(`{"service_name": "seeded", "tiers": [], "storage": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway-settings.json"),
		[]byte(`{"tier_provider": {"type": "mesh"}, "resources": [], "storage": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	violations, scanned, err := Lint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 4 {
		t.Fatalf("scanned = %d, want 4", len(scanned))
	}
	type hit struct {
		file    string
		pattern string
		line    int
	}
	got := map[hit]bool{}
	for _, v := range violations {
		got[hit{filepath.Base(v.Path), v.Pattern, v.Line}] = true
	}
	for _, w := range wants {
		if !got[hit{"seeded.md", w.pattern, w.line}] {
			t.Errorf("plant NOT caught: seeded.md [%s] line %d (%q) — the linter is green over "+
				"a blind spot again (D-14)", w.pattern, w.line, lines[w.line-1])
		}
	}
	for _, extra := range []hit{
		{"seeded.yaml", "generic-AUTH_SERVICE_URL", 2},
		{"quota-config.seeded.json", "config-example-without-declared-store", 1},
		{"gateway-settings.json", "config-example-without-declared-store", 1},
	} {
		if !got[extra] {
			t.Errorf("plant NOT caught: %+v", extra)
		}
	}
	for h := range got {
		for _, n := range negatives {
			if h.file == "seeded.md" && h.line == n {
				t.Errorf("false positive on negative-control line %d: %+v (%q)", n, h, lines[n-1])
			}
		}
	}
	t.Logf("plant control: %d positive forms + %d negatives, %d violations recorded",
		len(wants)+3, len(negatives), len(violations))
}

// expectedScanSet recomputes, INDEPENDENTLY of the linter's own walk, which
// files the documented policy says must be scanned: every .md/.json/.yaml/
// .yml outside the justified exemptions. This is the T-G7 behaviour binding.
func expectedScanSet(t *testing.T, root string) map[string]bool {
	t.Helper()
	skipDir := map[string]bool{
		".git": true, "tickets": true, "testdata": true, "node_modules": true, "conformance": true,
	}
	skipFile := func(base string) bool {
		return base == "CHANGELOG.md" ||
			strings.HasPrefix(base, "migrating") ||
			strings.HasPrefix(base, "review_") ||
			strings.HasPrefix(base, "architecture_review") ||
			strings.HasPrefix(base, "tasklist_")
	}
	want := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		ok := false
		for _, suf := range []string{".md", ".json", ".yaml", ".yml"} {
			if strings.HasSuffix(base, suf) {
				ok = true
			}
		}
		if ok && !skipFile(base) {
			want[path] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return want
}

func diffSets(want map[string]bool, got []string) (missing, extra []string) {
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p] = true
	}
	for p := range want {
		if !gotSet[p] {
			missing = append(missing, p)
		}
	}
	for p := range gotSet {
		if !want[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return
}

// TestScanSetBindsBehaviour — the T-G7 fix. The freeze binds to what the
// linter ACTUALLY visited: any mechanism that shrinks coverage (skippedDirs,
// exemptions, suffix filter, globs) produces a set diff and goes RED, and
// key load-bearing files are asserted present by name.
func TestScanSetBindsBehaviour(t *testing.T) {
	want := expectedScanSet(t, "../..")
	_, scanned, err := Lint("../..")
	if err != nil {
		t.Fatal(err)
	}
	missing, extra := diffSets(want, scanned)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("effective scan set diverges from the documented policy:\n missing (policy says "+
			"scan, linter skipped): %v\n extra (linter scanned, policy does not name): %v\n"+
			"Coverage changes are DELIBERATE edits to both this test's policy restatement and the "+
			"linter (D-14 rule 3)", missing, extra)
	}
	for _, sentinel := range []string{
		"docs/INTEGRATION_RUNBOOK.md",
		"Skills/ab0t-quota-go-config/SKILL.md",
		"examples/basic/quota-config.json",
		"PRODUCT_SPEC.md",
		"CONSUMING.md",
	} {
		found := false
		for _, p := range scanned {
			if strings.HasSuffix(p, sentinel) {
				found = true
			}
		}
		if !found {
			t.Errorf("load-bearing file %q missing from the effective scan set", sentinel)
		}
	}
	t.Logf("scan-set binding: %d files, policy and behaviour agree", len(scanned))
}

// TestScanSetControl_CatchesSkippedDirsWidening — the negative control for
// the binding itself, replaying the Gate-E verifier's exact attack: adding
// docs/ to skippedDirs (NOT to ExemptionList) must produce a detectable set
// diff. skippedDirs is restored via defer; the repo copy is never edited.
func TestScanSetControl_CatchesSkippedDirsWidening(t *testing.T) {
	skippedDirs["docs"] = true
	defer delete(skippedDirs, "docs")

	want := expectedScanSet(t, "../..")
	_, scanned, err := Lint("../..")
	if err != nil {
		t.Fatal(err)
	}
	missing, _ := diffSets(want, scanned)
	if len(missing) == 0 {
		t.Fatal("ATTACK NOT DETECTED: docs/ was removed from coverage via skippedDirs and the " +
			"scan-set binding saw no difference — the T-G7 control cannot fail")
	}
	sawDocs := false
	for _, m := range missing {
		if strings.Contains(m, string(filepath.Separator)+"docs"+string(filepath.Separator)) {
			sawDocs = true
		}
	}
	if !sawDocs {
		t.Fatalf("set diff detected (%v) but not the hidden docs/ tree", missing)
	}
	t.Logf("attack detected: %d files reported missing, incl. the docs/ tree", len(missing))
}

// TestExemptionListFrozen — kept as the human-readable record of the
// exemption surface (justifications in the package docstring). The BEHAVIOUR
// binding above is what actually enforces coverage; this pin makes list
// edits reviewable.
func TestExemptionListFrozen(t *testing.T) {
	want := []string{
		"dir:.git", "dir:tickets", "dir:testdata", "dir:node_modules", "dir:conformance",
		"file:CHANGELOG.md", "file:migrating*", "file:review_*",
		"file:architecture_review*", "file:tasklist_*",
	}
	if got := ExemptionList(); !reflect.DeepEqual(got, want) {
		t.Errorf("exemption surface changed:\n got: %v\nwant: %v\nJustify the new entry in "+
			"the package docstring and update BOTH this pin and expectedScanSet deliberately — "+
			"an exemption list is a finding (D-14)", got, want)
	}
}
