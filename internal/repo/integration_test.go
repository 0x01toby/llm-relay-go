//go:build integration

// Integration tests for the repository layer against a real Postgres. Skipped
// unless TEST_DATABASE_URL is set. Run with:
//
//	docker run -d --name lrs-test-pg -e POSTGRES_USER=lrs -e POSTGRES_PASSWORD=lrs \
//	  -e POSTGRES_DB=lrs_test -p 5433:5432 postgres:17-alpine
//	TEST_DATABASE_URL=postgresql://lrs:lrs@localhost:5433/lrs_test \
//	go test ./internal/repo/ -tags integration -v
package repo

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/migrate"
)

func testDBURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

// freshDB drops and migrates the test database, returning a pool. Each test
// gets a clean schema.
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDBURL(t)
	ctx := context.Background()

	if err := migrate.ResetDB(ctx, url); err != nil {
		t.Fatalf("ResetDB: %v", err)
	}
	if status := migrate.NewRunner(url, false).Run(ctx); status.State != migrate.StateSuccess {
		t.Fatalf("migrate: %+v", status)
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIntegration_Migrations_CreateAllTables(t *testing.T) {
	pool := freshDB(t)
	var tableCount int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name NOT LIKE 'schema_migrations'
	`).Scan(&tableCount)
	if err != nil {
		t.Fatal(err)
	}
	// 6 application tables: console_requests, console_api_keys,
	// console_providers, model_aliases, model_catalog_cache,
	// model_metadata_overrides, gateway_settings = 7. (The dropped
	// console_request_cache_points is gone.)
	if tableCount < 6 {
		t.Errorf("expected >= 6 tables, got %d", tableCount)
	}
}

func TestIntegration_ProviderCRUD(t *testing.T) {
	pool := freshDB(t)
	r := NewProviderRepo(pool)
	ctx := context.Background()

	sp := func(s string) *string { return &s }
	if err := r.Create(ctx, "ch1", ProviderInput{
		Type: "openai", TargetBaseURL: "https://api.openai.com/v1",
		Models: []map[string]interface{}{{"model": "gpt-4o"}},
	}); err != nil {
		t.Fatal(err)
	}

	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ChannelName != "ch1" {
		t.Fatalf("list: %+v", list)
	}
	if list[0].ProviderUUID == "" {
		t.Error("provider_uuid should be backfilled")
	}
	if list[0].ModelsJSON != `[{"model":"gpt-4o"}]` {
		t.Errorf("models json: %q", list[0].ModelsJSON)
	}

	// Update (rename + change).
	if err := r.Update(ctx, "ch1", "ch2", ProviderInput{
		Type: "anthropic", TargetBaseURL: "https://api.anthropic.com",
		SystemPrompt: sp("be nice"),
	}); err != nil {
		t.Fatal(err)
	}
	list, _ = r.List(ctx)
	if len(list) != 1 || list[0].ChannelName != "ch2" || list[0].Type != "anthropic" {
		t.Fatalf("after update: %+v", list)
	}

	// Toggle off.
	if err := r.SetEnabled(ctx, "ch2", false); err != nil {
		t.Fatal(err)
	}
	list, _ = r.List(ctx)
	if list[0].Enabled != 0 {
		t.Error("should be disabled")
	}

	// Delete.
	if err := r.Delete(ctx, "ch2"); err != nil {
		t.Fatal(err)
	}
	// Delete again → not found.
	if err := r.Delete(ctx, "ch2"); err == nil {
		t.Error("expected ErrNotFound on second delete")
	}
}

func TestIntegration_AliasCRUD(t *testing.T) {
	pool := freshDB(t)
	r := NewAliasRepo(pool)
	ctx := context.Background()

	a, err := r.Create(ctx, AliasInput{
		Alias: "fast", Provider: "ch", Model: "gpt-4o",
		Targets: []AliasTarget{{Provider: "ch", Model: "gpt-4o"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Alias != "fast" {
		t.Errorf("alias: %q", a.Alias)
	}

	list, _ := r.List(ctx)
	if len(list) != 1 {
		t.Fatalf("list len: %d", len(list))
	}
	targets, err := ParseTargets(list[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Model != "gpt-4o" {
		t.Errorf("targets: %+v", targets)
	}

	// Toggle + update.
	if _, err := r.SetEnabled(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(ctx, a.ID)
	if got.Enabled != 0 {
		t.Error("should be disabled")
	}

	if err := r.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_APIKeyCRUD(t *testing.T) {
	pool := freshDB(t)
	r := NewAPIKeyRepo(pool)
	ctx := context.Background()

	created, err := r.Create(ctx, "my-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.RawKey == "" {
		t.Error("raw key should be returned")
	}

	// Authenticate by the raw key.
	auth, ok := r.Authenticate(ctx, created.RawKey)
	if !ok {
		t.Fatal("authenticate failed")
	}
	if auth.ID != created.Row.ID {
		t.Errorf("auth id mismatch: %q vs %q", auth.ID, created.Row.ID)
	}
	// Authenticate returns the row captured before the last_used_at update, so
	// last_used_at is nil on the first auth. A second auth observes the
	// update written by the first (this mirrors the original service's
	// read-then-update ordering).
	auth2, _ := r.Authenticate(ctx, created.RawKey)
	if auth2.LastUsedAt == nil {
		t.Error("last_used_at should be set after a prior auth")
	}

	// Wrong key fails.
	if _, ok := r.Authenticate(ctx, "wrong"); ok {
		t.Error("wrong key should not authenticate")
	}

	// Set allowed models.
	updated, err := r.SetAllowedModels(ctx, created.Row.ID, []string{"gpt-4o", "claude", "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	models := ParseAllowedModels(updated.AllowedModelsJSON)
	if len(models) != 2 || models[0] != "gpt-4o" {
		t.Errorf("allowed models: %+v", models)
	}

	// Quota.
	quota := int64(1_000_000)
	if _, err := r.SetCostQuota(ctx, created.Row.ID, &quota); err != nil {
		t.Fatal(err)
	}

	// Rename + delete.
	if _, err := r.Rename(ctx, created.Row.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, created.Row.ID); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_SettingsKV(t *testing.T) {
	pool := freshDB(t)
	r := NewSettingsRepo(pool)
	ctx := context.Background()

	// Absent key.
	if _, _, err := r.Get(ctx, "gateway.timeouts"); err == nil {
		t.Error("absent key should error")
	}

	// Upsert + read.
	ts, err := r.Upsert(ctx, "gateway.timeouts", `{"default":300000}`)
	if err != nil {
		t.Fatal(err)
	}
	if ts == 0 {
		t.Error("updated_at should be non-zero")
	}
	val, _, err := r.Get(ctx, "gateway.timeouts")
	if err != nil {
		t.Fatal(err)
	}
	if val != `{"default":300000}` {
		t.Errorf("value: %q", val)
	}

	// Replace.
	r.Upsert(ctx, "gateway.timeouts", `{"default":120000}`)
	val, _, _ = r.Get(ctx, "gateway.timeouts")
	if val != `{"default":120000}` {
		t.Errorf("after replace: %q", val)
	}
}

func TestIntegration_ResetDB(t *testing.T) {
	url := testDBURL(t)
	ctx := context.Background()
	pool := freshDB(t)
	r := NewProviderRepo(pool)
	r.Create(ctx, "temp", ProviderInput{Type: "openai", TargetBaseURL: "https://x"})
	pool.Close()

	// Reset wipes everything.
	if err := migrate.ResetDB(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool2, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	var n int
	pool2.QueryRow(ctx, `SELECT count(*) FROM console_providers`).Scan(&n)
	if n != 0 {
		t.Errorf("after reset, providers should be empty, got %d", n)
	}
}
