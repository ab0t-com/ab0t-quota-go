package main

import (
	"bytes"
	"strings"
	"testing"
)

// runRoot wires the root cmd and captures stdout/stderr for assertion.
func runRoot(t *testing.T, args ...string) (out, errOut string, err error) {
	t.Helper()
	root := newRoot()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	err = root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestRoot_Help(t *testing.T) {
	out, _, err := runRoot(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"subscribe-events", "events", "replay", "backfill", "delete-user", "capabilities"} {
		if !strings.Contains(out, sub) {
			t.Errorf("--help missing %q", sub)
		}
	}
}

func TestEvents_RequiresFilter(t *testing.T) {
	_, _, err := runRoot(t, "events")
	if err == nil {
		t.Error("expected error when no --user/--status")
	}
}

func TestDeleteUser_RequiresUserID(t *testing.T) {
	_, _, err := runRoot(t, "delete-user")
	if err == nil {
		t.Error("expected error without --user-id")
	}
}

func TestReplay_RequiresFlags(t *testing.T) {
	_, _, err := runRoot(t, "replay")
	if err == nil {
		t.Error("expected error without --file/--target/--secret")
	}
}

func TestSplitEvents_JSONLAndArray(t *testing.T) {
	// JSONL
	events, err := splitEvents([]byte(`{"event_id":"e1"}` + "\n" + `{"event_id":"e2"}`))
	if err != nil || len(events) != 2 {
		t.Fatalf("got %d events err=%v", len(events), err)
	}
	// JSON array
	events, err = splitEvents([]byte(`[{"id":1},{"id":2},{"id":3}]`))
	if err != nil || len(events) != 3 {
		t.Errorf("array got %d events err=%v", len(events), err)
	}
}
