// Minimal ab0t-quota integration: load config, wrap one handler with the
// guard, expose a /healthz exempt path. Run with:
//
//	AB0T_QUOTA_CONFIG_PATH=./quota-config.json go run ./examples/basic
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ab0t-com/ab0t-quota-go/quota"
)

type ctxKey string

const userKey ctxKey = "user_id"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	q, err := quota.Setup(ctx, quota.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer q.Close(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	guard := q.Middleware(quota.MiddlewareDeps{
		Identity: func(r *http.Request) (string, string, error) {
			u, _ := r.Context().Value(userKey).(string)
			return u, "", nil
		},
		Router: func(r *http.Request) (string, float64) {
			// In real apps, derive resource from URL/route.
			return "api.requests", 1
		},
		Exempt: []string{"/healthz"},
	})

	mux.Handle("/api/echo", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})))

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		log.Println("listening on :8080")
		_ = srv.ListenAndServe()
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
