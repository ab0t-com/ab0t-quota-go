package authevents

import (
	"context"
	"reflect"
	"sort"
	"sync"
)

// Handler is the registry's element type. Plain handlers go through a
// HandlerFunc adapter; @idempotent wrappers implement Handler directly
// (via *handlerledger.IdempotentHandler), so the dispatcher type-switches
// without reflection.
type Handler interface {
	Handle(ctx context.Context, event Event) error
}

// HandlerFunc adapts a function to the Handler interface.
type HandlerFunc func(ctx context.Context, event Event) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, e Event) error { return f(ctx, e) }

// Registry holds handlers grouped by event_type. Safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string][]Handler
}

// NewRegistry returns an empty registry. Used by tests and multi-tenant
// binaries that need isolation; package-level functions delegate to a
// default Registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string][]Handler{}}
}

// OnAuthEvent registers h for eventType. Idempotent — same Handler
// registered twice is a no-op (deduped by pointer for interface values).
func (r *Registry) OnAuthEvent(eventType string, h Handler) Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.handlers[eventType]
	for _, existing := range list {
		if handlersEqual(existing, h) {
			return h
		}
	}
	r.handlers[eventType] = append(list, h)
	return h
}

// Unregister removes h. Returns true if it was present.
func (r *Registry) Unregister(eventType string, h Handler) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.handlers[eventType]
	for i, existing := range list {
		if handlersEqual(existing, h) {
			r.handlers[eventType] = append(list[:i], list[i+1:]...)
			return true
		}
	}
	return false
}

// Handlers returns a copy of the handler slice for eventType. Safe to
// iterate without holding the lock.
func (r *Registry) Handlers(eventType string) []Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.handlers[eventType]
	out := make([]Handler, len(src))
	copy(out, src)
	return out
}

// EventTypes returns the event types with at least one handler, sorted.
// Used by SubscribeOnStartup to know what to subscribe to with auth.
func (r *Registry) EventTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for k, v := range r.handlers {
		if len(v) > 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Clear removes all registrations. Test-only.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers = map[string][]Handler{}
}

// handlersEqual compares two Handler values. For pointer-backed handlers
// (the common case — *IdempotentHandler, struct pointers) we use pointer
// equality via reflect; for HandlerFunc we use reflect.ValueOf().Pointer()
// (function pointer). Plain function values are not comparable with ==.
func handlersEqual(a, b Handler) bool {
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)
	if av.Kind() != bv.Kind() {
		return false
	}
	switch av.Kind() {
	case reflect.Func:
		return av.Pointer() == bv.Pointer()
	case reflect.Ptr:
		return av.Pointer() == bv.Pointer()
	}
	return false
}

// ----- Package-level default registry + delegate funcs -----------------

var defaultRegistry = NewRegistry()

// OnAuthEvent registers a handler on the package-level registry.
func OnAuthEvent(eventType string, h Handler) Handler {
	return defaultRegistry.OnAuthEvent(eventType, h)
}

// RegisterHandler is an alias for OnAuthEvent.
func RegisterHandler(eventType string, h Handler) {
	defaultRegistry.OnAuthEvent(eventType, h)
}

// UnregisterHandler removes a handler. Returns true if it was present.
func UnregisterHandler(eventType string, h Handler) bool {
	return defaultRegistry.Unregister(eventType, h)
}

// RegisteredEventTypes returns event types with at least one handler.
func RegisteredEventTypes() []string {
	return defaultRegistry.EventTypes()
}

// ClearHandlers drops all registrations from the default registry. Test-only.
func ClearHandlers() {
	defaultRegistry.Clear()
}

// DefaultRegistry returns the package-level registry. Useful for advanced
// dispatchers that need to enumerate Handlers per event_type.
func DefaultRegistry() *Registry { return defaultRegistry }
