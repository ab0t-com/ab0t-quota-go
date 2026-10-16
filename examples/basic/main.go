// Minimal ab0t-quota integration: load config, guard one route, count usage
// after the work, expose an exempt /healthz. Run from the repo root with:
//
//	QUOTA_CONFIG_PATH=examples/basic/quota-config.json go run ./examples/basic
//	curl -s -H 'X-Demo-User: alice' localhost:8080/api/echo    # 100/hour, then 429
//
// The config declares "memory://" counters (one process, dev only). Point
// storage.redis_url (or QUOTA_REDIS_URL) at your Redis before you run replicas.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/quota"
)

type ctxKey string

const (
	userKey ctxKey = "user_id"
	orgKey  ctxKey = "org_id"
)

// demoAuth stands in for YOUR auth middleware: it must validate the caller's
// token first and then put the verified user and org in the request context.
// Never trust identity headers from the client in production — this demo does,
// only so it runs with curl.
func demoAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := r.Header.Get("X-Demo-User")
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, orgKey, "org-"+u) // one org per demo user
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func identity(r *http.Request) (string, string, error) {
	u, _ := r.Context().Value(userKey).(string)
	o, _ := r.Context().Value(orgKey).(string)
	return u, o, nil
}

// newMux wires the quota guard around the API. Split out of main so the
// example is tested (examples/basic/main_test.go).
func newMux(q *quota.Quota) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	guard := q.Middleware(quota.MiddlewareDeps{
		Identity: identity,
		Router: func(r *http.Request) (string, float64) {
			return "api.requests", 1 // in real apps, derive the resource from the route
		},
		Exempt: []string{"/healthz"},
	})

	mux.Handle("/api/echo", demoAuth(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
		// The guard only CHECKS. Count the usage after the work succeeded —
		// without this the limit is never reached.
		u, o, _ := identity(r)
		if _, err := q.Spend(r.Context(), engine.CheckInput{UserID: u, OrgID: o, ResourceKey: "api.requests", Cost: 1}); err != nil {
			log.Printf("quota spend: %v", err) // the work happened: log, reconcile later
		}
	}))))
	return mux
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	q, err := quota.Setup(ctx, quota.Options{}) // QUOTA_CONFIG_PATH, else ./quota-config.json
	if err != nil {
		log.Fatal(err)
	}
	defer q.Close(context.Background())

	srv := &http.Server{Addr: ":8080", Handler: newMux(q)}
	go func() {
		log.Println("listening on :8080")
		_ = srv.ListenAndServe()
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
