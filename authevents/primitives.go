package authevents

// ComposeCreditDedupKey builds a business-dedup key for credit grants
// according to the policy. Used by the lib's default handler and exposed
// for consumer custom handlers that want to share the same dedup semantics.
//
// Policies (matches Python lib v0.5.2):
//
//	per_user_per_tier (default) — credit_granted:user:{user_id}:{tier_id}
//	per_org_per_tier            — credit_granted:org:{org_id}:{tier_id}
//	per_user_global             — credit_granted:user:{user_id}
//	per_org_global              — credit_granted:org:{org_id}
//
// Wire-level parity with the Python lib is pinned: the default policy's
// key shape MUST equal `credit_granted:user:{user_id}:{tier_id}` so a
// mixed Python/Go shared-Redis deployment dedups correctly.
func ComposeCreditDedupKey(policy, userID, orgID, tierID string) string {
	const prefix = "credit_granted"
	switch policy {
	case "per_org_per_tier":
		return prefix + ":org:" + orgID + ":" + tierID
	case "per_user_global":
		return prefix + ":user:" + userID
	case "per_org_global":
		return prefix + ":org:" + orgID
	default: // per_user_per_tier
		return prefix + ":user:" + userID + ":" + tierID
	}
}

// PinStore is the user_id → billing org_id pinning interface.
// Three implementations: memory (tests), redis (bridge mode), ddb
// (mesh services). Stub backends live in pinstore.go for v0.1.0.
type PinStore interface {
	Get(userID string) (orgID string, err error)
	Set(userID, orgID, source string) error
}

// NoopPinStore is a sentinel PinStore that always returns empty + nil.
// Used when no real store is available.
type NoopPinStore struct{}

func (NoopPinStore) Get(string) (string, error)            { return "", nil }
func (NoopPinStore) Set(string, string, string) error      { return nil }
