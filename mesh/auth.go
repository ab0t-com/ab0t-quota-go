package mesh

import (
	"context"
	"net/http"
)

// AuthInjector mutates a request to add mesh-auth credentials. The
// default just sets `Authorization: Bearer {token}`.
type AuthInjector func(ctx context.Context, req *http.Request)

// BearerAuth returns an AuthInjector that sets a static bearer.
func BearerAuth(token string) AuthInjector {
	return func(_ context.Context, req *http.Request) {
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
}
