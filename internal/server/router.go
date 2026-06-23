package server

import (
	"encoding/json"
	"net/http"

	"github.com/taozhang/llmrelay/internal/cors"
	"github.com/taozhang/llmrelay/internal/health"
)

// RootMux builds the top-level HTTP mux. It wires:
//   - GET /health (CORS-wrapped, reports migration status)
//   - GET /        (degraded HTML when migration failed; otherwise a placeholder)
//   - POST /api/db/reset (only available in degraded mode)
//
// The gateway proxy and console handlers are composed on top via GatewayMux.
func RootMux(mig *health.Status, resetFn func() (bool, string, error)) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", mig.Handler())

	mux.HandleFunc("/api/db/reset", func(w http.ResponseWriter, r *http.Request) {
		cors.Apply(w.Header(), r)
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if mig.Healthy() {
			writeJSON(w, http.StatusBadRequest, obj{"error": "数据库状态正常，无需重置"})
			return
		}
		if resetFn == nil {
			writeJSON(w, http.StatusInternalServerError, obj{"error": "database reset is not configured"})
			return
		}
		ok, msg, err := resetFn()
		if !ok {
			writeJSON(w, http.StatusInternalServerError, obj{"error": errMsg(err)})
			return
		}
		mig.Set(health.StatusSnapshot{State: health.StatusSuccess})
		writeJSON(w, http.StatusOK, obj{"message": msg})
	})

	return mux
}

// GatewayMux composes the health/reset routes with the gateway proxy handler
// and an optional root handler (the console SPA, mounted in P7). The proxy
// is the catch-all; model-listing routes are registered before it.
func GatewayMux(mig *health.Status, resetFn func() (bool, string, error), proxy http.Handler, modelsHandler http.HandlerFunc, rootHandler http.HandlerFunc) http.Handler {
	base := RootMux(mig, resetFn)

	mux := http.NewServeMux()
	// Delegate health + db/reset to the base mux.
	mux.Handle("/health", base)
	mux.Handle("/api/db/reset", base)

	if modelsHandler != nil {
		mux.HandleFunc("/v1/models", modelsHandler)
		mux.HandleFunc("/openai/v1/models", modelsHandler)
	}

	if rootHandler != nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// Degraded root takes precedence when migration failed.
			if r.URL.Path == "/" && !mig.Healthy() {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				cors.Apply(w.Header(), r)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(health.DegradedPage(mig.Get().Err)))
				return
			}
			rootHandler(w, r)
		})
	} else if proxy != nil {
		mux.Handle("/", proxy)
	}

	return cors.Middleware(mux)
}

type obj map[string]interface{}

func writeJSON(w http.ResponseWriter, status int, body obj) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errMsg(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}
