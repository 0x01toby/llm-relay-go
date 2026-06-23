package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/schema"
)

// SettingsRepo owns the gateway_settings table (key/value JSON store).
type SettingsRepo struct {
	pool *pgxpool.Pool
}

// NewSettingsRepo builds a SettingsRepo against pool.
func NewSettingsRepo(pool *pgxpool.Pool) *SettingsRepo { return &SettingsRepo{pool: pool} }

// Get loads the JSON value for a settings key. Returns ("", nil, ErrNotFound)
// when the key is absent (the caller treats absence as "use defaults").
func (r *SettingsRepo) Get(ctx context.Context, key string) (string, int64, error) {
	var s schema.GatewaySetting
	err := r.pool.QueryRow(ctx, `
		SELECT key, value_json, updated_at FROM gateway_settings WHERE key = $1
	`, key).Scan(&s.Key, &s.ValueJSON, &s.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return "", 0, ErrNotFound
		}
		return "", 0, err
	}
	return s.ValueJSON, s.UpdatedAt, nil
}

// Upsert stores valueJSON under key, creating or replacing the row (mirrors the
// INSERT ... ON CONFLICT DO UPDATE in the original).
func (r *SettingsRepo) Upsert(ctx context.Context, key, valueJSON string) (int64, error) {
	now := nowMs()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO gateway_settings (key, value_json, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET value_json = EXCLUDED.value_json, updated_at = EXCLUDED.updated_at
	`, key, valueJSON, now)
	if err != nil {
		return 0, err
	}
	return now, nil
}

// Delete removes a settings key. No-op if absent.
func (r *SettingsRepo) Delete(ctx context.Context, key string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM gateway_settings WHERE key = $1`, key)
	return err
}

// ErrSettingsNotFound is returned by Get when the key is absent. Aliased to
// ErrNotFound for consistency; callers may compare against either.
var ErrSettingsNotFound = errors.New("settings key not found")
