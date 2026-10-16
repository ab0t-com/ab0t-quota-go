package main

// Ticket 20261016 review, findings C-17 / C-18 / C-19 (lane C, re-verified):
//
//   - replay/backfill took their --secret DEFAULT from AB0T_AUTH_WEBHOOK_SECRET
//     at construction, so `quotactl replay --help` printed the live secret
//     (pflag shows non-empty defaults) — replay.go:72, backfill.go:89.
//   - replay/backfill exited 0 when the receiver answered 401/404/500 (they only
//     printed the status), and backfill ran with no --target / --secret.
//   - delete-user / events run against a fresh in-memory stub
//     (store_from_env.go:10-12): delete-user printed "deleted 0 rows" and exited
//     0 for a GDPR request while the service's real ledger kept the data.
//
// Each test is RED on v0.1.9.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const liveSecret = "sekrit-do-not-print-7f3e"

func TestHelp_NeverPrintsTheWebhookSecret_20261016(t *testing.T) {
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", liveSecret)
	for _, verb := range []string{"replay", "backfill"} {
		out, errOut, _ := runRoot(t, verb, "--help")
		if strings.Contains(out+errOut, liveSecret) {
			t.Errorf("`quotactl %s --help` printed the live AB0T_AUTH_WEBHOOK_SECRET", verb)
		}
	}
}

func receiverAnswering(code int, calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"detail":"invalid signature"}`))
	}))
}

func TestReplay_NonZeroWhenReceiverRefuses_20261016(t *testing.T) {
	var calls atomic.Int32
	srv := receiverAnswering(http.StatusUnauthorized, &calls)
	defer srv.Close()
	f := t.TempDir() + "/ev.jsonl"
	if err := os.WriteFile(f, []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runRoot(t, "replay", "--file", f, "--target", srv.URL, "--secret", "x")
	if err == nil || calls.Load() != 1 {
		t.Fatalf("receiver answered 401 but replay succeeded (err=%v calls=%d)", err, calls.Load())
	}
}

func TestBackfill_NonZeroWhenReceiverRefuses_20261016(t *testing.T) {
	var calls atomic.Int32
	srv := receiverAnswering(http.StatusNotFound, &calls)
	defer srv.Close()
	f := t.TempDir() + "/users.csv"
	if err := os.WriteFile(f, []byte("u1,o1\nu2,o2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runRoot(t, "backfill", "--input", f, "--target", srv.URL, "--secret", "x")
	if err == nil || calls.Load() != 2 {
		t.Fatalf("receiver answered 404 but backfill succeeded (err=%v calls=%d)", err, calls.Load())
	}
	if !strings.Contains(err.Error(), "2 of 2") {
		t.Errorf("error should count the failures: %v", err)
	}
}

func TestBackfill_RequiresTargetAndSecret_20261016(t *testing.T) {
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "")
	f := t.TempDir() + "/users.csv"
	_ = os.WriteFile(f, []byte("u1,o1\n"), 0o600)
	if _, _, err := runRoot(t, "backfill", "--input", f, "--secret", "x"); err == nil {
		t.Error("no --target: want an error")
	}
	if _, _, err := runRoot(t, "backfill", "--input", f, "--target", "http://127.0.0.1:1"); err == nil {
		t.Error("no --secret and no AB0T_AUTH_WEBHOOK_SECRET: want an error")
	}
	// --dry-run needs neither.
	if _, _, err := runRoot(t, "backfill", "--input", f, "--dry-run"); err != nil {
		t.Errorf("--dry-run: %v", err)
	}
}

// A GDPR deletion the CLI cannot perform must not report success.
func TestDeleteUser_RefusesInsteadOfFakeSuccess_20261016(t *testing.T) {
	out, _, err := runRoot(t, "delete-user", "--user-id", "u-gdpr")
	if err == nil {
		t.Fatalf("delete-user exited 0 against an in-memory stub; output: %q", out)
	}
	if strings.Contains(out, "deleted 0 rows") {
		t.Errorf("printed a fake deletion count: %q", out)
	}
	if !strings.Contains(err.Error(), "DeleteUser") {
		t.Errorf("error should say how to delete for real: %v", err)
	}
}

func TestEvents_RefusesInsteadOfEmptyResult_20261016(t *testing.T) {
	if _, _, err := runRoot(t, "events", "--status", "failed_permanent"); err == nil {
		t.Fatal("events exited 0 with an empty in-memory stub (looks like 'no failures')")
	}
}
