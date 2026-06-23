// Package db provides PostgreSQL connection pooling and schema access.
//
// It mirrors src/db/client.ts: a lazily-initialized, shared *pgxpool.Pool keyed
// by the configured DATABASE_URL, plus one-off connections for migration/test
// paths. The pool is injected into repository structs (P2 onward) rather than
// held as module global state, so it stays testable and free of data races.
package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig tunes the connection pool. The defaults mirror postgres.js
// defaults (max 10) used by the original service.
type PoolConfig struct {
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	ConnectTimeout    time.Duration
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

// Pool is the shared connection pool. Create one with NewPool and close it on
// shutdown. All repositories hold a *Pool and call Acquire on it per query.
type Pool = pgxpool.Pool

// NewPool builds and validates a pgxpool against databaseURL. The pool is
// pinged immediately so a bad URL fails fast at boot rather than on first
// request.
func NewPool(ctx context.Context, databaseURL string, cfg PoolConfig) (*Pool, error) {
	if databaseURL == "" {
		return nil, errors.New("database URL is empty")
	}
	pcfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// PoolHolder caches a single pool per URL, matching the lazy-singleton
// behavior of db/client.ts's sharedSqlClient. Production wiring usually only
// needs one URL, so this keeps the lookup O(1) and race-free.
type PoolHolder struct {
	mu       sync.Mutex
	url      string
	pool     *Pool
	poolCfg  PoolConfig
}

// NewPoolHolder creates a holder that will lazily create a pool for url on
// first Get. Pass the production DATABASE_URL here.
func NewPoolHolder(url string, cfg PoolConfig) *PoolHolder {
	return &PoolHolder{url: url, poolCfg: cfg}
}

// Get returns the shared pool, creating it on first call. Subsequent calls
// return the same instance.
func (h *PoolHolder) Get(ctx context.Context) (*Pool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pool != nil {
		return h.pool, nil
	}
	pool, err := NewPool(ctx, h.url, h.poolCfg)
	if err != nil {
		return nil, err
	}
	h.pool = pool
	return pool, nil
}

// Close releases the pool. Safe to call multiple times.
func (h *PoolHolder) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pool != nil {
		h.pool.Close()
		h.pool = nil
	}
}
