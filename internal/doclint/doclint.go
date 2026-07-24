// Package doclint enforces the "declared, not discovered" contract on the
// INSTRUCTION SURFACE (pack 20260721, T-G4/T-G6; findings GO-03/GO-04 and the
// Gate-E runbook defect). The docs are part of the product: a template that
// injects an invented default, a copy-paste config the shipped library
// refuses at boot, or a claim that contradicts behaviour is the same defect
// as the code harvesting ambient config — delivered through the consumer's
// own files.
//
// D-14 compliance statement (read DECISIONS.md D-14 before changing this):
//
// WHAT IS SCANNED: *.md, *.yaml, *.yml everywhere outside the exemptions,
// plus raw *.json files (root-config templates named quota-config*.json get
// the whole-file declared-store check). Fenced ```json blocks inside
// markdown are additionally analysed as CONFIG EXAMPLES: a block that looks
// like a root quota-config (>= 2 root markers) and declares no redis_url is
// a violation — the exact Gate-E defect (a "minimal viable" config the
// library refuses with QUOTA-CFG-001).
//
// STATED LIMITS (D-14 #4 — honesty over implied coverage):
//   - Go source (comments and string literals) is NOT scanned: it would
//     self-flag this linter's own rule table and the test fixtures that
//     deliberately contain offenders. Doc-shaped content in .go files is a
//     blind spot; keep instruction text out of code comments.
//   - YAML examples get line rules only; the declared-store block analysis
//     covers JSON shapes (the config format this library ships). A
//     YAML-shaped quota-config example would evade the storeless check —
//     none exist today; teach the parser first if one is added.
//   - Semantic truth of prose ("X is wired") cannot be checked statically;
//     only the enumerated false claims in the rule table are caught.
//
// EXEMPTIONS (D-14 #3 — each justified here; the frozen-list test makes any
// addition a visible, reviewable diff):
//   - CHANGELOG.md, migrating-*: their JOB is naming removed generic names.
//   - review_*, architecture_review*: dated historical review records —
//     rewriting them would falsify the record they exist to be.
//   - tasklist_*: historical work journals, same rationale.
//   - tickets/: ticket archives (historical). testdata/: deliberate bad
//     fixtures. conformance/: byte-identical sync of the CANONICAL
//     scenarios.json owned by the Python repo — findings there are fixed
//     upstream, never by editing the synced copy.
package doclint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Violation is one forbidden pattern found in a scanned doc.
type Violation struct {
	Path    string
	Line    int
	Pattern string
	Text    string
}

type rule struct {
	name string
	re   *regexp.Regexp
	// allowPrefixed exempts a match when an identifier character immediately
	// precedes it (e.g. QUOTA_REDIS_URL contains REDIS_URL).
	allowPrefixed bool
}

var rules = []rule{
	// GO-03: an env interpolation whose FALLBACK invents a Redis endpoint in
	// the consumer's own config file. Covers redis:// and rediss://, spaced
	// or not, any letter case (T-G7: the ad-hoc attack's case variants).
	{name: "invented-redis-default-in-template", re: regexp.MustCompile(`(?i):-\s*rediss?://`)},
	// T-G6/T-G7: a local Redis endpoint taught ANYWHERE in the docs, with or
	// without the `:-`, any case, any local spelling (localhost, 127.0.0.1,
	// [::1], host.docker.internal). The documented dev mode is the explicit
	// "memory://" declaration. One line-scoped allowance: instructions for
	// the operator-gated test-harness vars (AB0T_QUOTA_TEST_*) address the
	// operator's own machine.
	{name: "localhost-redis-endpoint", re: regexp.MustCompile(`(?i)rediss?://(\[::1\]|localhost|127\.0\.0\.1|host\.docker\.internal)`)},
	// T-G7: a config example teaching the REFUSED store shape — `"redis_url":
	// null` / `""` contains the key (so the storeless block check passes) yet
	// QUOTA-CFG-001 refuses it at boot. The closest real miss of the Gate-E
	// ad-hoc attack; caught as a line rule so fragments are covered too.
	{name: "undeclared-redis_url-in-example", re: regexp.MustCompile(`"redis_url"\s*:\s*(null|"")`)},
	// Generic infrastructure names — they belong to whichever service defines
	// them, never to this library. Namespaced QUOTA_*/AB0T_* forms are fine.
	{name: "generic-REDIS_URL", re: regexp.MustCompile(`REDIS_URL`), allowPrefixed: true},
	{name: "generic-REDIS_PASSWORD", re: regexp.MustCompile(`REDIS_PASSWORD`), allowPrefixed: true},
	{name: "generic-AUTH_SERVICE_URL", re: regexp.MustCompile(`AUTH_SERVICE_URL`)},
	{name: "generic-STRIPE_WEBHOOK_SECRET", re: regexp.MustCompile(`STRIPE_WEBHOOK_SECRET`), allowPrefixed: true},
	// GO-04: the claim the skill made against the shipped library.
	{name: "false-claim-in-memory-only", re: regexp.MustCompile(`in-memory backends only`)},
}

// prefixRe extracts a redis_key_prefix value in any quoting style — double,
// single, or bare (JSON, YAML, prose assignment). Anything but "quota" is
// refused at boot (D-17, setup.go) — an example teaching another value is a
// config the library will not start (the Gate-E runbook defect, leg 1).
var prefixRe = regexp.MustCompile(`redis_key_prefix["']?\s*[:=]\s*["']?([A-Za-z0-9_.\-]+)`)

// testHarnessAllowRe: the one line-scoped allowance for
// localhost-redis-endpoint (see the rule comment).
var testHarnessAllowRe = regexp.MustCompile(`AB0T_QUOTA_TEST_[A-Z_]*`)

// Root-config markers for the declared-store analysis: >= 2 of these in one
// JSON document/block means "this is presented as a whole quota-config",
// which post-T-G1 MUST declare storage.redis_url (or "memory://").
var rootMarkers = []string{
	`"service_name"`, `"tier_provider"`, `"tiers"`, `"resources"`, `"enforcement"`,
}

var skippedDirs = map[string]bool{
	".git": true, "tickets": true, "testdata": true, "node_modules": true,
	"conformance": true, // byte-identical sync of the Python-owned canonical fixture
}

func skippedFile(base string) bool {
	switch {
	case base == "CHANGELOG.md":
		return true
	case strings.HasPrefix(base, "migrating"):
		return true
	case strings.HasPrefix(base, "review_"):
		return true
	case strings.HasPrefix(base, "architecture_review"):
		return true
	case strings.HasPrefix(base, "tasklist_"):
		return true
	}
	return false
}

// ExemptionList returns the exemption surface verbatim, so a test can freeze
// it: any future widening is a visible, reviewable diff (D-14 #3).
func ExemptionList() []string {
	return []string{
		"dir:.git", "dir:tickets", "dir:testdata", "dir:node_modules", "dir:conformance",
		"file:CHANGELOG.md", "file:migrating*", "file:review_*",
		"file:architecture_review*", "file:tasklist_*",
	}
}

func prefixedOK(line string, start int) bool {
	if start == 0 {
		return false
	}
	c := line[start-1]
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func scannableSuffix(base string) bool {
	for _, suf := range []string{".md", ".json", ".yaml", ".yml"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// lintLines applies the line rules + the prefix-value rule.
func lintLines(path, content string) []Violation {
	var out []Violation
	for i, line := range strings.Split(content, "\n") {
		for _, r := range rules {
			for _, loc := range r.re.FindAllStringIndex(line, -1) {
				if r.allowPrefixed && prefixedOK(line, loc[0]) {
					continue
				}
				if r.name == "localhost-redis-endpoint" && testHarnessAllowRe.MatchString(line) {
					continue
				}
				out = append(out, Violation{Path: path, Line: i + 1, Pattern: r.name, Text: strings.TrimSpace(line)})
			}
		}
		for _, idx := range prefixRe.FindAllStringSubmatchIndex(line, -1) {
			// Same preceding-identifier guard as allowPrefixed: a larger
			// identifier (e.g. `_comment_redis_key_prefix`) is not the key.
			if prefixedOK(line, idx[0]) {
				continue
			}
			if val := line[idx[2]:idx[3]]; val != "quota" {
				out = append(out, Violation{Path: path, Line: i + 1,
					Pattern: "non-quota-redis_key_prefix", Text: strings.TrimSpace(line)})
			}
		}
	}
	return out
}

// storelessConfig reports whether a JSON document/block is presented as a
// whole quota-config but declares no store — the boot-refused-copy-paste
// class (QUOTA-CFG-001).
func storelessConfig(body string) bool {
	markers := 0
	for _, m := range rootMarkers {
		if strings.Contains(body, m) {
			markers++
		}
	}
	return markers >= 2 && !strings.Contains(body, `"redis_url"`)
}

// lintCodeBlocks analyses code blocks in markdown as config examples —
// FENCED blocks labeled json* (```json, ```jsonc, ```JSON) or BARE (```),
// and INDENTED code runs (4-space/tab markdown code) — the Gate-E ad-hoc
// attack's non-```json forms. Fences declaring ANOTHER language (```go,
// ```bash …) are excluded: a struct listing with json tags is source, not a
// copy-paste config. STATED LIMIT (D-14 #4): a config example deliberately
// mislabeled as another language evades this block check; the line rules
// still apply inside it. The root-marker heuristic in storelessConfig keeps
// prose and fragments out.
func lintCodeBlocks(path, content string) []Violation {
	var out []Violation
	lines := strings.Split(content, "\n")
	inBlock, blockStart := false, 0
	var block []string
	flush := func(start int, body []string) {
		if storelessConfig(strings.Join(body, "\n")) {
			out = append(out, Violation{Path: path, Line: start,
				Pattern: "config-example-without-declared-store",
				Text:    "whole-config example declares no storage.redis_url — the library refuses it at boot (QUOTA-CFG-001)"})
		}
	}
	indentStart, indentRun := 0, []string(nil)
	flushIndent := func() {
		if indentRun != nil {
			flush(indentStart, indentRun)
			indentRun = nil
		}
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if strings.HasPrefix(trimmed, "```") {
				if blockStart > 0 {
					flush(blockStart, block)
				}
				inBlock = false
				continue
			}
			block = append(block, line)
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			flushIndent()
			label := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
			if label == "" || strings.HasPrefix(label, "json") {
				inBlock, blockStart, block = true, i+1, nil
			} else {
				inBlock, blockStart, block = true, -1, nil // declared-language fence: tracked, not analysed
			}
			continue
		}
		// Indented code run (outside fences): 4+ spaces or a tab.
		if trimmed != "" && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")) {
			if indentRun == nil {
				indentStart = i + 1
			}
			indentRun = append(indentRun, line)
			continue
		}
		if trimmed != "" {
			flushIndent()
		}
	}
	flushIndent()
	if inBlock && blockStart > 0 {
		flush(blockStart, block)
	}
	return out
}

// Lint walks root and returns every violation plus THE SET of files it
// actually examined. T-G7/D-14 rule 3: the control binds to this effective
// set, so ANY mechanism that shrinks coverage — exemptions, skippedDirs, the
// extension filter, path globs — turns the control RED, not just edits to
// one frozen list.
func Lint(root string) (violations []Violation, scanned []string, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if !scannableSuffix(base) || skippedFile(base) {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned = append(scanned, path)
		content := string(raw)
		violations = append(violations, lintLines(path, content)...)
		switch {
		case strings.HasSuffix(base, ".md"):
			violations = append(violations, lintCodeBlocks(path, content)...)
		case strings.HasSuffix(base, ".json"):
			// T-G7: ANY shipped .json that reads as a whole quota-config
			// (root-marker heuristic) — not just quota-config* names, which
			// the ad-hoc attack evaded by renaming.
			if storelessConfig(content) {
				violations = append(violations, Violation{Path: path, Line: 1,
					Pattern: "config-example-without-declared-store",
					Text:    "shipped config template declares no storage.redis_url — the library refuses it at boot (QUOTA-CFG-001)"})
			}
		}
		return nil
	})
	return violations, scanned, err
}
