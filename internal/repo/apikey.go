package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/schema"
)

// APIKeyRepo owns the console_api_keys table.
type APIKeyRepo struct {
	pool *pgxpool.Pool
}

// NewAPIKeyRepo builds an APIKeyRepo against pool.
func NewAPIKeyRepo(pool *pgxpool.Pool) *APIKeyRepo { return &APIKeyRepo{pool: pool} }

// CreatedAPIKey is the result of Create: the raw key (shown once) plus the row.
type CreatedAPIKey struct {
	RawKey string
	Row    schema.ConsoleAPIKey
}

// List returns all non-revoked keys, newest first (mirrors listManagedApiKeys).
func (r *APIKeyRepo) List(ctx context.Context) ([]schema.ConsoleAPIKey, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, key_hash, key_value, prefix, created_at, last_used_at,
		       revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd
		FROM console_api_keys
		WHERE revoked = 0
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schema.ConsoleAPIKey
	for rows.Next() {
		var k schema.ConsoleAPIKey
		if err := scanAPIKey(rows, &k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Create inserts a new key and returns the raw key + row (mirrors
// createManagedApiKey). costQuotaMicrousd is nil for unlimited.
func (r *APIKeyRepo) Create(ctx context.Context, name string, costQuotaMicrousd *int64) (CreatedAPIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CreatedAPIKey{}, errors.New("Key 名称不能为空")
	}
	rawKey, err := createRawKey()
	if err != nil {
		return CreatedAPIKey{}, err
	}
	id, err := createKeyID()
	if err != nil {
		return CreatedAPIKey{}, err
	}
	now := nowMs()
	var k schema.ConsoleAPIKey
	err = r.pool.QueryRow(ctx, `
		INSERT INTO console_api_keys
		  (id, name, key_hash, key_value, prefix, created_at, last_used_at, revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, name, key_hash, key_value, prefix, created_at, last_used_at, revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd
	`, id, name, hashKey(rawKey), rawKey, keyPrefix(rawKey), now, nil, 0, "[]", costQuotaMicrousd, 0).Scan(
		&k.ID, &k.Name, &k.KeyHash, &k.KeyValue, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.Revoked, &k.AllowedModelsJSON, &k.CostQuotaMicrousd, &k.CostUsedMicrousd)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	return CreatedAPIKey{RawKey: rawKey, Row: k}, nil
}

// Get returns one non-revoked key by id, or ErrNotFound.
func (r *APIKeyRepo) Get(ctx context.Context, id string) (schema.ConsoleAPIKey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	var k schema.ConsoleAPIKey
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, key_hash, key_value, prefix, created_at, last_used_at, revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd
		FROM console_api_keys
		WHERE id = $1 AND revoked = 0
	`, id).Scan(scanAPIKeyDests(&k)...)
	if err != nil {
		if isNoRows(err) {
			return schema.ConsoleAPIKey{}, ErrNotFound
		}
		return schema.ConsoleAPIKey{}, err
	}
	return k, nil
}

// Rename updates a key's name (mirrors renameManagedApiKey).
func (r *APIKeyRepo) Rename(ctx context.Context, id, name string) (schema.ConsoleAPIKey, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	var k schema.ConsoleAPIKey
	err := r.pool.QueryRow(ctx, `
		UPDATE console_api_keys SET name = $1
		WHERE id = $2 AND revoked = 0
		RETURNING id, name, key_hash, key_value, prefix, created_at, last_used_at, revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd
	`, name, id).Scan(scanAPIKeyDests(&k)...)
	if err != nil {
		if isNoRows(err) {
			return schema.ConsoleAPIKey{}, ErrNotFound
		}
		return schema.ConsoleAPIKey{}, err
	}
	return k, nil
}

// Delete hard-deletes a key (mirrors deleteManagedApiKey).
func (r *APIKeyRepo) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM console_api_keys WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Clear removes all keys.
func (r *APIKeyRepo) Clear(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM console_api_keys`)
	return err
}

// Authenticate looks up a key by hash (non-revoked), best-effort updates
// last_used_at, and returns the row. Returns ErrNotFound on miss or DB error
// (errors are swallowed to avoid leaking internals — mirrors the try/catch in
// authenticateManagedApiKey).
func (r *APIKeyRepo) Authenticate(ctx context.Context, rawKey string) (schema.ConsoleAPIKey, bool) {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return schema.ConsoleAPIKey{}, false
	}
	var k schema.ConsoleAPIKey
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, key_hash, key_value, prefix, created_at, last_used_at, revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd
		FROM console_api_keys
		WHERE key_hash = $1 AND revoked = 0
		LIMIT 1
	`, hashKey(rawKey)).Scan(scanAPIKeyDests(&k)...)
	if err != nil {
		return schema.ConsoleAPIKey{}, false
	}
	// Best-effort last_used_at update; ignore errors.
	_, _ = r.pool.Exec(ctx, `UPDATE console_api_keys SET last_used_at = $1 WHERE id = $2 AND revoked <> 1`, nowMs(), k.ID)
	return k, true
}

// SetAllowedModels replaces a key's model allowlist (mirrors
// setApiKeyAllowedModels). Models are trimmed, de-duplicated, non-empty.
func (r *APIKeyRepo) SetAllowedModels(ctx context.Context, id string, models []string) (schema.ConsoleAPIKey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	cleaned := dedupeTrim(models)
	b, err := json.Marshal(cleaned)
	if err != nil {
		return schema.ConsoleAPIKey{}, err
	}
	var k schema.ConsoleAPIKey
	err = r.pool.QueryRow(ctx, `
		UPDATE console_api_keys SET allowed_models_json = $1
		WHERE id = $2 AND revoked = 0
		RETURNING id, name, key_hash, key_value, prefix, created_at, last_used_at, revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd
	`, string(b), id).Scan(scanAPIKeyDests(&k)...)
	if err != nil {
		if isNoRows(err) {
			return schema.ConsoleAPIKey{}, ErrNotFound
		}
		return schema.ConsoleAPIKey{}, err
	}
	return k, nil
}

// SetCostQuota sets a key's cost quota (nil = unlimited).
func (r *APIKeyRepo) SetCostQuota(ctx context.Context, id string, costQuotaMicrousd *int64) (schema.ConsoleAPIKey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	var k schema.ConsoleAPIKey
	err := r.pool.QueryRow(ctx, `
		UPDATE console_api_keys SET cost_quota_microusd = $1
		WHERE id = $2 AND revoked = 0
		RETURNING id, name, key_hash, key_value, prefix, created_at, last_used_at, revoked, allowed_models_json, cost_quota_microusd, cost_used_microusd
	`, costQuotaMicrousd, id).Scan(scanAPIKeyDests(&k)...)
	if err != nil {
		if isNoRows(err) {
			return schema.ConsoleAPIKey{}, ErrNotFound
		}
		return schema.ConsoleAPIKey{}, err
	}
	return k, nil
}

// --- helpers ---

func scanAPIKey(rows interface{ Scan(...interface{}) error }, k *schema.ConsoleAPIKey) error {
	return rows.Scan(scanAPIKeyDests(k)...)
}

// scanAPIKeyDests returns the destination slice in column order, so both Query
// and QueryRow paths share the exact same scan layout.
func scanAPIKeyDests(k *schema.ConsoleAPIKey) []interface{} {
	return []interface{}{
		&k.ID, &k.Name, &k.KeyHash, &k.KeyValue, &k.Prefix, &k.CreatedAt, &k.LastUsedAt,
		&k.Revoked, &k.AllowedModelsJSON, &k.CostQuotaMicrousd, &k.CostUsedMicrousd,
	}
}

// dedupeTrim trims, drops empties, and de-duplicates while preserving order.
func dedupeTrim(models []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(models))
	for _, m := range models {
		t := strings.TrimSpace(m)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// ParseAllowedModels decodes allowed_models_json defensively (mirrors
// parseAllowedModels): non-strings dropped, trimmed, de-duped.
func ParseAllowedModels(jsonStr string) []string {
	var raw []interface{}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil
	}
	var models []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			s = strings.TrimSpace(s)
			if s != "" {
				models = append(models, s)
			}
		}
	}
	return dedupeTrim(models)
}

// IsModelAllowed reports whether model matches any pattern. Patterns support a
// trailing "*" suffix wildcard (mirrors isModelAllowed). Empty patterns = allow
// all.
func IsModelAllowed(model string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == model {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// MicroUSDPerUSD is the conversion factor between USD and micro-USD.
const MicroUSDPerUSD = 1_000_000

// USDToQuotaMicroUSD converts a USD amount to a quota in micro-USD (rounded).
func USDToQuotaMicroUSD(usd float64) int64 {
	return int64(usd*MicroUSDPerUSD + 0.5)
}

// USDCostToChargeMicroUSD converts a cost to a charge in micro-USD, rounding up
// (favors the gateway — mirrors usdCostToChargeMicrousd's Math.ceil).
func USDCostToChargeMicroUSD(cost float64) int64 {
	return int64(cost*MicroUSDPerUSD + 0.9999999)
}

// QuotaSnapshot is the derived quota view (mirrors buildApiKeyQuotaSnapshot).
type QuotaSnapshot struct {
	CostQuota      *float64
	CostUsed       float64
	CostRemaining  *float64
	QuotaExhausted bool
}

// BuildQuotaSnapshot computes the USD view from micro-USD columns.
func BuildQuotaSnapshot(quotaMicrousd *int64, usedMicrousd int64) QuotaSnapshot {
	used := float64(usedMicrousd) / MicroUSDPerUSD
	snap := QuotaSnapshot{CostUsed: used}
	if quotaMicrousd != nil {
		quota := float64(*quotaMicrousd) / MicroUSDPerUSD
		snap.CostQuota = &quota
		remaining := quota - used
		snap.CostRemaining = &remaining
		if used >= quota {
			snap.QuotaExhausted = true
		}
	}
	return snap
}

// fmt-safe guard to keep the import in case future logging is added.
var _ = fmt.Sprintf
