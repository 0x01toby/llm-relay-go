package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/schema"
)

// AliasRepo owns the model_aliases table.
type AliasRepo struct {
	pool *pgxpool.Pool
}

// NewAliasRepo builds an AliasRepo against pool.
func NewAliasRepo(pool *pgxpool.Pool) *AliasRepo { return &AliasRepo{pool: pool} }

// AliasTarget is a single alias routing target.
type AliasTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// AliasInput is the mutation payload (mirrors ModelAliasMutationInput).
type AliasInput struct {
	Alias       string
	Provider    string
	Model       string
	Targets     []AliasTarget
	Description *string
	Visible     *bool
	Enabled     *bool
}

// List returns all aliases ordered by created_at (mirrors listModelAliases).
func (r *AliasRepo) List(ctx context.Context) ([]schema.ModelAlias, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, alias, provider, model, targets_json, description, visible, enabled, created_at, updated_at
		FROM model_aliases
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schema.ModelAlias
	for rows.Next() {
		var a schema.ModelAlias
		if err := rows.Scan(&a.ID, &a.Alias, &a.Provider, &a.Model, &a.TargetsJSON, &a.Description, &a.Visible, &a.Enabled, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get returns one alias by id, or ErrNotFound.
func (r *AliasRepo) Get(ctx context.Context, id int) (schema.ModelAlias, error) {
	var a schema.ModelAlias
	err := r.pool.QueryRow(ctx, `
		SELECT id, alias, provider, model, targets_json, description, visible, enabled, created_at, updated_at
		FROM model_aliases WHERE id = $1
	`, id).Scan(&a.ID, &a.Alias, &a.Provider, &a.Model, &a.TargetsJSON, &a.Description, &a.Visible, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return schema.ModelAlias{}, ErrNotFound
		}
		return schema.ModelAlias{}, err
	}
	return a, nil
}

// Create inserts a new alias and returns the created row.
func (r *AliasRepo) Create(ctx context.Context, in AliasInput) (schema.ModelAlias, error) {
	now := nowMs()
	targetsJSON := targetsToJSON(in.Targets)
	visible := 1
	if in.Visible != nil && !*in.Visible {
		visible = 0
	}
	enabled := 1
	if in.Enabled != nil && !*in.Enabled {
		enabled = 0
	}
	var a schema.ModelAlias
	err := r.pool.QueryRow(ctx, `
		INSERT INTO model_aliases (alias, provider, model, targets_json, description, visible, enabled, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, alias, provider, model, targets_json, description, visible, enabled, created_at, updated_at
	`, in.Alias, in.Provider, in.Model, targetsJSON, in.Description, visible, enabled, now, now).Scan(
		&a.ID, &a.Alias, &a.Provider, &a.Model, &a.TargetsJSON, &a.Description, &a.Visible, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return schema.ModelAlias{}, err
	}
	return a, nil
}

// Update modifies an alias. Returns ErrNotFound if absent.
func (r *AliasRepo) Update(ctx context.Context, id int, in AliasInput) (schema.ModelAlias, error) {
	now := nowMs()
	targetsJSON := targetsToJSON(in.Targets)
	visible := 1
	if in.Visible != nil && !*in.Visible {
		visible = 0
	}
	enabled := 1
	if in.Enabled != nil && !*in.Enabled {
		enabled = 0
	}
	var a schema.ModelAlias
	err := r.pool.QueryRow(ctx, `
		UPDATE model_aliases SET
		  alias = $1, provider = $2, model = $3, targets_json = $4,
		  description = $5, visible = $6, enabled = $7, updated_at = $8
		WHERE id = $9
		RETURNING id, alias, provider, model, targets_json, description, visible, enabled, created_at, updated_at
	`, in.Alias, in.Provider, in.Model, targetsJSON, in.Description, visible, enabled, now, id).Scan(
		&a.ID, &a.Alias, &a.Provider, &a.Model, &a.TargetsJSON, &a.Description, &a.Visible, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return schema.ModelAlias{}, fmt.Errorf("alias %d %w", id, ErrNotFound)
		}
		return schema.ModelAlias{}, err
	}
	return a, nil
}

// SetEnabled toggles an alias's enabled flag.
func (r *AliasRepo) SetEnabled(ctx context.Context, id int, enabled bool) (schema.ModelAlias, error) {
	var a schema.ModelAlias
	err := r.pool.QueryRow(ctx, `
		UPDATE model_aliases SET enabled = $1, updated_at = $2
		WHERE id = $3
		RETURNING id, alias, provider, model, targets_json, description, visible, enabled, created_at, updated_at
	`, boolToInt(enabled), nowMs(), id).Scan(
		&a.ID, &a.Alias, &a.Provider, &a.Model, &a.TargetsJSON, &a.Description, &a.Visible, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return schema.ModelAlias{}, fmt.Errorf("alias %d %w", id, ErrNotFound)
		}
		return schema.ModelAlias{}, err
	}
	return a, nil
}

// Delete removes an alias. Returns ErrNotFound if absent.
func (r *AliasRepo) Delete(ctx context.Context, id int) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM model_aliases WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("alias %d %w", id, ErrNotFound)
	}
	return nil
}

// targetsToJSON serializes targets; empty slice → "[]" (never empty string,
// so the DB column always holds valid JSON). Mirrors the original's
// JSON.stringify(targets).
func targetsToJSON(targets []AliasTarget) string {
	if len(targets) == 0 {
		return ""
	}
	b, err := json.Marshal(targets)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseTargets decodes an alias's targets_json, falling back to the single
// {provider, model} pair when the column is empty (mirrors parseTargets).
func ParseTargets(a schema.ModelAlias) ([]AliasTarget, error) {
	raw := ""
	if a.TargetsJSON != "" {
		raw = a.TargetsJSON
	}
	if raw != "" {
		var t []AliasTarget
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, fmt.Errorf("invalid targets_json for alias %q: %w", a.Alias, err)
		}
		return t, nil
	}
	return []AliasTarget{{Provider: a.Provider, Model: a.Model}}, nil
}

// ErrEmptyTargets signals a targets array with zero entries.
var ErrEmptyTargets = errors.New("targets must have at least one entry")
