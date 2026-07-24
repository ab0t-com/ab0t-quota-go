package counters

// K-10 (D-KS-11) — the Go key-construction census, the QI-09 static-gate
// idiom extended from Lua scripts to KEY CONSTRUCTION: every quota:* counter
// key string must be produced by the ONE shape home. A raw construction
// elsewhere is exactly how a straggler writer survives a keyspace migration
// (spec §11.1 top risk) — spend landing where nobody reads.
//
// Rules (mirrors Python tests/test_keyspace_census_20260721.py):
//   R1 no string literal opening a quota:* KEY outside the shape-home files
//      (constant-folded across `+` so a split literal cannot dodge it);
//      "quota:*" (a SCAN pattern) and "quota: " (prose) are not keys.
//   R2 no KeyPrefix `.Build(` call outside the home (keyspace.go,
//      keys_python.go) — shape assembly lives in one place.
//   R3 `DeprecatedScopeKey(` callable ONLY from the five pre-P5.3 builder
//      files — a SHRINK-ONLY pin; a new deprecated call site fails here.
//   Exemptions, each justified: quota/topology.go (the D-71 probe pair —
//   fixed synthetic hash-tag keys, spec §7 row 15); cmd/quotactl (TOOL-lane
//   SCAN patterns, no key writes).
//
// Every rule ships with planted offenders (D-14: forms, not instances),
// proven caught below by feeding plant sources through the SAME analyzer.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Shape-home files: the only places a quota:* key string may be assembled.
var censusLiteralHome = map[string]bool{
	"counters/keyspace.go":           true,
	"counters/keyspace_migration.go": true,
}

var censusBuildHome = map[string]bool{
	"counters/keyspace.go":    true,
	"counters/keys_python.go": true,
}

// Justified exemptions (D-KS-11): fixed synthetic probe keys / TOOL patterns.
var censusExemptFiles = map[string]string{
	"quota/topology.go": "D-71 data-plane probe pair {foo}/{bar} — fixed synthetic keys (spec §7 row 15)",
}

// The five pre-P5.3 builder files — the ONLY legal DeprecatedScopeKey callers.
var deprecatedCallers = map[string]bool{
	"counters/gauge.go":       true,
	"counters/rate.go":        true,
	"counters/accumulator.go": true,
	"counters/counter.go":     true,
	"counters/idempotency.go": true,
}

// isKeyLiteral reports whether a (folded) string opens a quota:* KEY.
func isKeyLiteral(s string) bool {
	if !strings.HasPrefix(s, "quota:") {
		return false
	}
	rest := s[len("quota:"):]
	if rest == "*" || strings.HasPrefix(rest, " ") {
		return false // SCAN pattern / prose, not a key
	}
	return true
}

// foldConcat constant-folds a tree of `+` over string literals; ok=false when
// any leaf is not a literal (the folded prefix up to that point still counts).
func foldConcat(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err == nil {
				return s, true
			}
		}
	case *ast.ParenExpr:
		return foldConcat(v.X)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			l, lok := foldConcat(v.X)
			if !lok {
				return "", false
			}
			r, _ := foldConcat(v.Y)
			return l + r, true // right side may be partial — prefix suffices
		}
	}
	return "", false
}

type censusViolation struct {
	File string
	Line int
	Rule string
	What string
}

func censusAnalyze(fset *token.FileSet, relPath string, f *ast.File) []censusViolation {
	var out []censusViolation
	add := func(pos token.Pos, rule, what string) {
		out = append(out, censusViolation{File: relPath, Line: fset.Position(pos).Line,
			Rule: rule, What: what})
	}
	inBinary := map[ast.Node]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				// fold once at the top of a concat chain
				if !inBinary[v] {
					if s, _ := foldConcat(v); isKeyLiteral(s) &&
						!censusLiteralHome[relPath] {
						if _, exempt := censusExemptFiles[relPath]; !exempt {
							add(v.Pos(), "R1", "folded key construction "+strconv.Quote(s))
						}
					}
					ast.Inspect(v, func(m ast.Node) bool {
						if b, ok := m.(*ast.BinaryExpr); ok && b != v {
							inBinary[b] = true
						}
						return true
					})
				}
			}
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				if err == nil && isKeyLiteral(s) && !censusLiteralHome[relPath] {
					if _, exempt := censusExemptFiles[relPath]; !exempt {
						add(v.Pos(), "R1", "quota:* literal "+strconv.Quote(s))
					}
				}
			}
		case *ast.CallExpr:
			switch fn := v.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name == "Build" && !censusBuildHome[relPath] {
					add(v.Pos(), "R2", "KeyPrefix.Build call outside the shape home")
				}
			case *ast.Ident:
				if fn.Name == "DeprecatedScopeKey" && !deprecatedCallers[relPath] {
					add(v.Pos(), "R3", "NEW DeprecatedScopeKey call site (shrink-only pin, D-KS-11)")
				}
			}
		}
		return true
	})
	return out
}

func censusOfSource(t *testing.T, relPath, src string) []censusViolation {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, relPath, src, 0)
	if err != nil {
		t.Fatalf("plant does not parse: %v", err)
	}
	return censusAnalyze(fset, relPath, f)
}

// The real sweep: every non-test .go file in the module.
func TestGoKeyConstructionCensus(t *testing.T) {
	root := ".."
	var violations []censusViolation
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "testdata" || name == "vendor" || name == "Skills" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, rel, src, 0)
		if perr != nil {
			return perr
		}
		violations = append(violations, censusAnalyze(fset, rel, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Errorf("%s:%d [%s] %s — quota:* keys are built ONLY in the shape home "+
			"(counters/keyspace*.go); a raw writer here survives the migration as a "+
			"straggler (spec §11.1)", v.File, v.Line, v.Rule, v.What)
	}
}

// Planted offenders (D-14): each FORM proven caught by the same analyzer.
func TestGoCensusPlantsAreCaught(t *testing.T) {
	plants := map[string]struct{ src, rule string }{
		"raw literal": {
			`package p
func k(org string) string { return "quota:" + org + ":x.y:gauge" }`, "R1"},
		"split literal": {
			`package p
func k(org string) string { return "quo" + "ta:" + org + ":x.y:gauge" }`, "R1"},
		"prefix build outside home": {
			`package p
func k(p2 interface{ Build(...string) string }, org string) string { return p2.Build(org, "x.y", "gauge") }`, "R2"},
		"new deprecated call site": {
			`package p
func k() string { return DeprecatedScopeKey("quota", "gauge", "x.y", "s") }`, "R3"},
	}
	for name, plant := range plants {
		hits := censusOfSource(t, "billing/planted.go", plant.src)
		found := false
		for _, h := range hits {
			if h.Rule == plant.rule {
				found = true
			}
		}
		if !found {
			t.Errorf("plant %q NOT caught (rule %s) — the census is green over its own "+
				"blind spot (D-14)", name, plant.rule)
		}
	}
	// Negative controls: a SCAN pattern and prose are not keys; the home may build.
	clean := `package p
var pattern = "quota:*"
var msg = "quota: declared Redis unreachable"`
	if hits := censusOfSource(t, "billing/clean.go", clean); len(hits) != 0 {
		t.Errorf("negative control flagged: %+v", hits)
	}
	home := `package p
func k(org, rk string) string { return "quota:" + org + ":" + rk }`
	if hits := censusOfSource(t, "counters/keyspace.go", home); len(hits) != 0 {
		t.Errorf("the shape home itself was flagged: %+v", hits)
	}
}
