// Command spike is the end-to-end validation binary for the Go rewrite.
//
// It runs a minimal HTTP server that accepts POST /v1/responses (OpenAI
// Responses API), converts the request to Chat Completions, forwards it to a
// configured OpenAI-compatible upstream, and converts the (possibly streaming)
// response back to Responses API format.
//
// Usage:
//
//	UPSTREAM_BASE_URL=https://api.openai.com/v1 \
//	UPSTREAM_AUTH_VALUE=sk-... \
//	go run ./cmd/spike
//
// Then:
//
//	curl -N http://localhost:3300/v1/responses \
//	  -H 'content-type: application/json' \
//	  -d '{"model":"gpt-4o-mini","input":"Say hello in one word.","stream":true}'
//
// Set STREAM=0 to test the non-streaming JSON path.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/taozhang/llmrelay/internal/proxy"
)

func main() {
	cfg := proxy.SpikeConfig{
		UpstreamBaseURL:    getenv("UPSTREAM_BASE_URL", "https://api.openai.com/v1"),
		UpstreamAuthHeader: "Authorization",
		UpstreamAuthValue:  getenv("UPSTREAM_AUTH_VALUE", ""),
	}
	addr := ":" + getenv("PORT", "3300")

	handler, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("build proxy: %v", err)
	}

	// Quick self-check: if no upstream auth is configured we still run, but the
	// upstream will reject requests. Warn loudly so it's obvious.
	if cfg.UpstreamAuthValue == "" {
		log.Printf("WARNING: UPSTREAM_AUTH_VALUE is empty; upstream will reject proxied requests")
	}

	log.Printf("spike listening on %s -> %s", addr, cfg.UpstreamBaseURL)
	log.Printf("test: curl -N http://localhost%s/v1/responses -H 'content-type: application/json' -d '{\"model\":\"gpt-4o-mini\",\"input\":\"hi\",\"stream\":true}'", addr)
	srv := &http.Server{Addr: addr, Handler: handler}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
