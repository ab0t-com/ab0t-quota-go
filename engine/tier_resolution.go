package engine

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/providers"
)

// ResolveTier is exposed for callers that want the resolved tier without
// running a full Check (e.g. middleware composing X-Tier-ID header).
func (e *Engine) ResolveTier(ctx context.Context, userID, orgID string) (string, error) {
	return e.Provider.GetTier(ctx, userID, orgID)
}

// SetProvider swaps the provider. Used by quota.Setup when the consumer
// later supplies a mesh lookup via SetLookup; safe for tests.
func (e *Engine) SetProvider(p providers.Provider) { e.Provider = p }
