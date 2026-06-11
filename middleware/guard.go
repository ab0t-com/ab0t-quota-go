package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/providers"
)

// Identity extracts user/org identity from the request. Usually pulls from
// JWT claims set by upstream auth middleware. Consumer-supplied.
type Identity func(r *http.Request) (userID, orgID string, err error)

// ResourceRouter maps a request to a resource_key + optional Cost. Return
// "" resource to skip the guard for this request (e.g., health checks).
type ResourceRouter func(r *http.Request) (resourceKey string, cost float64)

// GuardConfig wires the middleware.
type GuardConfig struct {
	Engine       *engine.Engine
	Identity     Identity
	Router       ResourceRouter
	Exempt       []string         // path prefixes to skip entirely (e.g. "/healthz")
	FailOpen     bool             // engine error → allow (true) or deny (false)
	OnWarn       func(*http.Request, engine.Result)
	OnDecision   func(*http.Request, engine.Result) // every check, allowed or denied
}

// Guard wraps next with a quota check. On allow, dispatches to next. On
// deny, returns 429. Headers are always written via WriteHeaders so
// consumers can observe remaining budget on every response.
func Guard(cfg GuardConfig) func(http.Handler) http.Handler {
	if cfg.Engine == nil {
		panic("middleware.Guard: Engine required")
	}
	if cfg.Identity == nil {
		panic("middleware.Guard: Identity required")
	}
	if cfg.Router == nil {
		panic("middleware.Guard: Router required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isExempt(r.URL.Path, cfg.Exempt) {
				next.ServeHTTP(w, r)
				return
			}
			resourceKey, cost := cfg.Router(r)
			if resourceKey == "" {
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()
			if tok := bearerToken(r); tok != "" {
				ctx = providers.WithToken(ctx, tok)
			}

			userID, orgID, err := cfg.Identity(r)
			if err != nil {
				if cfg.FailOpen {
					slog.Warn("quota guard: identity failed, fail-open", "err", err)
					next.ServeHTTP(w, r)
					return
				}
				http.Error(w, `{"detail":"identity required"}`, http.StatusUnauthorized)
				return
			}

			res, err := cfg.Engine.Check(ctx, engine.CheckInput{
				UserID: userID, OrgID: orgID, ResourceKey: resourceKey, Cost: cost,
			})
			if err != nil {
				if cfg.FailOpen {
					slog.Warn("quota guard: engine error, fail-open",
						"resource", resourceKey, "err", err)
					next.ServeHTTP(w, r)
					return
				}
				slog.Warn("quota guard: engine error, fail-closed",
					"resource", resourceKey, "err", err)
				http.Error(w, `{"detail":"quota engine error"}`,
					http.StatusServiceUnavailable)
				return
			}

			if cfg.OnDecision != nil {
				cfg.OnDecision(r, res)
			}
			if res.Decision == engine.Warn || res.Decision == engine.Critical {
				if cfg.OnWarn != nil {
					cfg.OnWarn(r, res)
				}
				WriteWarn(w, res)
			}
			if !res.Allowed() {
				WriteDenial(w, res)
				return
			}
			WriteHeaders(w, res)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isExempt(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}

// ErrSkip — returned from Identity to indicate "no quota check, but
// not an auth error". Reserved for future use.
var ErrSkip = errors.New("skip quota check")

// IdentityFromContext is the trivial Identity if user/org are already in
// the request context (set by upstream auth middleware). The key types
// are the consumer's; this only documents the pattern.
func IdentityFromContext(userKey, orgKey any) Identity {
	return func(r *http.Request) (string, string, error) {
		u, _ := r.Context().Value(userKey).(string)
		o, _ := r.Context().Value(orgKey).(string)
		if u == "" {
			return "", "", errors.New("no user_id in context")
		}
		return u, o, nil
	}
}

var _ = context.Background // keep context referenced for future helpers
