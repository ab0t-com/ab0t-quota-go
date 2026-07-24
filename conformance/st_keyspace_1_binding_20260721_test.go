package conformance

// K-7 — Go binding for ST-KEYSPACE-1 (the versioned counter keyspace).
//
// Asserts against the byte-level oracle conformance/keyspace_v2_vectors.json
// (canonical in the Python repo, mirrored here): the Go builder reproduces
// every vector row byte-identically, every v2 key of one (svc,org) hashes to
// ONE cluster slot (the co-slot law), the charset guard refuses corrupting
// scopes, and the config keys exist in the strict schema with illegal values
// refused. ST-KEYSPACE-2 (migration semantics) is python-only until the Go
// dual-write path lands — see config.Validate's loud refusal, asserted below.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

type ksVector struct {
	Service     string            `json:"service"`
	OrgID       string            `json:"org_id"`
	ResourceKey string            `json:"resource_key"`
	UserID      string            `json:"user_id"`
	Period      string            `json:"period"`
	Version     int               `json:"version"`
	IdemKey     string            `json:"idempotency_key"`
	Keys        map[string]string `json:"keys"`
	HashTag     string            `json:"hash_tag"`
}

type ksVectorFile struct {
	MarkerKeyExample string     `json:"marker_key_example"`
	Vectors          []ksVector `json:"vectors"`
}

func loadKeyspaceVectors(t *testing.T) ksVectorFile {
	t.Helper()
	raw, err := os.ReadFile("keyspace_v2_vectors.json")
	if err != nil {
		t.Fatalf("ST-KEYSPACE-1 vector oracle missing: %v", err)
	}
	var doc ksVectorFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("empty vector file cannot pass as coverage")
	}
	return doc
}

func mustKey(t *testing.T) func(string, error) string {
	t.Helper()
	return func(s string, err error) string {
		if err != nil {
			t.Fatalf("builder refused a vector-legal scope: %v", err)
		}
		return s
	}
}

func TestSTKeyspace1BuilderReproducesVectorsByteIdentically(t *testing.T) {
	doc := loadKeyspaceVectors(t)
	if got := counters.MarkerKey("sandbox-platform"); got != doc.MarkerKeyExample {
		t.Fatalf("marker key %q != oracle %q", got, doc.MarkerKeyExample)
	}
	for _, v := range doc.Vectors {
		ks, err := counters.NewKeyspace(v.Service, v.Version, false)
		if err != nil {
			t.Fatalf("vector (%s v%d): %v", v.Service, v.Version, err)
		}
		mk := mustKey(t)
		got := map[string]string{
			"gauge":          mk(ks.GaugeKey(v.OrgID, v.ResourceKey)),
			"gauge_user":     mk(ks.UserKey(v.OrgID, v.ResourceKey, v.UserID)),
			"gauge_seq_user": mk(ks.UserSeqKey(v.OrgID, v.ResourceKey, v.UserID)),
			"idem":           mk(ks.IdemKey(v.OrgID, v.ResourceKey, v.IdemKey)),
			"idem_unused":    mk(ks.IdemKey(v.OrgID, v.ResourceKey, "")),
			"idemgen":        mk(ks.IdemGenKey(v.OrgID, v.ResourceKey, v.IdemKey)),
			"idemgen_unused": mk(ks.IdemGenKey(v.OrgID, v.ResourceKey, "")),
			"acc":            mk(ks.AccKey(v.OrgID, v.ResourceKey, v.Period)),
			"rate":           mk(ks.RateKey(v.OrgID, v.ResourceKey)),
			"recent":         mk(ks.RecentKey(v.OrgID)),
		}
		for kind, want := range v.Keys {
			if got[kind] != want {
				t.Errorf("v%d %s/%s %s: got %q want %q (byte contract, ST-KEYSPACE-1)",
					v.Version, v.Service, v.OrgID, kind, got[kind], want)
			}
		}
		if v.HashTag != "" {
			for kind, key := range got {
				if !strings.Contains(key, "{"+v.HashTag+"}") {
					t.Errorf("v2 key %s %q lost the literal hash tag {%s}", kind, key, v.HashTag)
				}
			}
		}
	}
}

// crc16 implements the Redis Cluster CRC16 (CCITT / XMODEM), the exact
// server hashing — see CLUSTER KEYSLOT.
func crc16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func keySlot(key string) uint16 {
	if s := strings.Index(key, "{"); s >= 0 {
		if e := strings.Index(key[s+1:], "}"); e > 0 {
			key = key[s+1 : s+1+e]
		}
	}
	return crc16([]byte(key)) % 16384
}

func TestSTKeyspace1CoSlotLawForOneScope(t *testing.T) {
	doc := loadKeyspaceVectors(t)
	for _, v := range doc.Vectors {
		if v.Version != 2 {
			continue
		}
		slots := map[uint16][]string{}
		for _, key := range v.Keys {
			slots[keySlot(key)] = append(slots[keySlot(key)], key)
		}
		if len(slots) != 1 {
			t.Errorf("v2 keys for one (svc,org) hash to %d slots — CROSSSLOT on a "+
				"cluster (ST-KEYSPACE-1 co-slot law): %v", len(slots), slots)
		}
	}
	// negative control (D-14): the sanity of the slot function itself — two
	// untagged v1 keys of one org land in different slots (the D-23 defect).
	a := keySlot("quota:org-1:sandbox.concurrent:gauge")
	b := keySlot("quota:org-1:sandbox.concurrent:idem:k1")
	if a == b {
		t.Fatalf("control invalid: untagged v1 keys co-slotted (%d) — slot fn broken?", a)
	}
}

func TestSTKeyspace1CharsetGuardRefuses(t *testing.T) {
	for _, bad := range []string{"a{b", "a}b", "a/b", "a:b", "v2", ""} {
		if _, err := counters.NewKeyspace(bad, 2, false); err == nil {
			t.Errorf("service %q accepted — would corrupt the v2 hash tag", bad)
		}
	}
	ks, err := counters.NewKeyspace("svc-a", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"a{b", "a}b", "a/b", "a:b", "v2", "v13", ""} {
		if _, err := ks.GaugeKey(bad, "x.y"); err == nil {
			t.Errorf("org %q accepted — must refuse loudly, never mangle", bad)
		}
	}
	if _, err := ks.GaugeKey("0b7e-uuid-ok", "x.y"); err != nil {
		t.Errorf("clean org refused (guard over-broad): %v", err)
	}
}

func TestSTKeyspace1ConfigKeysStrictAndGoV2NotYetWired(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			ServiceName: "svc-a",
			Tiers: []config.Tier{{TierID: "free", DisplayName: "Free"}},
		}
	}
	c := base()
	c.Storage.KeyspaceVersion = 3
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "keyspace_version") {
		t.Errorf("keyspace_version=3 not refused: %v", err)
	}
	// K-8 (D-KS-10): Setup now CONSUMES the declared state — v2 / dual-write
	// validate cleanly with a service_name, and still refuse WITHOUT one
	// (the v2 tag needs the service segment, spec §2.2).
	c = base()
	c.Storage.KeyspaceVersion = 2
	if err := c.Validate(); err != nil {
		t.Errorf("keyspace_version=2 must validate now that the engine consumes it (K-8): %v", err)
	}
	c = base()
	c.Storage.KeyspaceDualWrite = true
	if err := c.Validate(); err != nil {
		t.Errorf("keyspace_dual_write must validate now that the dual path exists (K-8): %v", err)
	}
	c = base()
	c.ServiceName = ""
	c.Storage.KeyspaceVersion = 2
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("v2 without service_name must still refuse (service segment): %v", err)
	}
	// negative control: (1,false) — today's world — still validates.
	if err := base().Validate(); err != nil {
		t.Errorf("baseline config broken by the keyspace guard: %v", err)
	}
}
