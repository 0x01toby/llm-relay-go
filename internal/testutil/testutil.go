// Package testutil provides shared helpers for integration tests, including a
// 3-dialect table-driven DB provider (postgres / mysql / sqlite). It has no
// non-test dependencies.
package testutil

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/db"
	"github.com/taozhang/llmrelay/internal/migrate"
)

// DBCase is one database backend to run an integration test against.
type DBCase struct {
	Name string
	URL  string
}

// dialectURLs returns the database URLs to test, one per supported dialect.
// Each is enabled only if its env var is set, so the suite runs against
// whatever backends the developer has available. SQLite always runs (a temp
// file DB per package, for connection-pool isolation — an in-memory DB is not
// shared across pool connections, which breaks GORM's connection-per-query
// model during AutoMigrate).
//
// Env vars:
//   - TEST_DATABASE_URL      → postgres (and the migrate/repo integration tests)
//   - TEST_MYSQL_URL         → mysql
//   - TEST_SQLITE_URL        → sqlite (defaults to a temp file)
func DialectURLs() []DBCase {
	var cases []DBCase
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		cases = append(cases, DBCase{Name: "postgres", URL: u})
	}
	if u := os.Getenv("TEST_MYSQL_URL"); u != "" {
		cases = append(cases, DBCase{Name: "mysql", URL: u})
	}
	sqliteURL := os.Getenv("TEST_SQLITE_URL")
	if sqliteURL == "" {
		// A temp file DB per process so the pool's connections share state. It
		// is removed on test completion via t.Cleanup in FreshDB.
		f, err := os.CreateTemp("", "lrs-test-*.db")
		if err != nil {
			sqliteURL = "file::memory:?cache=shared"
		} else {
			_ = f.Close()
			_ = os.Remove(f.Name()) // FreshDB's ResetDB recreates the schema
			sqliteURL = "sqlite://" + f.Name()
		}
	}
	cases = append(cases, DBCase{Name: "sqlite", URL: sqliteURL})
	return cases
}

// FreshDB drops all application tables and re-applies AutoMigrate against url,
// returning a *gorm.DB. Each test gets a clean schema.
func FreshDB(t *testing.T, url string) *gorm.DB {
	t.Helper()
	ctx := context.Background()

	if err := migrate.ResetDB(ctx, url); err != nil {
		t.Fatalf("ResetDB: %v", err)
	}
	gdb, err := db.Open(ctx, url, db.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	// For a file-based sqlite DB, remove the file after the test for cleanliness.
	if strings.HasPrefix(url, "sqlite://") {
		path := strings.TrimPrefix(url, "sqlite://")
		t.Cleanup(func() {
			if sqlDB, e := gdb.DB(); e == nil {
				_ = sqlDB.Close()
			}
			_ = os.Remove(path)
		})
		return gdb
	}
	t.Cleanup(func() {
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// SkipIfNoDialect skips t when none of the dialect env vars match the request.
// (Currently unused — tests iterate DialectURLs directly — kept for clarity.)
func SkipIfNoDialect(t *testing.T, want string) {
	t.Helper()
	for _, c := range DialectURLs() {
		if c.Name == want {
			return
		}
	}
	t.Skipf("%s dialect not configured", want)
}

// DialectName mirrors db.Dialect detection for a URL (test-only convenience).
func DialectName(url string) string {
	d, err := db.DetectDialect(url)
	if err != nil {
		return fmt.Sprintf("unknown(%s)", url)
	}
	return d.String()
}
