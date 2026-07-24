// ST-CLI-1 — the Go binding half (Python half:
// tests/test_tool_doctor_20260721.py / test_tool_provision_20260721.py in the
// canonical repo). Pins the verb triad, the shared exit taxonomy, the report
// schemas, the emitted-artifact tokens, and — per D-14 — proves the
// provision conformance instruments can go RED on planted offenders.
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type stCli1 struct {
	ID           string            `json:"id"`
	PythonVerbs  []string          `json:"python_verbs"`
	GoVerbs      []string          `json:"go_verbs"`
	ExitTaxonomy map[string]string `json:"exit_taxonomy"`
	ReportSchema string            `json:"report_schema"`
	PostureSchem string            `json:"posture_schema"`
	EmitKinds    []string          `json:"emit_kinds"`
	EmitMust     map[string][]string `json:"emit_must_contain"`
	SideEffects  map[string]string `json:"side_effects"`
}

func loadStCli1(t *testing.T) stCli1 {
	t.Helper()
	raw, err := os.ReadFile("../../conformance/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Structural []json.RawMessage `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, item := range doc.Structural {
		var s stCli1
		if err := json.Unmarshal(item, &s); err != nil {
			t.Fatal(err)
		}
		if s.ID == "ST-CLI-1" {
			return s
		}
	}
	t.Fatal("ST-CLI-1 not declared in conformance/scenarios.json")
	return stCli1{}
}

func TestStCli1VerbsExist(t *testing.T) {
	sc := loadStCli1(t)
	root := newRoot()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, v := range sc.GoVerbs {
		if !have[v] {
			t.Errorf("ST-CLI-1 pins verb %q for go; quotactl does not have it", v)
		}
	}
	if have["setup"] {
		t.Error("`setup` must NOT be a verb (ticket §6.1: quota.Setup is the " +
			"library's own entry point; the verb is `provision`)")
	}
}

func TestStCli1ExitTaxonomyPinned(t *testing.T) {
	sc := loadStCli1(t)
	if len(sc.ExitTaxonomy) != 5 {
		t.Fatalf("taxonomy must have 5 codes, got %d", len(sc.ExitTaxonomy))
	}
	checks := map[int]string{
		exitOK: "ok", exitGate: "gate", exitConfig: "config",
		exitReach: "unreachable", exitInternal: "internal",
	}
	for code, prefix := range checks {
		meaning, ok := sc.ExitTaxonomy[strings.TrimSpace(jsonInt(code))]
		if !ok || !strings.HasPrefix(meaning, prefix) {
			t.Errorf("exit %d: scenario meaning %q does not pin prefix %q",
				code, meaning, prefix)
		}
	}
}

func jsonInt(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestStCli1ReportSchemasPinned(t *testing.T) {
	sc := loadStCli1(t)
	if sc.ReportSchema != reportSchema {
		t.Errorf("report schema drift: scenario %q vs code %q", sc.ReportSchema, reportSchema)
	}
	if sc.PostureSchem != postureSchema {
		t.Errorf("posture schema drift: scenario %q vs code %q", sc.PostureSchem, postureSchema)
	}
}

func TestStCli1EmittedArtifactsCarryThePinnedTokens(t *testing.T) {
	sc := loadStCli1(t)
	for _, kind := range sc.EmitKinds {
		text, err := emitArtifact(kind, false)
		if err != nil {
			t.Fatalf("emit %s: %v", kind, err)
		}
		for _, token := range sc.EmitMust[kind] {
			if !strings.Contains(text, token) {
				t.Errorf("emit %s: missing cross-runtime pinned token %q", kind, token)
			}
		}
	}
}

func TestStCli1HonestAsymmetryStated(t *testing.T) {
	// D-8: Go's doctor runs full Setup and must SAY so — in the scenario, in
	// the command help, and in the report's side-effect statement.
	sc := loadStCli1(t)
	if !strings.Contains(sc.SideEffects["go"], "Setup") {
		t.Error("scenario side_effects.go must state doctor runs full Setup")
	}
	cmd := newDoctorCmd()
	if !strings.Contains(cmd.Long, "NOT read-only") {
		t.Error("doctor's help must state it is NOT read-only (D-8)")
	}
	joined := strings.Join(doctorSideEffects, " ")
	if !strings.Contains(joined, "Setup") || !strings.Contains(joined, "CREATE") {
		t.Error("doctorSideEffects must state Setup runs and may create tables")
	}
}

// --- D-14: the instruments must be seen to fail ----------------------------

func TestProvisionSelfCheckGoesRedOnPlantedEvictingPolicy(t *testing.T) {
	orig := requiredMaxmemoryPolicy
	defer func() { requiredMaxmemoryPolicy = orig }()
	requiredMaxmemoryPolicy = "allkeys-lru"
	if _, err := emitArtifact("compose", false); err == nil {
		t.Fatal("planted evicting policy was NOT refused — the self-check cannot fail")
	}
}

func TestConformanceCheckerGoesRedOnPlantedOffenders(t *testing.T) {
	plants := map[string]func(string) string{
		"compose":   func(s string) string { return strings.ReplaceAll(s, requiredMaxmemoryPolicy, "allkeys-lru") },
		"acl":       func(s string) string { return strings.ReplaceAll(s, "~"+keyspacePattern, "~*") },
		"terraform": func(s string) string { return strings.ReplaceAll(s, "ab0t_quota_outbox", "wrong") },
		"iam":       func(s string) string { return strings.ReplaceAll(s, "dynamodb:Scan", "dynamodb:Nope") },
	}
	for kind, plant := range plants {
		text, err := emitArtifact(kind, false)
		if err != nil {
			t.Fatalf("emit %s: %v", kind, err)
		}
		broken := plant(text)
		if broken == text {
			t.Fatalf("%s: the plant did not change the artifact (dead control)", kind)
		}
		if problems := checkArtifactConformance(kind, broken); len(problems) == 0 {
			t.Errorf("%s: planted offender NOT detected — the verifier cannot fail", kind)
		}
	}
}

func TestLocalDockerArgsDeriveFromRegistry(t *testing.T) {
	args := strings.Join(localDockerArgs(7001, "x"), " ")
	for _, want := range []string{"--maxmemory-policy", requiredMaxmemoryPolicy,
		"--appendonly", requiredAppendonly, "7001:6379", redisImage} {
		if !strings.Contains(args, want) {
			t.Errorf("local docker args missing %q: %s", want, args)
		}
	}
}
