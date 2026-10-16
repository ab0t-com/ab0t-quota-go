package authevents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// WebhookPath is the receiver's own path segment. The full public path is
// whatever you mount it under, conventionally "/api/quotas"+WebhookPath — the
// endpoint SubscribeOnStartup registers with auth.
const WebhookPath = "/_webhooks/auth"

// DefaultMaxBodyBytes caps the request body the receiver buffers before it
// verifies the signature. Auth events are a few KB; 1 MiB is generous.
const DefaultMaxBodyBytes int64 = 1 << 20

// ReceiverConfig configures the webhook receiver returned by MakeRouter.
type ReceiverConfig struct {
	// Secret is required. HMAC over the raw body uses this.
	Secret string
	// Registry is optional; defaults to the package-level registry.
	Registry *Registry
	// LedgerStore is optional; when nil, @idempotent handlers fall back to
	// the InMemoryLedgerStore (logged loudly).
	LedgerStore handlerledger.LedgerStore
	// PreviousSecrets (v0.1.10) are also accepted while a secret rotation is in
	// flight: auth cannot change a subscription's secret, so a rotation creates a
	// new subscription signed with the new Secret while deliveries signed with the
	// old one may still arrive. Empty entries are ignored (an empty secret never
	// verifies anything). Remove the old secret once the rotation is done.
	PreviousSecrets []string
	// MaxBodyBytes (v0.1.10) caps the body buffered before signature
	// verification. 0 → DefaultMaxBodyBytes. Larger bodies get 413.
	MaxBodyBytes int64
}

// MakeRouter returns an http.Handler implementing the webhook receiver.
//
// Mount it at its full public path — the path SubscribeOnStartup registers
// with auth, "<PublicURL>/api/quotas/_webhooks/auth" by default:
//
//	mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
//
// The receiver answers any request path that ENDS in WebhookPath (on a
// segment boundary), so the full-path mount, a subtree mount
// (mux.Handle("/api/quotas/", ...)) and http.StripPrefix("/api/quotas", ...)
// all work. Before v0.1.10 only the StripPrefix form worked; the full-path
// mount answered 404 to every event (ticket 20261016). Any other path is 404;
// a non-POST on the webhook path is 405 with Allow: POST.
//
// Wire contract (PRODUCT_SPEC §11.3):
//
//	401 + {"detail":"invalid signature"} — bad/missing HMAC (static string)
//	400 + {"detail":"invalid json"}      — body isn't JSON
//	413 + {"detail":"body too large"}    — body over MaxBodyBytes (v0.1.10)
//	200 + {"status":"ignored","event_type":...} — no handlers
//	200 + {"status":"ok","ran":N,"event_type":...} — handlers ran
//
// Handler errors are caught + logged; auth always sees 200 (else retry
// compounds). For an idempotent handler whose retries are exhausted the
// ledger row is status=failed_permanent, which is TERMINAL for that event_id:
// re-posting the same event (quotactl replay) is a no-op. Recover by fixing the
// cause and re-sending the grant under a NEW event id (quotactl backfill) —
// business dedup keeps already-granted users no-ops.
func MakeRouter(cfg ReceiverConfig) http.Handler {
	if cfg.Registry == nil {
		cfg.Registry = defaultRegistry
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	secrets := make([]string, 0, 1+len(cfg.PreviousSecrets))
	secrets = append(secrets, cfg.Secret)
	for _, s := range cfg.PreviousSecrets {
		if s != "" {
			secrets = append(secrets, s)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsWebhookPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		_ = r.Body.Close()
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"detail": "body too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid body"})
			return
		}

		sig := r.Header.Get("X-Event-Signature")
		if sig == "" {
			// legacy publisher
			sig = r.Header.Get("X-Webhook-Signature")
		}
		verified := false
		for _, s := range secrets {
			if VerifyHMAC(body, sig, s) {
				verified = true
				break
			}
		}
		if !verified {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "invalid signature"})
			return
		}

		var event Event
		if err := json.Unmarshal(body, &event); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid json"})
			return
		}
		event.SetRaw(body)

		evtType := event.GetEventType()
		handlers := cfg.Registry.Handlers(evtType)
		if len(handlers) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "ignored",
				"event_type": evtType,
			})
			return
		}

		ran := 0
		for _, h := range handlers {
			err := dispatchOne(r.Context(), h, event, cfg.LedgerStore)
			if err != nil {
				slog.Warn("auth-event handler failed",
					"event_type", evtType,
					"event_id", event.GetEventID(),
					"err", err)
				continue
			}
			ran++
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"ran":        ran,
			"event_type": evtType,
		})
	})
}

// IsWebhookPath reports whether a request path addresses the receiver: the bare
// WebhookPath, or any path ending in it on a segment boundary
// ("/api/quotas/_webhooks/auth"). "/api/quotas/x_webhooks/auth" does not match.
func IsWebhookPath(p string) bool {
	return p == WebhookPath || strings.HasSuffix(p, WebhookPath)
}

// dispatchOne routes one handler. Type-switches on *IdempotentHandler so
// wrapped handlers get the ledger machinery; plain HandlerFunc handlers
// run as v0.5.1.
func dispatchOne(ctx context.Context, h Handler, event Event, store handlerledger.LedgerStore) error {
	if idem, ok := h.(*IdempotentHandler); ok {
		return handlerledger.Dispatch(ctx, idem.Inner(), event, store)
	}
	return h.Handle(ctx, event)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
