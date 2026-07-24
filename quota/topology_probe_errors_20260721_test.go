package quota

// T-G3 REDs (GO-05, pack 20260721_shared_lib_declared_not_discovered; the Go
// leg of the parent incident pack's T1/T4/T5 — 20260721_redis_preflight_
// misdiagnoses_auth_failure). NEW FILE beside quota/topology_guard_d71_test.go,
// which is LOCKED (harness design §1.2/§6.1) and is not edited here.
//
// The incident's mechanism: ProbeClusterEnabled folded EVERY error into
// found=false, so an authentication failure became "topology unverifiable —
// set storage.redis_cluster_confirmed_disabled". An operator who follows that
// advice against a real cluster turns the safety rail off. Contract:
//   1. an auth/connection failure during the probe is NEVER a topology
//      verdict, and no operator assertion masks it;
//   2. a data-plane CROSSSLOT observation is a DEFINITIVE cluster verdict,
//      reachable without any admin command (INFO/CLUSTER may be ACL-denied);
//   3. NOPERM (least-privilege ACL denying the admin probes) remains the
//      genuine absent-signal case — the assertion flag applies there;
//   4. a data-plane multi-key SUCCESS is NOT proof of single-node (a
//      splitting proxy can mask a cluster) — unverifiable stays refused.
//
// NOT touched here (T-13/D-2 boundary): setup.go's degrade-on-unreachable at
// the ping stage. Same judgement, different question.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// existsProber extends the locked file's fakeProber with a data-plane
// EXISTS answer, so the CROSSSLOT probe can be driven with canned errors.
type existsProber struct {
	fakeProber
	existsErr string
}

func (f existsProber) Exists(ctx context.Context, keys ...string) *redis.IntCmd {
	if f.existsErr != "" {
		return redis.NewIntResult(0, errFromString(f.existsErr))
	}
	return redis.NewIntResult(0, nil)
}

func errFromString(s string) error { return &stringErr{s} }

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }

func TestAuthFailureIsNotATopologyVerdict(t *testing.T) {
	ctx := context.Background()
	for name, p := range map[string]fakeProber{
		"NOAUTH": {infoErr: "NOAUTH Authentication required.",
			clusterErr: "NOAUTH Authentication required."},
		"WRONGPASS": {infoErr: "WRONGPASS invalid username-password pair or user is disabled.",
			clusterErr: "WRONGPASS invalid username-password pair or user is disabled."},
		"connection": {infoErr: "dial tcp 10.0.0.9:6379: connect: connection refused",
			clusterErr: "dial tcp 10.0.0.9:6379: connect: connection refused"},
	} {
		t.Run(name, func(t *testing.T) {
			topo, detail := CheckRedisClusterTopology(ctx, p, false)
			if topo == TopologyUnknown {
				t.Errorf("GO-05: an auth/connection failure was folded into the UNKNOWN topology "+
					"verdict (detail: %s) — the refusal would tell the operator to set "+
					"redis_cluster_confirmed_disabled when the real problem is credentials/"+
					"reachability, the exact misdiagnosis that caused the outage", detail)
			}
			if topo == TopologySingleNode || topo == TopologyCluster {
				t.Errorf("a probe that could not run must not yield a topology at all, got %q", topo)
			}

			// Parent T3: the operator assertion must NOT mask a probe failure —
			// confirmed_disabled exists for an ABSENT signal, not a failed probe.
			topo, detail = CheckRedisClusterTopology(ctx, p, true)
			if topo == TopologySingleNode {
				t.Errorf("GO-05/T3: redis_cluster_confirmed_disabled MASKED an auth/connection "+
					"probe failure into an asserted single-node (detail: %s) — safety rails off", detail)
			}
		})
	}
}

func TestCrossslotIsDefinitiveCluster(t *testing.T) {
	ctx := context.Background()
	// The admin probes are ACL-denied (least privilege) — but the DATA PLANE
	// says CROSSSLOT, which is the very failure the guard exists to prevent.
	p := existsProber{
		fakeProber: fakeProber{
			infoErr:    "NOPERM this user has no permissions to run the 'info' command",
			clusterErr: "NOPERM this user has no permissions to run the 'cluster|info' command",
		},
		existsErr: "CROSSSLOT Keys in request don't hash to the same slot",
	}
	topo, detail := CheckRedisClusterTopology(ctx, p, false)
	if topo != TopologyCluster {
		t.Errorf("a data-plane CROSSSLOT is a DEFINITIVE cluster signal (no admin command "+
			"needed) — got %q (detail: %s)", topo, detail)
	}
	// And no operator assertion overrides an observed CROSSSLOT.
	topo, detail = CheckRedisClusterTopology(ctx, p, true)
	if topo != TopologyCluster {
		t.Errorf("redis_cluster_confirmed_disabled must NOT override an OBSERVED CROSSSLOT — "+
			"got %q (detail: %s)", topo, detail)
	}
}

// Control (pins parent T5's split — passes before and after): NOPERM on the
// admin probes with a working data plane is the GENUINE absent-signal case.
// The operator assertion is exactly the remedy there.
func TestNopermIsAbsentSignalNotProbeFailure(t *testing.T) {
	ctx := context.Background()
	p := existsProber{
		fakeProber: fakeProber{
			infoErr:    "NOPERM this user has no permissions to run the 'info' command",
			clusterErr: "NOPERM this user has no permissions to run the 'cluster|info' command",
		},
		// existsErr empty — multi-key EXISTS works (single node or masked).
	}
	if topo, detail := CheckRedisClusterTopology(ctx, p, false); topo != TopologyUnknown {
		t.Errorf("ACL-denied admin probes with a working data plane = absent signal = UNKNOWN "+
			"(refuse unless asserted), got %q (%s)", topo, detail)
	}
	if topo, detail := CheckRedisClusterTopology(ctx, p, true); topo != TopologySingleNode {
		t.Errorf("the operator assertion is the documented remedy for the ACL-denied case, "+
			"got %q (%s)", topo, detail)
	}
}

// Control (documents why the locked D-71 miniredis refusal still holds): a
// data-plane multi-key SUCCESS is NOT proof of single-node — a splitting
// proxy (twemproxy-style) can pass per-key EXISTS while the multi-key Lua
// still CROSSSLOTs. Success falls through to the admin probes.
func TestDataPlaneSuccessIsNotProofOfSingleNode(t *testing.T) {
	ctx := context.Background()
	p := existsProber{
		fakeProber: fakeProber{
			infoErr:    "ERR unsupported",
			clusterErr: "ERR unknown command 'cluster'",
		},
	}
	if topo, detail := CheckRedisClusterTopology(ctx, p, false); topo != TopologyUnknown {
		t.Errorf("EXISTS succeeding proves nothing about the Lua path — want UNKNOWN "+
			"(unverifiable fails closed), got %q (%s)", topo, detail)
	}
}

// crc16 is the Redis Cluster key-hashing CRC (XMODEM, poly 0x1021, init 0) —
// implemented locally so the slot-difference claim is verified, not assumed.
func crc16(b []byte) uint16 {
	var crc uint16
	for _, x := range b {
		crc ^= uint16(x) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// TestProbeKeysHashToDifferentSlots pins the load-bearing property of the
// data-plane probe: its two keys' hash tags land in DIFFERENT cluster slots,
// so a multi-key EXISTS on them must CROSSSLOT on any real cluster.
// (Green-time test — the constants did not exist at RED.)
func TestProbeKeysHashToDifferentSlots(t *testing.T) {
	tag := func(key string) string {
		i := strings.Index(key, "{")
		j := strings.Index(key, "}")
		if i < 0 || j <= i+1 {
			t.Fatalf("probe key %q carries no hash tag — its slot would depend on the whole key", key)
		}
		return key[i+1 : j]
	}
	slotA := crc16([]byte(tag(topologyProbeKeyA))) % 16384
	slotB := crc16([]byte(tag(topologyProbeKeyB))) % 16384
	if slotA == slotB {
		t.Fatalf("probe keys hash to the SAME slot (%d) — the CROSSSLOT probe would never fire", slotA)
	}
	// Pin the documented values ("foo"→12182, "bar"→5061, per redis docs).
	if slotA != 12182 || slotB != 5061 {
		t.Errorf("slots = (%d, %d), want the documented (12182, 5061) — if the tags changed, update this pin deliberately", slotA, slotB)
	}
}

// TestGoSatisfiesSTTopology1AuthFailureExtension binds the ST-TOPOLOGY-1
// extension (harness design §5.2): the probe-failed refusal carries the
// contract tokens, so both runtimes are held to the same text.
//
// The CANONICAL scenarios.json lives in the Python repo (sync-checked
// byte-identical here), so the JSON extension itself is the canonical
// owner's edit — QUEUED for the Python lane (see information_laneG2 +
// work log). Until it lands, the tokens are pinned LOCALLY below; once the
// item declares auth_failure_error_must_contain, the declared list wins and
// this test holds Go to it.
func TestGoSatisfiesSTTopology1AuthFailureExtension(t *testing.T) {
	tokens := []string{"credential", "NOT a topology verdict", "never ran"} // local pin
	raw, err := os.ReadFile("../conformance/scenarios.json")
	if err != nil {
		t.Fatalf("read scenarios.json: %v", err)
	}
	var doc struct {
		Structural []struct {
			ID                          string   `json:"id"`
			AuthFailureErrorMustContain []string `json:"auth_failure_error_must_contain"`
		} `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse scenarios.json: %v", err)
	}
	declared := false
	for _, it := range doc.Structural {
		if it.ID == "ST-TOPOLOGY-1" && len(it.AuthFailureErrorMustContain) > 0 {
			tokens = it.AuthFailureErrorMustContain
			declared = true
		}
	}
	if !declared {
		t.Log("ST-TOPOLOGY-1 does not yet declare auth_failure_error_must_contain — the canonical " +
			"(Python-repo) extension is queued; asserting the Go-side tokens from the local pin")
	}
	msg := TopologyError(TopologyProbeFailed, "INFO cluster probe failed (auth): NOAUTH Authentication required.").Error()
	for _, tok := range tokens {
		if !strings.Contains(msg, tok) {
			t.Errorf("probe-failed refusal must contain declared token %q; got: %s", tok, msg)
		}
	}
	// And the refusal must NOT point at the topology assertion as a remedy.
	if strings.Contains(msg, "set storage.redis_cluster_confirmed_disabled: true") {
		t.Errorf("the probe-failed refusal must not advise the topology assertion — that is the misdiagnosis")
	}
}
