package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/schema"
)

// APIKeyRepo owns the console_api_keys table.
type APIKeyRepo struct {
	db *gorm.DB
}

// NewAPIKeyRepo builds an APIKeyRepo against gdb.
func NewAPIKeyRepo(gdb *gorm.DB) *APIKeyRepo { return &APIKeyRepo{db: gdb} }

// CreatedAPIKey is the result of Create: the raw key (shown once) plus the row.
type CreatedAPIKey struct {
	RawKey string
	Row    schema.ConsoleAPIKey
}

// List returns all non-revoked keys, newest first.
func (r *APIKeyRepo) List(ctx context.Context) ([]schema.ConsoleAPIKey, error) {
	var rows []schema.ConsoleAPIKey
	if err := r.db.WithContext(ctx).Where("revoked = 0").Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Create inserts a new key and returns the raw key + row. costQuotaMicrousd is
// nil for unlimited.
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
	row := schema.ConsoleAPIKey{
		ID:                id,
		Name:              name,
		KeyHash:           hashKey(rawKey),
		KeyValue:          rawKey,
		Prefix:            keyPrefix(rawKey),
		CreatedAt:         now,
		LastUsedAt:        nil,
		Revoked:           0,
		AllowedModelsJSON: "[]",
		CostQuotaMicrousd: costQuotaMicrousd,
		CostUsedMicrousd:  0,
	}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return CreatedAPIKey{}, err
	}
	return CreatedAPIKey{RawKey: rawKey, Row: row}, nil
}

// Get returns one non-revoked key by id, or ErrNotFound.
func (r *APIKeyRepo) Get(ctx context.Context, id string) (schema.ConsoleAPIKey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	var row schema.ConsoleAPIKey
	err := r.db.WithContext(ctx).Where("id = ? AND revoked = 0", id).First(&row).Error
	if err != nil {
		if isNoRows(err) {
			return schema.ConsoleAPIKey{}, ErrNotFound
		}
		return schema.ConsoleAPIKey{}, err
	}
	return row, nil
}

// Rename updates a key's name.
func (r *APIKeyRepo) Rename(ctx context.Context, id, name string) (schema.ConsoleAPIKey, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	res := r.db.WithContext(ctx).Model(&schema.ConsoleAPIKey{}).Where("id = ? AND revoked = 0", id).Update("name", name)
	if res.Error != nil {
		return schema.ConsoleAPIKey{}, res.Error
	}
	if res.RowsAffected == 0 {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	return r.Get(ctx, id)
}

// Delete hard-deletes a key.
func (r *APIKeyRepo) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	res := r.db.WithContext(ctx).Where("id = ?", id).Delete(&schema.ConsoleAPIKey{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Clear removes all keys.
func (r *APIKeyRepo) Clear(ctx context.Context) error {
	return r.db.WithContext(ctx).Where("1 = 1").Delete(&schema.ConsoleAPIKey{}).Error
}

// Authenticate looks up a key by hash (non-revoked), best-effort updates
// last_used_at, and returns the row. Returns false on miss or DB error.
func (r *APIKeyRepo) Authenticate(ctx context.Context, rawKey string) (schema.ConsoleAPIKey, bool) {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return schema.ConsoleAPIKey{}, false
	}
	var row schema.ConsoleAPIKey
	err := r.db.WithContext(ctx).Where("key_hash = ? AND revoked = 0", hashKey(rawKey)).First(&row).Error
	if err != nil {
		return schema.ConsoleAPIKey{}, false
	}
	// Best-effort last_used_at update.
	_ = r.db.WithContext(ctx).Model(&schema.ConsoleAPIKey{}).
		Where("id = ? AND revoked <> 1", row.ID).
		Update("last_used_at", nowMs()).Error
	return row, true
}

// SetAllowedModels replaces a key's model allowlist. Models are trimmed,
// de-duplicated, non-empty.
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
	res := r.db.WithContext(ctx).Model(&schema.ConsoleAPIKey{}).Where("id = ? AND revoked = 0", id).Update("allowed_models_json", string(b))
	if res.Error != nil {
		return schema.ConsoleAPIKey{}, res.Error
	}
	if res.RowsAffected == 0 {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	return r.Get(ctx, id)
}

// SetCostQuota sets a key's cost quota (nil = unlimited).
func (r *APIKeyRepo) SetCostQuota(ctx context.Context, id string, costQuotaMicrousd *int64) (schema.ConsoleAPIKey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	res := r.db.WithContext(ctx).Model(&schema.ConsoleAPIKey{}).Where("id = ? AND revoked = 0", id).Update("cost_quota_microusd", costQuotaMicrousd)
	if res.Error != nil {
		return schema.ConsoleAPIKey{}, res.Error
	}
	if res.RowsAffected == 0 {
		return schema.ConsoleAPIKey{}, ErrNotFound
	}
	return r.Get(ctx, id)
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

// ParseAllowedModels decodes allowed_models_json defensively: non-strings
// dropped, trimmed, de-duped.
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
// trailing "*" suffix wildcard. Empty patterns = allow all.
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

// USDCostToChargeMicroUSD converts a cost to a charge in micro-USD, rounding up.
func USDCostToChargeMicroUSD(cost float64) int64 {
	return int64(cost*MicroUSDPerUSD + 0.9999999)
}

// QuotaSnapshot is the derived quota view.
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
