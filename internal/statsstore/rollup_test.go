package statsstore

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/pricing"
	"github.com/taozhang/llmrelay/internal/schema"
)

// fakeCatalog is a stub priceLooker for deterministic cost tests.
type fakeCatalog struct {
	prices map[string]*pricing.ModelPricing
}

func (f fakeCatalog) LookupPricing(modelID string) *pricing.ModelPricing {
	return f.prices[modelID]
}

func ptrFloat(f float64) *float64 { return &f }

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := gdb.AutoMigrate(&schema.ConsoleRequest{}, &schema.RequestStats5m{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

func insertRequest(t *testing.T, gdb *gorm.DB, r schema.ConsoleRequest) {
	t.Helper()
	// Rollup only processes rows with completed_at set (response data is written
	// asynchronously), so default it if the caller didn't.
	if r.CompletedAt == nil {
		c := r.CreatedAt + 5000
		r.CompletedAt = &c
	}
	if err := gdb.Create(&r).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func mustTick(t *testing.T, r *Rollup) {
	t.Helper()
	if err := r.RollupTick(context.Background()); err != nil {
		t.Fatalf("rollup tick: %v", err)
	}
}

func allRows(t *testing.T, gdb *gorm.DB) []schema.RequestStats5m {
	t.Helper()
	var rows []schema.RequestStats5m
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	return rows
}

// TestRollupTick_Incremental verifies two ticks process disjoint sets of rows
// and the rollup total equals the sum of both.
func TestRollupTick_Incremental(t *testing.T) {
	gdb := setupTestDB(t)
	cat := fakeCatalog{prices: map[string]*pricing.ModelPricing{
		"gpt-4o": {Input: ptrFloat(5), Output: ptrFloat(15)},
	}}
	rollup := NewRollup(gdb, cat)

	// Bucket base: 1000000 (a 5m boundary). Two requests in the first batch.
	// completed_at must be monotonic since the rollup cursor tracks it.
	insertRequest(t, gdb, schema.ConsoleRequest{
		RequestID: "r1", CreatedAt: 1000000, RoutePrefix: "openai",
		RequestModel: "gpt-4o", ResponseModel: strPtr("gpt-4o"),
		InputTokens: 1000, OutputTokens: 500, CompletedAt: int64Ptr(1001000),
	})
	insertRequest(t, gdb, schema.ConsoleRequest{
		RequestID: "r2", CreatedAt: 1001000, RoutePrefix: "openai",
		RequestModel: "gpt-4o", InputTokens: 2000, OutputTokens: 300,
		CompletedAt: int64Ptr(1002000),
	})

	mustTick(t, rollup)
	rows := allRows(t, gdb)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after first tick, got %d", len(rows))
	}
	if rows[0].Requests != 2 {
		t.Errorf("requests = %d, want 2", rows[0].Requests)
	}
	if rows[0].InputTokens != 3000 {
		t.Errorf("input tokens = %d, want 3000", rows[0].InputTokens)
	}
	if rows[0].OutputTokens != 800 {
		t.Errorf("output tokens = %d, want 800", rows[0].OutputTokens)
	}
	// cost: (1000+2000)/1M*5 + (500+300)/1M*15 = 0.015 + 0.012 = 0.027
	got := rows[0].InputCostUSD + rows[0].OutputCostUSD
	if got < 0.0269 || got > 0.0271 {
		t.Errorf("cost = %v, want ~0.027", got)
	}

	// Second batch: one new request in the SAME bucket (same 5m window).
	insertRequest(t, gdb, schema.ConsoleRequest{
		RequestID: "r3", CreatedAt: 1002000, RoutePrefix: "openai",
		RequestModel: "gpt-4o", InputTokens: 4000, OutputTokens: 100,
		CompletedAt: int64Ptr(1003000),
	})
	mustTick(t, rollup)
	rows = allRows(t, gdb)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after second tick, got %d", len(rows))
	}
	if rows[0].Requests != 3 {
		t.Errorf("requests = %d, want 3", rows[0].Requests)
	}
	if rows[0].InputTokens != 7000 {
		t.Errorf("input tokens = %d, want 7000", rows[0].InputTokens)
	}
}

// TestRollupTick_Idempotent verifies re-running a tick with no new rows is a
// no-op (the cursor prevents re-processing).
func TestRollupTick_Idempotent(t *testing.T) {
	gdb := setupTestDB(t)
	rollup := NewRollup(gdb, nil)

	insertRequest(t, gdb, schema.ConsoleRequest{
		RequestID: "r1", CreatedAt: 1000000, RoutePrefix: "x", RequestModel: "m",
		InputTokens: 10,
	})
	mustTick(t, rollup)

	// Re-run with no new rows — should be a no-op.
	mustTick(t, rollup)
	rows := allRows(t, gdb)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Requests != 1 {
		t.Errorf("requests = %d, want 1", rows[0].Requests)
	}
}

// TestRollupTick_NoPricingSkipsCost verifies requests for models without
// catalog pricing still get counted but contribute 0 cost.
func TestRollupTick_NoPricingSkipsCost(t *testing.T) {
	gdb := setupTestDB(t)
	rollup := NewRollup(gdb, nil) // nil catalog

	insertRequest(t, gdb, schema.ConsoleRequest{
		RequestID: "r1", CreatedAt: 1000000, RoutePrefix: "x", RequestModel: "unknown",
		InputTokens: 100, OutputTokens: 50,
	})
	mustTick(t, rollup)
	rows := allRows(t, gdb)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Requests != 1 {
		t.Errorf("requests = %d, want 1", rows[0].Requests)
	}
	if rows[0].InputCostUSD != 0 {
		t.Errorf("input cost = %v, want 0", rows[0].InputCostUSD)
	}
}

// TestRollupTick_Dimensions verifies rows are split by route/model/client.
func TestRollupTick_Dimensions(t *testing.T) {
	gdb := setupTestDB(t)
	rollup := NewRollup(gdb, nil)

	insertRequest(t, gdb, schema.ConsoleRequest{RequestID: "r1", CreatedAt: 1000000, RoutePrefix: "a", RequestModel: "m1", APIKeyName: strPtr("c1")})
	insertRequest(t, gdb, schema.ConsoleRequest{RequestID: "r2", CreatedAt: 1000000, RoutePrefix: "a", RequestModel: "m1", APIKeyName: strPtr("c2")})
	insertRequest(t, gdb, schema.ConsoleRequest{RequestID: "r3", CreatedAt: 1000000, RoutePrefix: "b", RequestModel: "m1", APIKeyName: strPtr("c1")})
	mustTick(t, rollup)
	rows := allRows(t, gdb)
	if len(rows) != 3 {
		t.Fatalf("expected 3 distinct (route,model,client) rows, got %d", len(rows))
	}
}

func strPtr(s string) *string { return &s }
func int64Ptr(i int64) *int64 { return &i }
