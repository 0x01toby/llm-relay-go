// Package migrate runs database schema setup at boot via GORM AutoMigrate.
//
// Unlike the previous golang-migrate version, there are no versioned SQL files:
// GORM inspects the models in internal/schema and creates tables/indexes
// additively (never drops). This is dialect-agnostic and works across Postgres,
// MySQL, and SQLite.
//
// It returns a Status (success/skipped/failed) the caller reports via /health,
// and ResetDB drops all application tables for the degraded-mode recovery
// endpoint.
package migrate

import (
	"context"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/taozhang/llmrelay/internal/db"
	"github.com/taozhang/llmrelay/internal/schema"
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

// Retry tuning for waitForDbReady.
const (
	readyMaxRetries     = 30
	readyTestMaxRetries = 2
	readyInitialDelay   = 500 * time.Millisecond
	readyMaxDelay       = 5 * time.Second
	readyConnectSecs    = 5
)

// Runner applies migrations. Create one with NewRunner and call Run.
type Runner struct {
	databaseURL string
	isTestDB    bool
}

// NewRunner builds a Runner for databaseURL. If isTestDB is true, Run returns
// skipped immediately (matching the original's test-database bypass — though
// with AutoMigrate this is mostly a no-op anyway).
func NewRunner(databaseURL string, isTestDB bool) *Runner {
	return &Runner{databaseURL: databaseURL, isTestDB: isTestDB}
}

// Run waits for the database and applies AutoMigrate. It never panics: any
// failure becomes a Status{State: failed}.
func (r *Runner) Run(ctx context.Context) Status {
	if r.isTestDB {
		return Status{State: StateSkipped, Reason: "Test database detected"}
	}

	if err := waitForDbReady(ctx, r.databaseURL, false); err != nil {
		return Status{State: StateFailed, Err: fmt.Sprintf("database not ready: %v", err)}
	}

	gdb, err := db.Open(ctx, r.databaseURL, db.DefaultPoolConfig())
	if err != nil {
		return Status{State: StateFailed, Err: fmt.Sprintf("open db: %v", err)}
	}
	sqlDB, _ := gdb.DB()
	defer sqlDB.Close()

	log.Printf("[DB] Running migrations...")
	if err := gdb.AutoMigrate(schema.AllModels()...); err != nil {
		log.Printf("[DB] Migration failed: %v", err)
		return Status{State: StateFailed, Err: err.Error()}
	}
	log.Printf("[DB] Migrations complete.")
	return Status{State: StateSuccess}
}

// waitForDbReady retries connecting until the database accepts connections or
// the retry budget is exhausted. Exponential backoff (500ms → 5s cap). isTest
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

// probe opens a short-lived connection and confirms it's usable.
func probe(ctx context.Context, databaseURL string) error {
	probeCtx, cancel := context.WithTimeout(ctx, readyConnectSecs*time.Second)
	defer cancel()
	gdb, err := gorm.Open(db.OpenGormDialector(probeCtx, databaseURL), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return err
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	return sqlDB.PingContext(probeCtx)
}

// isRetryableDbError mirrors the TS heuristic: connection-refused/reset and
// starting-up messages are retried.
func isRetryableDbError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	retryable := []string{
		"ECONNREFUSED", "ECONNRESET", "connection terminated",
		"the database system is starting up",
		"the database system is shutting down",
		"57P03", // PG cannot connect now
		"server has gone away",
		"connection refused",
	}
	for _, s := range retryable {
		if contains(msg, s) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ResetDB drops all application tables, then re-applies AutoMigrate. Used by
// the degraded-mode /api/db/reset endpoint. Cross-dialect: uses GORM's
// migrator to enumerate tables, then raw `DROP TABLE IF EXISTS` (portable and
// tolerant of a table that doesn't exist yet, which the migrator's DropTable is
// not on some drivers).
func ResetDB(ctx context.Context, databaseURL string) error {
	gdb, err := db.Open(ctx, databaseURL, db.DefaultPoolConfig())
	if err != nil {
		return err
	}
	sqlDB, _ := gdb.DB()
	defer sqlDB.Close()

	m := gdb.Migrator()
	_ = m
	quote := identQuote(gdb)
	for _, model := range schema.AllModels() {
		tableName := schemaTableName(gdb, model)
		// DROP TABLE IF EXISTS is portable across all three dialects and never
		// errors on a missing table — so we skip the HasTable probe (the
		// glebarez sqlite migrator errors on HasTable for a fresh in-memory DB).
		if err := gdb.WithContext(ctx).Exec("DROP TABLE IF EXISTS " + quote + tableName + quote).Error; err != nil {
			log.Printf("[DB] drop table %s: %v", tableName, err)
		}
	}
	if err := gdb.WithContext(ctx).Exec("DROP TABLE IF EXISTS " + quote + "schema_migrations" + quote).Error; err != nil {
		log.Printf("[DB] drop schema_migrations: %v", err)
	}

	if err := gdb.WithContext(ctx).AutoMigrate(schema.AllModels()...); err != nil {
		return err
	}
	return nil
}

// quoteIdent wraps an identifier in the dialect's quoting character. MySQL uses
// backticks; Postgres and SQLite use double quotes. (gorm's mysql driver does
// NOT enable ANSI_QUOTES, so "..." would be parsed as a string literal.) Table
// names here are our own (no user input), so quoting is safe.
func identQuote(gdb *gorm.DB) string {
	switch gdb.Dialector.Name() {
	case "mysql":
		return "`"
	default:
		return "\""
	}
}

// schemaTableName resolves a model's table name via GORM's statement parser
// (honors TableName() methods on the models in internal/schema).
func schemaTableName(gdb *gorm.DB, model interface{}) string {
	stmt := &gorm.Statement{DB: gdb}
	_ = stmt.Parse(model)
	return stmt.Schema.Table
}
