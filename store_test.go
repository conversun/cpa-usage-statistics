package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestInsertQueryDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 5, 2, 10, 30, 0, 0, time.UTC)
	rec := Record{
		ID: "record-1", Timestamp: ts, APIKey: "api-key-1", Provider: "anthropic",
		Model: "claude-sonnet-4-5", Source: "claude-code", AuthIndex: "auth-1",
		ReasoningEffort: "high", ServiceTier: "priority", LatencyMs: 1250, TTFTMs: 200,
		Tokens:      TokenStats{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 3, CachedTokens: 4, CacheReadTokens: 1, CacheCreationTokens: 2, TotalTokens: 33},
		Failed:      true, FailureStatusCode: 429, FailureBody: "rate limited",
	}
	if err := store.Insert(ctx, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	usage, err := store.Query(ctx, QueryRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	details := usage["api-key-1"]["claude-sonnet-4-5"]
	if len(details) != 1 {
		t.Fatalf("details len = %d payload=%v", len(details), usage)
	}
	d := details[0]
	if d.ID != "record-1" || d.ReasoningEffort != "high" || d.TTFTMs != 200 || d.LatencyMs != 1250 {
		t.Fatalf("detail = %+v", d)
	}
	if d.Tokens.CacheReadTokens != 1 || d.Tokens.CacheCreationTokens != 2 || d.Tokens.TotalTokens != 33 {
		t.Fatalf("tokens = %+v", d.Tokens)
	}
	if !d.Failed || d.FailureStatusCode != 429 || d.FailureBody != "rate limited" {
		t.Fatalf("failure = %+v", d)
	}

	res, err := store.Delete(ctx, []string{"record-1", "missing"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.Deleted != 1 || len(res.Missing) != 1 || res.Missing[0] != "missing" {
		t.Fatalf("delete result = %+v", res)
	}
}

func TestQueryFiltersByRange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 2, 11, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "before", Timestamp: start.Add(-time.Minute), APIKey: "k", Model: "m"},
		{ID: "in", Timestamp: start.Add(30 * time.Minute), APIKey: "k", Model: "m"},
		{ID: "after", Timestamp: end.Add(time.Minute), APIKey: "k", Model: "m"},
	}
	for _, r := range records {
		if err := store.Insert(ctx, r); err != nil {
			t.Fatalf("Insert %s: %v", r.ID, err)
		}
	}
	usage, err := store.Query(ctx, QueryRange{Start: &start, End: &end})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	details := usage["k"]["m"]
	if len(details) != 1 || details[0].ID != "in" {
		t.Fatalf("range filter = %+v", usage)
	}
}

func TestDeleteBefore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	if err := store.Insert(ctx, Record{ID: "old", Timestamp: old, APIKey: "k", Model: "m"}); err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	if err := store.Insert(ctx, Record{ID: "recent", Timestamp: recent, APIKey: "k", Model: "m"}); err != nil {
		t.Fatalf("Insert recent: %v", err)
	}
	n, err := store.DeleteBefore(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	usage, _ := store.Query(ctx, QueryRange{})
	if len(usage["k"]["m"]) != 1 {
		t.Fatalf("remaining = %+v", usage)
	}
}

func TestToRecordMapping(t *testing.T) {
	rec := usageRecord{
		APIKey: "k", Model: "m", ReasoningEffort: "medium",
		Latency: int64(2 * time.Second), TTFT: int64(300 * time.Millisecond),
		Failed: false, Failure: usageFailure{StatusCode: 500, Body: "err"},
		Detail: usageDetail{InputTokens: 5, CacheReadTokens: 7, CacheCreationTokens: 9},
	}
	r := toRecord(rec)
	if r.LatencyMs != 2000 {
		t.Fatalf("latency ms = %d, want 2000", r.LatencyMs)
	}
	if r.TTFTMs != 300 {
		t.Fatalf("ttft ms = %d, want 300", r.TTFTMs)
	}
	if r.ReasoningEffort != "medium" {
		t.Fatalf("effort = %q", r.ReasoningEffort)
	}
	if !r.Failed {
		t.Fatalf("failed should be true from status 500")
	}
	if r.Tokens.CacheReadTokens != 7 || r.Tokens.CacheCreationTokens != 9 {
		t.Fatalf("cache tokens = %+v", r.Tokens)
	}
	if r.ID == "" {
		t.Fatalf("id should be generated")
	}
}

func TestParseConfig(t *testing.T) {
	cfg := parseConfig([]byte("enabled: true\npriority: 120\ndata_dir: /tmp/usage\nretention_days: 45\n"))
	if cfg.DataDir != "/tmp/usage" {
		t.Fatalf("data_dir = %q", cfg.DataDir)
	}
	if cfg.RetentionDays != 45 {
		t.Fatalf("retention_days = %d", cfg.RetentionDays)
	}

	zh := parseConfig([]byte("用量保留天数: 7\n"))
	if zh.RetentionDays != 7 {
		t.Fatalf("chinese retention = %d", zh.RetentionDays)
	}
}

// TestMigrateSchemaAddsMissingColumns 验证 schema 自愈：用一个缺少多列的老表
// 打开 store，NewSQLiteStore 应通过 migrateSchema 补齐所有最新列，且老数据保留、
// 补列后可正常读写。
func TestMigrateSchemaAddsMissingColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// 建一个只有早期少数列的老表（缺 alias/auth_id/executor_type/reasoning_effort/
	// service_tier/ttft_ms/reasoning_tokens/cache_read_tokens/cache_creation_tokens/
	// failed/failure_* 等）。
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	if _, err := old.Exec(`CREATE TABLE usage_records (
		id TEXT PRIMARY KEY,
		timestamp TEXT NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO usage_records (id, timestamp, api_key, model, input_tokens, total_tokens) VALUES (?, ?, ?, ?, ?, ?)`,
		"legacy-1", "2026-05-02T10:30:00.000000000Z", "k", "m", 11, 11); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	_ = old.Close()

	// 用插件打开：应触发 migrateSchema 补齐所有缺列。
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore(old): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	cols, err := store.existingColumns(ctx)
	if err != nil {
		t.Fatalf("existingColumns: %v", err)
	}
	for _, want := range []string{
		"alias", "auth_id", "auth_index", "auth_type", "executor_type",
		"reasoning_effort", "service_tier", "ttft_ms",
		"reasoning_tokens", "cache_read_tokens", "cache_creation_tokens",
		"failed", "failure_status_code", "failure_body",
	} {
		if _, ok := cols[want]; !ok {
			t.Fatalf("column %q not added by migrateSchema", want)
		}
	}

	// 老数据保留，且能按新 schema 查询；补出的新列取默认值。
	usage, err := store.Query(ctx, QueryRange{})
	if err != nil {
		t.Fatalf("Query after migrate: %v", err)
	}
	details := usage["k"]["m"]
	if len(details) != 1 || details[0].ID != "legacy-1" {
		t.Fatalf("legacy row lost: %+v", usage)
	}
	if details[0].Tokens.InputTokens != 11 {
		t.Fatalf("legacy input tokens = %d", details[0].Tokens.InputTokens)
	}
	if details[0].Tokens.CacheReadTokens != 0 || details[0].Tokens.CacheCreationTokens != 0 {
		t.Fatalf("new cache cols should default 0: %+v", details[0].Tokens)
	}

	// 补列后仍可正常写入含新字段的数据。
	if err := store.Insert(ctx, Record{ID: "new-1", Timestamp: time.Now(), APIKey: "k", Model: "m",
		ReasoningEffort: "high", Tokens: TokenStats{InputTokens: 5, CacheCreationTokens: 3, TotalTokens: 8}}); err != nil {
		t.Fatalf("Insert after migrate: %v", err)
	}
}
