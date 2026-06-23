// Package migrate runs database schema migrations at boot. It is a port of
// src/db/migrate.ts:
//
//   - embeds the migration SQL files (no external filesystem dependency)
//   - waits for Postgres to accept connections (exponential backoff)
//   - acquires a pg_advisory_lock so concurrent replicas don't race
//   - applies migrations via golang-migrate
//   - returns a Status (success/skipped/failed) the caller reports via /health
//
// The migration files were converted from the original Drizzle format: the
// "--> statement-breakpoint" markers were stripped and the files renamed to
// golang-migrate's NNNNNN_name.up.sql convention (down migrations are not
// provided — the schema is append-only and reset is handled by the
// /api/db/reset endpoint, which drops and re-applies).
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx driver for migrate
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/migrations"
)

// Status mirrors the TS MigrationStatus union.
type Status struct {
	State  string // "success" | "skipped" | "failed"
	Reason string // populated when skipped
	Err    string // populated when failed
}

// Status values.
const (
	StateSuccess = "success"
	StateSkipped = "skipped"
	StateFailed  = "failed"
)

// Advisory-lock constants match the original (namespace 20817, key 1).
const (
	LockNamespace = 20817
	LockKey       = 1
)

// Retry tuning for waitForDbReady.
const (
	readyMaxRetries    = 30
	readyTestMaxRetries = 2
	readyInitialDelay  = 500 * time.Millisecond
	readyMaxDelay      = 5 * time.Second
	probeConnectSecs   = 5
)

// Runner applies migrations. Create one with NewRunner and call Run.
type Runner struct {
	databaseURL string
	isTestDB    bool
}

// NewRunner builds a Runner for databaseURL. If isTestDB is true, Run returns
// skipped immediately (matching the original's test-database bypass).
func NewRunner(databaseURL string, isTestDB bool) *Runner {
	return &Runner{databaseURL: databaseURL, isTestDB: isTestDB}
}

// Run waits for the database, takes the advisory lock, and applies migrations.
// It never panics: any failure becomes a Status{State: failed}.
func (r *Runner) Run(ctx context.Context) Status {
	if r.isTestDB {
		return Status{State: StateSkipped, Reason: "Test database detected"}
	}

	if err := waitForDbReady(ctx, r.databaseURL, false); err != nil {
		return Status{State: StateFailed, Err: fmt.Sprintf("database not ready: %v", err)}
	}

	// Use a single-connection pool for the advisory lock + migrate so the lock
	// is held on the same connection that runs the migrations.
	pcfg, err := pgxpool.ParseConfig(r.databaseURL)
	if err != nil {
		return Status{State: StateFailed, Err: fmt.Sprintf("parse url: %v", err)}
	}
	pcfg.MaxConns = 1
	pcfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return Status{State: StateFailed, Err: fmt.Sprintf("create pool: %v", err)}
	}
	defer pool.Close()

	// Acquire the advisory lock on a dedicated connection we keep for the whole
	// migration. pg_advisory_lock is session-scoped, so the connection must
	// live until we unlock.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Status{State: StateFailed, Err: fmt.Sprintf("acquire connection: %v", err)}
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, fmt.Sprintf("SELECT pg_advisory_lock(%d, %d)", LockNamespace, LockKey)); err != nil {
		return Status{State: StateFailed, Err: fmt.Sprintf("advisory lock: %v", err)}
	}
	defer func() {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SELECT pg_advisory_unlock(%d, %d)", LockNamespace, LockKey)); err != nil {
			log.Printf("[DB] advisory unlock error: %v", err)
		}
	}()

	log.Printf("[DB] Running migrations...")
	if err := applyMigrations(r.databaseURL); err != nil {
		log.Printf("[DB] Migration failed: %v", err)
		// Migrate's "no change" is not an error, but a dirty state is. Distinguish.
		if errors.Is(err, migrate.ErrNoChange) {
			log.Printf("[DB] Migrations: no change (already up to date)")
			return Status{State: StateSuccess}
		}
		return Status{State: StateFailed, Err: err.Error()}
	}
	log.Printf("[DB] Migrations complete.")
	return Status{State: StateSuccess}
}

// applyMigrations runs golang-migrate against databaseURL using the embedded
// SQL files. The migrate library opens its own connection (the URL must
// include the pgx/v5 scheme it expects).
func applyMigrations(databaseURL string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("embed source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, databaseURLForMigrate(databaseURL))
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// databaseURLForMigrate coerces a standard postgres:// or postgresql:// URL
// into the form golang-migrate's pgx/v5 driver expects. The driver registers
// under the "pgx5" scheme.
func databaseURLForMigrate(databaseURL string) string {
	u := databaseURL
	switch {
	case strings.HasPrefix(u, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(u, "postgres://")
	case strings.HasPrefix(u, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(u, "postgresql://")
	}
	return u
}

// waitForDbReady retries connecting until Postgres accepts connections or the
// retry budget is exhausted. Exponential backoff (500ms → 5s cap). isTest
// shortens the budget to 2 attempts.
func waitForDbReady(ctx context.Context, databaseURL string, isTest bool) error {
	max := readyMaxRetries
	if isTest {
		max = readyTestMaxRetries
	}
	delay := readyInitialDelay
	var lastErr error
	for attempt := 1; attempt <= max; attempt++ {
		if err := probe(ctx, databaseURL); err == nil {
			return nil
		} else {
			lastErr = err
			if !isRetryableDbError(err) || attempt == max {
				return err
			}
			log.Printf("[DB] not ready (attempt %d/%d): %v", attempt, max, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > readyMaxDelay {
			delay = readyMaxDelay
		}
	}
	return lastErr
}

// probe opens a short-lived connection and runs SELECT 1.
func probe(ctx context.Context, databaseURL string) error {
	pcfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return err
	}
	pcfg.MaxConns = 1
	pcfg.MinConns = 0
	pcfg.MaxConnIdleTime = probeConnectSecs * time.Second

	probeCtx, cancel := context.WithTimeout(ctx, probeConnectSecs*time.Second)
	defer cancel()

	// Use a raw connect so we don't hold pool resources.
	conn, err := pgx.Connect(probeCtx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(probeCtx)
	var one int
	if err := conn.QueryRow(probeCtx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	return nil
}

// isRetryableDbError mirrors the TS heuristic: PG starting-up code 57P03 and
// common connection-refused/reset messages are retried.
func isRetryableDbError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	retryable := []string{
		"ECONNREFUSED", "ECONNRESET", "connection terminated",
		"the database system is starting up",
		"the database system is shutting down",
		"57P03", // cannot connect now
	}
	for _, s := range retryable {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// ResetDB drops all user tables and the migrate bookkeeping, then re-applies
// migrations. Mirrors resetDatabase in src/server.ts. Used by the degraded-mode
// /api/db/reset endpoint.
func ResetDB(ctx context.Context, databaseURL string) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// Drop all user tables in the public schema.
	rows, err := conn.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public'
		AND tablename NOT LIKE 'pg_%'
		AND tablename NOT LIKE 'sql_%'
	`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	rows.Close()

	for _, t := range tables {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS "%s" CASCADE`, t)); err != nil {
			return err
		}
		log.Printf("[DB] Dropped table: %s", t)
	}

	// Drop the migrate bookkeeping schema (golang-migrate uses a "schema_migrations"
	// table, not a schema; drop it explicitly).
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS "schema_migrations"`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS "drizzle" CASCADE`); err != nil {
		return err
	}

	// Re-apply migrations on a fresh state.
	if err := applyMigrations(databaseURL); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// silence unused fs import (kept for documentation of the embed source type).
var _ fs.FS
