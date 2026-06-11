package authevents

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// WebhookPath is the path where the receiver mounts.
const WebhookPath = "/_webhooks/auth"

// ReceiverConfig configures the webhook receiver returned by MakeRouter.
type ReceiverConfig struct {
	// Secret is required. HMAC over the raw body uses this.
	Secret string
	// Registry is optional; defaults to the package-level registry.
	Registry *Registry
	// LedgerStore is optional; when nil, @idempotent handlers fall back to
	// the InMemoryLedgerStore (logged loudly).
	LedgerStore handlerledger.LedgerStore
}

// MakeRouter returns an http.Handler implementing the webhook receiver.
// Wire it at "<your prefix>/api/quotas/_webhooks/auth".
//
// Wire contract (PRODUCT_SPEC §11.3):
//   401 + {"detail":"invalid signature"} — bad/missing HMAC (static string)
//   400 + {"detail":"invalid json"}      — body isn't JSON
//   200 + {"status":"ignored","event_type":...} — no handlers
//   200 + {"status":"ok","ran":N,"event_type":...} — handlers ran
//
// Handler errors are caught + logged; auth always sees 200 (else retry
// compounds). For an idempotent handler whose retries are exhausted,
// status=failed_permanent and `quotactl replay` is the recovery path.
func MakeRouter(cfg ReceiverConfig) http.Handler {
	if cfg.Registry == nil {
		cfg.Registry = defaultRegistry
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+WebhookPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid body"})
			return
		}

		sig := r.Header.Get("X-Event-Signature")
		if sig == "" {
			// legacy publisher
			sig = r.Header.Get("X-Webhook-Signature")
		}
		if !VerifyHMAC(body, sig, cfg.Secret) {
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
	return mux
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
