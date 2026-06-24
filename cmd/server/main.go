// Command server is the production entrypoint for the LLM Relay gateway.
//
// Boot sequence (mirrors src/server.ts):
//  1. Load configuration from the environment.
//  2. Run database migrations (degrades on failure rather than crashing).
//  3. Build the connection pool, configstore, and gateway handler.
//  4. Build the root HTTP mux and start the server with graceful shutdown.
package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/taozhang/llmrelay/internal/catalog"
	"github.com/taozhang/llmrelay/internal/config"
	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/consoleapi"
	"github.com/taozhang/llmrelay/internal/consolestore"
	"github.com/taozhang/llmrelay/internal/db"
	"github.com/taozhang/llmrelay/internal/gateway"
	"github.com/taozhang/llmrelay/internal/health"
	"github.com/taozhang/llmrelay/internal/logtasks"
	"github.com/taozhang/llmrelay/internal/migrate"
	"github.com/taozhang/llmrelay/internal/server"
	"github.com/taozhang/llmrelay/internal/statsstore"
	"github.com/taozhang/llmrelay/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("loaded config: port=%d debug_max_records=%d", cfg.Port, cfg.DebugDBMaxRecords)

	migStatus := health.New()

	// 1. Run migrations. Non-blocking on boot: failure sets degraded status.
	isTest := cfg.DatabaseURL == cfg.TestDatabaseURL && cfg.TestDatabaseURL != ""
	runner := migrate.NewRunner(cfg.DatabaseURL, isTest)
	status := runner.Run(context.Background())
	migStatus.Set(health.StatusSnapshot{State: status.State, Err: status.Err, Reason: status.Reason})
	if status.State == migrate.StateFailed {
		log.Printf("[DB] migration failed (running degraded): %s", status.Err)
	}

	// 2. Build the connection pool + configstore + gateway handler. These are
	// built even in degraded mode (the gateway will fail per-request if the DB
	// is unreachable), but the health/db-reset endpoints let the operator
	// recover without a restart.
	poolHolder := db.NewHolder(cfg.DatabaseURL, db.DefaultPoolConfig())
	gdb, err := poolHolder.Get(context.Background())
	if err != nil {
		log.Printf("[DB] pool unavailable: %v (gateway will degrade)", err)
	}
	dialect, _ := db.DetectDialect(cfg.DatabaseURL)

	var proxy http.Handler
	var modelsHandler http.HandlerFunc
	var consoleHandler http.Handler
	var rootHandler http.HandlerFunc
	lt := logtasks.New()
	// Background scheduler runs periodic housekeeping (currently: console log
	// retention pruning). It is independent of the request path and shuts down
	// cleanly via the server's drain.
	sched := server.NewBackgroundScheduler()
	if gdb != nil {
		store := configstore.NewStoreForDB(gdb)
		reqRepo := consolestore.New(gdb, cfg.DebugDBMaxRecords)

		// Catalog (models.dev pricing/context) is held purely in memory. The
		// pricing data is //go:embed'd from internal/catalog/models-dev.json,
		// which is kept up to date via scripts/sync-prices.sh (committed to git).
		// At boot it is parsed into memory with zero DB/network dependency, and
		// lookups never touch the DB or the network. Update prices by running
		// the sync script and redeploying.
		cat := catalog.New()
		if err := cat.WarmFromEmbed(); err != nil {
			log.Printf("[catalog] warm from vendored catalog: %v", err)
		}

		// Prune old request-log rows every 10 minutes so the table stays within
		// the retention cap. Runs out-of-band from SaveRequest.
		sched.Add("console-cleanup", 10*time.Minute, func(ctx context.Context) error {
			return reqRepo.Cleanup(ctx)
		})

		// Fold new request-log rows into the 5-minute stats rollup table every
		// 3 minutes, so Usage/Monitor stats stay accurate even after old rows
		// are pruned by the retention cap.
		rollup := statsstore.NewRollup(gdb, cat)
		sched.Add("stats-rollup", 3*time.Minute, func(ctx context.Context) error {
			return rollup.RollupTick(ctx)
		})

		gwTimeouts := gateway.TimeoutSettings{
			DefaultFirstByteMs: cfg.Timeouts.DefaultFirstByteMs,
			StreamFirstByteMs:  cfg.Timeouts.StreamFirstByteMs,
			ImageFirstByteMs:   cfg.Timeouts.ImageFirstByteMs,
			ResponseIdleMs:     cfg.Timeouts.ResponseIdleMs,
		}
		gw := gateway.NewHandler(gdb, store, cfg.GatewayAPIKey, gwTimeouts, reqRepo, lt)
		proxy = gw
		modelsHandler = gw.ModelListHandler("")
		consoleHandler = consoleapi.New(gdb, dialect, store, cat, cfg.GatewayAPIKey, cfg.DebugDBMaxRecords).Routes()
		// The root handler serves the SPA for browser navigation, but defers to
		// the proxy for API/model paths (handled inside web.Handler).
		webHandler := web.Handler()
		rootHandler = func(w http.ResponseWriter, r *http.Request) {
			// If it looks like an API/model/proxy path, hand to the gateway.
			p := r.URL.Path
			if strings.HasPrefix(p, "/v1/") || p == "/v1/models" {
				proxy.ServeHTTP(w, r)
				return
			}
			webHandler.ServeHTTP(w, r)
		}
	} else {
		proxy = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"database unavailable"}`))
		})
	}

	// Build the root mux. The console API is mounted at /__console/*, the
	// gateway proxy is the catch-all, and the SPA root handler serves the
	// dashboard for browser navigation.
	mux := server.GatewayMux(migStatus, func() (bool, string, error) {
		if err := migrate.ResetDB(context.Background(), cfg.DatabaseURL); err != nil {
			return false, "", err
		}
		return true, "数据库已重置并重新迁移", nil
	}, proxy, modelsHandler, rootHandler)

	// Mount the console API at /__console/* (it owns that prefix entirely).
	fullHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__console") {
			if consoleHandler != nil {
				consoleHandler.ServeHTTP(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})

	srv := server.New(server.ServerConfig{
		Addr:    cfg.Addr(),
		Handler: fullHandler,
	})

	// Register the background-log drain: wait for in-flight request/response
	// log writes to flush before exiting (fixes the original service's defect),
	// then stop the periodic background scheduler.
	sched.Start()
	srv.WithDrain(func(ctx context.Context) error {
		if err := lt.Wait(ctx); err != nil {
			log.Printf("log drain incomplete: %v", err)
		} else {
			log.Printf("log drain complete")
		}
		_ = sched.Stop(ctx)
		return nil
	})

	if err := srv.Run(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
