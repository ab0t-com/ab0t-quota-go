package counters

// K-1 (keyspace spec 20260721): the versioned counter keyspace — the ONE Go
// home of key shape, mirroring Python ab0t_quota/keyspace.py byte-for-byte.
//
//	v1: quota:{org}:{rk}<suffix>            (today; untagged)
//	v2: quota:v2:{<svc>/<org>}:{rk}<suffix> (braces literal — Redis hash tag)
//
// Cross-runtime oracle: conformance/keyspace_v2_vectors.json (ST-KEYSPACE-1).
// NOTE: Go dual-write/migration (ST-KEYSPACE-2) is NOT yet implemented — Go
// consumers stay (1,false); this file provides the shape seam and guards.

import (
	"fmt"
	"regexp"
	"strings"
)

var versionish = regexp.MustCompile(`^v[0-9]+$`)

// ValidateScope refuses a service/org value that cannot be embedded in a v2
// key — '{', '}', '/', ':' would corrupt the hash tag, and ^v[0-9]+$ collides
// with the version discriminator (spec §2.3/§2.4). Loud, never mangled.
func ValidateScope(value, what string) error {
	if value == "" {
		return fmt.Errorf("counters: %s must be non-empty for keyspace v2 keys", what)
	}
	if strings.ContainsAny(value, "{}/:") {
		return fmt.Errorf("counters: %s %q contains a forbidden character ({, }, /, :) — "+
			"it would corrupt the hash-tagged v2 key shape", what, value)
	}
	if versionish.MatchString(value) {
		return fmt.Errorf("counters: %s %q matches ^v[0-9]+$ — reserved as the keyspace "+
			"version discriminator (spec §2.4)", what, value)
	}
	return nil
}

// Keyspace is the declared key-shape state. The zero value is v1 single-shape,
// bit-identical to today.
type Keyspace struct {
	Service   string
	Version   int // 0 (absent) or 1 = v1; 2 = v2
	DualWrite bool
}

// NewKeyspace validates the declaration (four legal states, spec §3.1).
func NewKeyspace(service string, version int, dualWrite bool) (Keyspace, error) {
	if version == 0 {
		version = 1
	}
	if version != 1 && version != 2 {
		return Keyspace{}, fmt.Errorf("counters: storage.keyspace_version must be 1 or 2, got %d", version)
	}
	if service != "" {
		if err := ValidateScope(service, "service_name"); err != nil {
			return Keyspace{}, err
		}
	}
	if (version == 2 || dualWrite) && service == "" {
		return Keyspace{}, fmt.Errorf("counters: keyspace v2 keys carry a service segment — " +
			"declare service_name before keyspace_version=2 or keyspace_dual_write")
	}
	return Keyspace{Service: service, Version: version, DualWrite: dualWrite}, nil
}

func (k Keyspace) version() int {
	if k.Version == 0 {
		return 1
	}
	return k.Version
}

// Enabled reports whether this state departs from the (1,false) default —
// the engine routes keys through the keyspace seam only then, so the v1
// default code path stays byte-identical to pre-K-8 behaviour.
func (k Keyspace) Enabled() bool { return k.version() == 2 || k.DualWrite }

// SecondaryVersion is the non-authoritative shape maintained during dual
// (0 when not dual-writing) — mirrors Python Keyspace.secondary_version.
func (k Keyspace) SecondaryVersion() int {
	if !k.DualWrite {
		return 0
	}
	if k.version() == 1 {
		return 2
	}
	return 1
}

// PrimaryIsV2 reports whether the READ-authoritative shape is v2.
func (k Keyspace) PrimaryIsV2() bool { return k.version() == 2 }

// prefixV builds the shape for an explicit version. Scope validation is the
// ENGINE-BOUNDARY's job when the keyspace is enabled (engine.validateScopes,
// mirroring Python's per-request validate_scope raise) — builders here refuse
// via the erroring public methods; prefixV trusts a pre-validated org.
func (k Keyspace) prefixV(org, rk string, version int) string {
	if version == 1 {
		return "quota:" + org + ":" + rk
	}
	return "quota:v2:{" + k.Service + "/" + org + "}:" + rk
}

func (k Keyspace) prefix(org, rk string) (string, error) {
	if k.version() == 1 {
		return "quota:" + org + ":" + rk, nil
	}
	if err := ValidateScope(org, "org_id"); err != nil {
		return "", err
	}
	return "quota:v2:{" + k.Service + "/" + org + "}:" + rk, nil
}

// DualPair is a (primary, secondary) key pair; Secondary is "" outside dual.
type DualPair struct {
	P string // authoritative shape
	S string // the other shape, maintained during dual-write
}

// Pair builders — used by the engine when Enabled(). The suffix composes
// onto the versioned prefix so P and S differ only in shape.
func (k Keyspace) pair(org, rk, suffix string) DualPair {
	pr := DualPair{P: k.prefixV(org, rk, k.version()) + suffix}
	if sv := k.SecondaryVersion(); sv != 0 {
		pr.S = k.prefixV(org, rk, sv) + suffix
	}
	return pr
}

func (k Keyspace) GaugePair(org, rk string) DualPair { return k.pair(org, rk, ":gauge") }
func (k Keyspace) UserPair(org, rk, uid string) DualPair {
	return k.pair(org, rk, ":gauge:user:"+uid)
}
func (k Keyspace) SeqUserPair(org, rk, uid string) DualPair {
	return k.pair(org, rk, ":gauge:seq:user:"+uid)
}
func (k Keyspace) IdemPair(org, rk, key string) DualPair {
	if key == "" {
		key = "__unused__"
	}
	return k.pair(org, rk, ":idem:"+key)
}
func (k Keyspace) AccPair(org, rk, period string) DualPair {
	return k.pair(org, rk, ":acc:"+period)
}
func (k Keyspace) RatePair(org, rk string) DualPair { return k.pair(org, rk, ":rate") }

// DeprecatedScopeKey is the ONE home of the pre-P5.3 scope shapes
// (quota:<head>:<rk>:<scope…>, finding QG-03) — retained until D-13
// authorizes deletion; the engine provably never writes them
// (engine/canonical_keys_pin_test.go). NEW call sites are FORBIDDEN: the
// K-10 construction census pins the existing five files shrink-only
// (D-KS-11).
func DeprecatedScopeKey(prefix KeyPrefix, head string, parts ...string) string {
	return prefix.Build(append([]string{head}, parts...)...)
}

// ClassifyV1CounterKey returns (org, rk, tail) for a v1 COUNTER key, else
// ok=false — the migration/straggler classifier (Python
// keyspace_migration.classify_v1_counter_key, ported).
func ClassifyV1CounterKey(key string) (org, rk, tail string, ok bool) {
	if !strings.HasPrefix(key, "quota:") || strings.HasPrefix(key, "quota:v2:") {
		return "", "", "", false
	}
	parts := strings.Split(key, ":")
	if len(parts) < 4 || !strings.Contains(parts[2], ".") || versionish.MatchString(parts[1]) {
		return "", "", "", false
	}
	return parts[1], parts[2], strings.Join(parts[3:], ":"), true
}

// GaugeKey — quota[:v2:{svc/org}]:{rk}:gauge (vector key "gauge").
func (k Keyspace) GaugeKey(org, rk string) (string, error) {
	p, err := k.prefix(org, rk)
	if err != nil {
		return "", err
	}
	return p + ":gauge", nil
}

// UserKey — …:gauge:user:{uid}.
func (k Keyspace) UserKey(org, rk, uid string) (string, error) {
	p, err := k.prefix(org, rk)
	if err != nil {
		return "", err
	}
	return p + ":gauge:user:" + uid, nil
}

// UserSeqKey — …:gauge:seq:user:{uid} (QI-05.1 create generation).
func (k Keyspace) UserSeqKey(org, rk, uid string) (string, error) {
	p, err := k.prefix(org, rk)
	if err != nil {
		return "", err
	}
	return p + ":gauge:seq:user:" + uid, nil
}

// IdemKey — …:idem:{key}; empty key = the ":idem:__unused__" placeholder.
func (k Keyspace) IdemKey(org, rk, key string) (string, error) {
	p, err := k.prefix(org, rk)
	if err != nil {
		return "", err
	}
	if key == "" {
		return p + ":idem:__unused__", nil
	}
	return p + ":idem:" + key, nil
}

// IdemGenKey — …:idemgen:{key} (generation-scoped teardown claim HASH).
func (k Keyspace) IdemGenKey(org, rk, key string) (string, error) {
	p, err := k.prefix(org, rk)
	if err != nil {
		return "", err
	}
	if key == "" {
		return p + ":idemgen:__unused__", nil
	}
	return p + ":idemgen:" + key, nil
}

// AccKey — …:acc:{period}.
func (k Keyspace) AccKey(org, rk, period string) (string, error) {
	p, err := k.prefix(org, rk)
	if err != nil {
		return "", err
	}
	return p + ":acc:" + period, nil
}

// RateKey — …:rate.
func (k Keyspace) RateKey(org, rk string) (string, error) {
	p, err := k.prefix(org, rk)
	if err != nil {
		return "", err
	}
	return p + ":rate", nil
}

// RecentKey — reconciler activity guard. v1: quota:reconcile:recent:{org};
// v2 gains the svc scope: quota:v2:{svc/org}:reconcile:recent (spec §7 #5).
func (k Keyspace) RecentKey(org string) (string, error) {
	if k.version() == 1 {
		return "quota:reconcile:recent:" + org, nil
	}
	if err := ValidateScope(org, "org_id"); err != nil {
		return "", err
	}
	return "quota:v2:{" + k.Service + "/" + org + "}:reconcile:recent", nil
}

// MarkerKey — the per-service migration marker (spec §3.1).
func MarkerKey(service string) string {
	return "quota:keyspace:meta:" + service
}
