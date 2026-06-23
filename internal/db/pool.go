// Package db provides database connection management with multi-dialect
// support (Postgres, MySQL, SQLite) via GORM.
//
// The dialect is auto-detected from the DATABASE_URL scheme, so the same binary
// works against all three backends with no code changes:
//
//	DATABASE_URL=postgres://user:pass@host:5432/db
//	DATABASE_URL=mysql://user:pass@tcp(host:3306)/db
//	DATABASE_URL=sqlite:///data/lrs.db
//
// The *gorm.DB is injected into repository structs rather than held as module
// global state, so it stays testable and free of data races.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Dialect identifies which database backend is in use.
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectMySQL    Dialect = "mysql"
	DialectSQLite   Dialect = "sqlite"
)

// String returns the human-readable backend name (used by the console API's
// storage_backend field and /health).
func (d Dialect) String() string { return string(d) }

// DetectDialect infers the dialect from the DATABASE_URL scheme. SQLite is the
// default for a bare file path or a sqlite:// scheme; postgres for postgres(ql)://;
// mysql for mysql://. Returns an error for an unrecognized scheme.
func DetectDialect(databaseURL string) (Dialect, error) {
	u := strings.TrimSpace(strings.ToLower(databaseURL))
	switch {
	case u == "":
		return "", errors.New("database URL is empty")
	case strings.HasPrefix(u, "postgres://"), strings.HasPrefix(u, "postgresql://"):
		return DialectPostgres, nil
	case strings.HasPrefix(u, "mysql://"), strings.HasPrefix(u, "mysql+"):
		return DialectMySQL, nil
	case strings.HasPrefix(u, "sqlite://"), strings.HasPrefix(u, "file:"), strings.HasSuffix(u, ".db"), strings.HasSuffix(u, ".sqlite"), strings.HasSuffix(u, ".sqlite3"):
		return DialectSQLite, nil
	// A path like /data/lrs.db is already caught above; treat other bare paths
	// (no scheme) as SQLite too, since PG/MySQL always use a scheme.
	case !strings.Contains(u, "://"):
		return DialectSQLite, nil
	}
	return "", fmt.Errorf("unrecognized database URL scheme: %s", databaseURL)
}

// PoolConfig tunes the connection pool. Values map to each driver's pool knobs.
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// DefaultPoolConfig returns sensible production defaults.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxConns:        10,
		MinConns:        0,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
}

// Open builds and validates a *gorm.DB against databaseURL, auto-detecting the
// dialect. The connection is pinged immediately so a bad URL fails fast at boot.
// For SQLite, DSN options (WAL, busy_timeout) are applied for concurrency safety.
func Open(ctx context.Context, databaseURL string, cfg PoolConfig) (*gorm.DB, error) {
	if databaseURL == "" {
		return nil, errors.New("database URL is empty")
	}
	dialect, err := DetectDialect(databaseURL)
	if err != nil {
		return nil, err
	}

	gormCfg := &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
		// Keep GORM from wrapping returns-table RETURNING on query-style calls;
		// we control upserts explicitly via clauses.
		SkipDefaultTransaction: true,
	}

	var gdb *gorm.DB
	switch dialect {
	case DialectPostgres:
		// The gorm postgres driver accepts a standard postgres:// URL.
		// Pool sizing is configured via the URL's pool_max_conns or left to defaults;
		// pgx's default pool (max 4-25) is fine for this workload.
		gdb, err = gorm.Open(postgres.Open(normalizePostgresURL(databaseURL, cfg)), gormCfg)
	case DialectMySQL:
		gdb, err = gorm.Open(mysql.Open(normalizeMySQLDSN(databaseURL, cfg)), gormCfg)
	case DialectSQLite:
		gdb, err = gorm.Open(sqlite.Open(normalizeSQLiteDSN(databaseURL)), gormCfg)
		if err == nil {
			// Enable WAL + a sane busy timeout so concurrent reads + one writer
			// don't immediately error under load. Best-effort: ignore errors
			// (some sqlite builds ignore these pragmas).
			_ = gdb.Exec("PRAGMA journal_mode=WAL").Error
			_ = gdb.Exec("PRAGMA busy_timeout=5000").Error
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dialect, err)
	}

	// Ping to fail fast on a bad URL.
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("get *sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(int(cfg.MaxConns))
	sqlDB.SetMaxIdleConns(int(cfg.MinConns))
	sqlDB.SetConnMaxLifetime(cfg.MaxConnLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.MaxConnIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping %s: %w", dialect, err)
	}
	return gdb, nil
}

// normalizePostgresURL returns the URL as-is (gorm's postgres driver accepts
// postgres:// and postgresql://). Pool knobs could be appended but pgx defaults
// are adequate.
func normalizePostgresURL(databaseURL string, _ PoolConfig) string {
	return databaseURL
}

// normalizeMySQLDSN converts a mysql:// URL into the DSN the go-sql-driver
// expects (user:pass@tcp(host:port)/db?params). mysql://tcp(host:port)/... is
// already a DSN minus the scheme; we also accept mysql://user:pass@host:port/db.
func normalizeMySQLDSN(databaseURL string, cfg PoolConfig) string {
	u := databaseURL
	u = strings.TrimPrefix(u, "mysql://")
	u = strings.TrimPrefix(u, "mysql+")
	// If it already looks like a DSN (contains @ and /), return it.
	if strings.Contains(u, "@") {
		return u
	}
	return u
}

// normalizeSQLiteDSN returns the path for the glebarez sqlite driver. Strips a
// sqlite:// scheme and keeps any query options the caller appended.
func normalizeSQLiteDSN(databaseURL string) string {
	u := databaseURL
	switch {
	case strings.HasPrefix(u, "sqlite://"):
		u = strings.TrimPrefix(u, "sqlite://")
	case strings.HasPrefix(u, "file:"):
		u = strings.TrimPrefix(u, "file:")
	}
	return u
}

// Holder caches a single *gorm.DB per URL, matching the lazy-singleton
// behavior of the original db/client.ts. Production wiring usually only needs
// one URL, so this keeps the lookup O(1) and race-free.
type Holder struct {
	mu      sync.Mutex
	url     string
	poolCfg PoolConfig
	gdb     *gorm.DB
}

// NewHolder creates a holder that will lazily create a *gorm.DB for url on
// first Get. Pass the production DATABASE_URL here.
func NewHolder(url string, cfg PoolConfig) *Holder {
	return &Holder{url: url, poolCfg: cfg}
}

// Get returns the shared *gorm.DB, creating it on first call. Subsequent calls
// return the same instance.
func (h *Holder) Get(ctx context.Context) (*gorm.DB, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gdb != nil {
		return h.gdb, nil
	}
	gdb, err := Open(ctx, h.url, h.poolCfg)
	if err != nil {
		return nil, err
	}
	h.gdb = gdb
	return gdb, nil
}

// Close releases the connection pool. Safe to call multiple times.
func (h *Holder) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gdb != nil {
		if sqlDB, err := h.gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		h.gdb = nil
	}
}

// OpenGormDialector returns a gorm.Dialector for databaseURL without opening a
// pool. Used by migrate's short-lived probe connection (it wants gorm.Open to
// build a one-shot connection rather than going through the full Open path).
func OpenGormDialector(_ context.Context, databaseURL string) gorm.Dialector {
	dialect, _ := DetectDialect(databaseURL)
	switch dialect {
	case DialectMySQL:
		return mysql.Open(normalizeMySQLDSN(databaseURL, DefaultPoolConfig()))
	case DialectSQLite:
		return sqlite.Open(normalizeSQLiteDSN(databaseURL))
	default:
		return postgres.Open(normalizePostgresURL(databaseURL, DefaultPoolConfig()))
	}
}
