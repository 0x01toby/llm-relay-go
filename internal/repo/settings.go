package repo

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/taozhang/llmrelay/internal/schema"
)

// SettingsRepo owns the gateway_settings table (key/value JSON store).
type SettingsRepo struct {
	db *gorm.DB
}

// NewSettingsRepo builds a SettingsRepo against gdb.
func NewSettingsRepo(gdb *gorm.DB) *SettingsRepo { return &SettingsRepo{db: gdb} }

// Get loads the JSON value for a settings key. Returns ("", 0, ErrNotFound)
// when the key is absent (the caller treats absence as "use defaults").
//
// Note: `key` is a reserved word in MySQL, so we build the WHERE clause via
// GORM's clause package so the column is quoted per-dialect (mysql uses
// backticks; postgres/sqlite use double quotes).
func (r *SettingsRepo) Get(ctx context.Context, key string) (string, int64, error) {
	var row schema.GatewaySetting
	err := r.db.WithContext(ctx).
		Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: key}).
		First(&row).Error
	if err != nil {
		if isNoRows(err) {
			return "", 0, ErrNotFound
		}
		return "", 0, err
	}
	return row.ValueJSON, row.UpdatedAt, nil
}

// Upsert stores valueJSON under key, creating or replacing the row.
func (r *SettingsRepo) Upsert(ctx context.Context, key, valueJSON string) (int64, error) {
	now := nowMs()
	row := schema.GatewaySetting{Key: key, ValueJSON: valueJSON, UpdatedAt: now}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value_json", "updated_at"}),
	}).Create(&row).Error
	if err != nil {
		return 0, err
	}
	return now, nil
}

// Delete removes a settings key. No-op if absent.
func (r *SettingsRepo) Delete(ctx context.Context, key string) error {
	return r.db.WithContext(ctx).
		Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: key}).
		Delete(&schema.GatewaySetting{}).Error
}

// ErrSettingsNotFound is returned by Get when the key is absent. Aliased to
// ErrNotFound for consistency; callers may compare against either.
var ErrSettingsNotFound = errors.New("settings key not found")
