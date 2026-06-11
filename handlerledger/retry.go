package handlerledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// BackoffKind selects the retry backoff strategy.
type BackoffKind string

const (
	BackoffExponential BackoffKind = "exponential"
	BackoffLinear      BackoffKind = "linear"
	BackoffConstant    BackoffKind = "constant"
)

// RetryConfig governs the retry loop.
type RetryConfig struct {
	Attempts int
	Backoff  BackoffKind
	Initial  time.Duration
	Max      time.Duration
}

// DefaultRetry returns the lib's standard retry: 3 attempts, exponential,
// starting at 1s, capped at 30s.
func DefaultRetry() *RetryConfig {
	return &RetryConfig{
		Attempts: 3,
		Backoff:  BackoffExponential,
		Initial:  1 * time.Second,
		Max:      30 * time.Second,
	}
}

// NoRetry disables retry — handler runs at most once.
var NoRetry = &RetryConfig{Attempts: 1}

// runWithRetry executes inner with retry per the policy and records the
// final outcome to the ledger. Returns any non-sentinel error from the
// last attempt.
func runWithRetry(
	ctx context.Context,
	h *IdempotentHandler,
	event Event,
	hctx *Context,
	store LedgerStore,
) error {
	cfg := h.Config.Retry
	if cfg == nil {
		cfg = NoRetry
	}
	maxAttempts := cfg.Attempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := h.Inner(ctx, event, hctx)

		// Sentinel returns short-circuit retry.
		var skip *SkipError
		var success *SuccessError
		if errors.As(err, &skip) {
			return store.RecordOutcome(ctx, OutcomeInput{
				HandlerName: h.Config.Handler,
				EventID:     hctx.EventID,
				Status:      StatusSkipped,
				Reason:      skip.Reason,
				Attempts:    attempt,
			})
		}
		if errors.As(err, &success) {
			return store.RecordOutcome(ctx, OutcomeInput{
				HandlerName:  h.Config.Handler,
				EventID:      hctx.EventID,
				Status:       StatusSuccess,
				SideEffectID: success.SideEffectID,
				Attempts:     attempt,
			})
		}
		if err == nil {
			return store.RecordOutcome(ctx, OutcomeInput{
				HandlerName: h.Config.Handler,
				EventID:     hctx.EventID,
				Status:      StatusSuccess,
				Attempts:    attempt,
			})
		}

		// Real failure — log and retry if attempts remain.
		lastErr = err
		slog.Warn("handler attempt failed",
			"handler", h.Config.Handler,
			"event_id", hctx.EventID,
			"attempt", attempt,
			"of", maxAttempts,
			"err", err)
		if attempt == maxAttempts {
			break
		}
		select {
		case <-time.After(backoffDelay(cfg, attempt)):
		case <-ctx.Done():
			lastErr = fmt.Errorf("retry cancelled: %w", ctx.Err())
			break
		}
	}

	// All attempts failed.
	_ = store.RecordOutcome(ctx, OutcomeInput{
		HandlerName: h.Config.Handler,
		EventID:     hctx.EventID,
		Status:      StatusFailedPermanent,
		Error:       lastErr.Error(),
		Attempts:    maxAttempts,
	})
	return lastErr
}

func backoffDelay(cfg *RetryConfig, attempt int) time.Duration {
	if cfg.Initial == 0 {
		cfg.Initial = 1 * time.Second
	}
	if cfg.Max == 0 {
		cfg.Max = 30 * time.Second
	}
	var d time.Duration
	switch cfg.Backoff {
	case BackoffLinear:
		d = cfg.Initial * time.Duration(attempt)
	case BackoffConstant:
		d = cfg.Initial
	default: // exponential
		d = time.Duration(float64(cfg.Initial) * math.Pow(2, float64(attempt-1)))
	}
	if d > cfg.Max {
		d = cfg.Max
	}
	return d
}

// Dispatch runs an IdempotentHandler with delivery dedup + retry + ledger
// persistence. The caller (typically the webhook receiver) builds the
// HandlerContext.
func Dispatch(ctx context.Context, h *IdempotentHandler, event Event, store LedgerStore) error {
	if store == nil {
		store = NewInMemoryLedgerStore()
	}
	cfg := h.Config
	attempt, err := store.RecordAttempt(ctx, AttemptInput{
		HandlerName:  cfg.Handler,
		EventID:      event.GetEventID(),
		EventType:    event.GetEventType(),
		EventPayload: event.Raw(),
		UserID:       event.GetUserID(),
		OrgID:        event.GetOrgID(),
		LeaseSeconds: cfg.LeaseSeconds,
	})
	if err != nil {
		return err
	}
	if !attempt.Proceed {
		slog.Info("handler already processed; cached outcome",
			"handler", cfg.Handler,
			"event_id", event.GetEventID(),
			"status", attempt.CachedRow.Status)
		return nil
	}

	hctx := &Context{
		HandlerName:  cfg.Handler,
		EventID:      event.GetEventID(),
		EventType:    event.GetEventType(),
		EventPayload: event.Raw(),
		Store:        store,
	}
	if cfg.Key != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("key fn panicked; running without business dedup",
						"handler", cfg.Handler, "panic", r)
				}
			}()
			hctx.DedupKey = cfg.Key(event)
		}()
	}
	return runWithRetry(ctx, h, event, hctx, store)
}
