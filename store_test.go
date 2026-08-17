package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		Tokens: TokenStats{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 3, CachedTokens: 4, CacheReadTokens: 1, CacheCreationTokens: 2, TotalTokens: 33},
		Failed: true, FailureStatusCode: 429, FailureBody: "rate limited",
	}
	if err := store.Insert(ctx, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	usage, _, err := store.Query(ctx, QueryRange{})
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
	usage, _, err := store.Query(ctx, QueryRange{Start: &start, End: &end})
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
	usage, _, _ := store.Query(ctx, QueryRange{})
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
	usage, _, err := store.Query(ctx, QueryRange{})
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

// bulkInsert writes n rows in one transaction. It bypasses Insert on purpose so
// row-count tests stay fast; the columns it omits all carry NOT NULL defaults.
func bulkInsert(t *testing.T, store *SQLiteStore, n int, base time.Time) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_records (id, timestamp, api_key, model, total_tokens) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(
			fmt.Sprintf("bulk-%06d", i),
			formatTimestamp(base.Add(time.Duration(i)*time.Second)),
			"k", "m", int64(i),
		); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func countDetails(usage APIUsage) int {
	total := 0
	for _, models := range usage {
		for _, details := range models {
			total += len(details)
		}
	}
	return total
}

// TestStoreAppliesDSNPragmas guards the reason the PRAGMAs live in the DSN:
// busy_timeout/cache_size/temp_store are per-connection settings, so applying
// them with db.Exec would configure only whichever pooled connection ran them.
//
// Every connection in the pool is pinned SIMULTANEOUSLY before checking, because
// querying through the pool would keep reusing one already-configured
// connection and pass even if the others were left at their defaults.
func TestStoreAppliesDSNPragmas(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	conns := make([]*sql.Conn, 0, 4)
	for i := 0; i < 4; i++ {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	for i, conn := range conns {
		var journalMode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if !strings.EqualFold(journalMode, "wal") {
			t.Fatalf("conn %d journal_mode = %q, want wal", i, journalMode)
		}
		var busyTimeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if busyTimeout != 5000 {
			t.Fatalf("conn %d busy_timeout = %d, want 5000", i, busyTimeout)
		}
		var tempStore int
		if err := conn.QueryRowContext(ctx, "PRAGMA temp_store").Scan(&tempStore); err != nil {
			t.Fatalf("conn %d temp_store: %v", i, err)
		}
		if tempStore != 2 { // 2 = MEMORY
			t.Fatalf("conn %d temp_store = %d, want 2 (MEMORY)", i, tempStore)
		}
	}
}

// TestSchemaReplacesLegacyIndexes verifies the composite index exists and that
// the redundant/zero-selectivity ones are gone even on a db that had them.
func TestSchemaReplacesLegacyIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-idx.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	if _, err := old.Exec(`CREATE TABLE usage_records (id TEXT PRIMARY KEY, timestamp TEXT NOT NULL, api_key TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, ddl := range []string{
		`CREATE INDEX idx_usage_records_timestamp ON usage_records(timestamp)`,
		`CREATE INDEX idx_usage_records_api_model ON usage_records(api_key, provider, model)`,
	} {
		if _, err := old.Exec(ddl); err != nil {
			t.Fatalf("create index: %v", err)
		}
	}
	_ = old.Close()

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='usage_records'`)
	if err != nil {
		t.Fatalf("index list: %v", err)
	}
	defer func() { _ = rows.Close() }()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names[name] = true
	}
	if !names["idx_usage_ts_id"] {
		t.Fatalf("idx_usage_ts_id missing: %v", names)
	}
	if names["idx_usage_records_timestamp"] || names["idx_usage_records_api_model"] {
		t.Fatalf("legacy indexes still present: %v", names)
	}
}

// TestQueryKeepsNewestWhenCapped: the cap must drop the OLDEST rows, because a
// usage panel cares about recent traffic. Dropping the tail instead would make
// a wide range silently render stale data.
func TestQueryKeepsNewestWhenCapped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	total := legacyQueryMaxRows + 50
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	bulkInsert(t, store, total, base)

	usage, truncated, err := store.Query(ctx, QueryRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated = false, want true at %d rows", total)
	}
	if got := countDetails(usage); got != legacyQueryMaxRows {
		t.Fatalf("rows = %d, want %d", got, legacyQueryMaxRows)
	}
	details := usage["k"]["m"]
	if details[0].ID != fmt.Sprintf("bulk-%06d", 50) {
		t.Fatalf("oldest kept = %s, want the 50 oldest dropped", details[0].ID)
	}
	if last := details[len(details)-1].ID; last != fmt.Sprintf("bulk-%06d", total-1) {
		t.Fatalf("newest kept = %s, want bulk-%06d", last, total-1)
	}
	// Ascending order must survive the descending inner select.
	for i := 1; i < len(details); i++ {
		if details[i].Timestamp.Before(details[i-1].Timestamp) {
			t.Fatalf("order broken at %d", i)
		}
	}
}

// TestQueryTruncationIsExactAtCap pins the boundary the previous
// `scanned >= cap` check got wrong: a range holding EXACTLY the cap loses
// nothing and must not be advertised as truncated.
func TestQueryTruncationIsExactAtCap(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		rows      int
		truncated bool
		kept      int
	}{
		{"one below cap", legacyQueryMaxRows - 1, false, legacyQueryMaxRows - 1},
		{"exactly cap", legacyQueryMaxRows, false, legacyQueryMaxRows},
		{"one above cap", legacyQueryMaxRows + 1, true, legacyQueryMaxRows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			bulkInsert(t, store, tc.rows, base)
			usage, truncated, err := store.Query(context.Background(), QueryRange{})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if truncated != tc.truncated {
				t.Fatalf("truncated = %v at %d rows, want %v", truncated, tc.rows, tc.truncated)
			}
			if got := countDetails(usage); got != tc.kept {
				t.Fatalf("kept = %d, want %d", got, tc.kept)
			}
		})
	}
}

// TestQueryOrdersTiedTimestampsByID: the descending scan is reversed per group
// to restore ascending order. With identical timestamps only the id tie-breaker
// distinguishes rows, so this catches a reversal that ignores the second sort
// key -- which a unique-timestamp fixture would never notice.
func TestQueryOrdersTiedTimestampsByID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"id-c", "id-a", "id-d", "id-b"} {
		if err := store.Insert(ctx, Record{ID: id, Timestamp: ts, APIKey: "k", Model: "m"}); err != nil {
			t.Fatalf("Insert %s: %v", id, err)
		}
	}
	usage, _, err := store.Query(ctx, QueryRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	details := usage["k"]["m"]
	got := make([]string, 0, len(details))
	for _, d := range details {
		got = append(got, d.ID)
	}
	want := []string{"id-a", "id-b", "id-c", "id-d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestLegacyQueryShapeOmitsAPIKey pins the wire contract the existing frontend
// panel consumes: api_key is a map key, never a field inside a detail.
func TestLegacyQueryShapeOmitsAPIKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Record{ID: "r1", Timestamp: time.Now(), APIKey: "secret-key", Model: "m"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	usage, _, err := store.Query(ctx, QueryRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"api_key"`) {
		t.Fatalf("legacy payload gained an api_key field: %s", raw)
	}
}

func TestSummaryAggregates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	day1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "a", Timestamp: day1, APIKey: "k1", Provider: "p", Model: "m", LatencyMs: 100, Tokens: TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
		{ID: "b", Timestamp: day1.Add(time.Hour), APIKey: "k1", Provider: "p", Model: "m", LatencyMs: 300, Failed: true, FailureStatusCode: 429, Tokens: TokenStats{InputTokens: 20, OutputTokens: 5, TotalTokens: 25}},
		{ID: "c", Timestamp: day2, APIKey: "k1", Provider: "p", Model: "m", LatencyMs: 200, Tokens: TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
	}
	for _, r := range records {
		if err := store.Insert(ctx, r); err != nil {
			t.Fatalf("Insert %s: %v", r.ID, err)
		}
	}

	result, err := store.Summary(ctx, SummaryQuery{Bucket: BucketDay})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(result.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2: %+v", len(result.Buckets), result.Buckets)
	}
	first := result.Buckets[0]
	if first.Bucket != "2026-05-01" {
		t.Fatalf("bucket = %q", first.Bucket)
	}
	if first.Requests != 2 || first.Failures != 1 {
		t.Fatalf("requests=%d failures=%d, want 2/1", first.Requests, first.Failures)
	}
	if first.Tokens.InputTokens != 30 || first.Tokens.TotalTokens != 40 {
		t.Fatalf("tokens = %+v", first.Tokens)
	}
	if first.AvgLatencyMs != 200 || first.MaxLatencyMs != 300 {
		t.Fatalf("avg=%d max=%d, want 200/300", first.AvgLatencyMs, first.MaxLatencyMs)
	}

	hourly, err := store.Summary(ctx, SummaryQuery{Bucket: BucketHour})
	if err != nil {
		t.Fatalf("Summary hourly: %v", err)
	}
	if len(hourly.Buckets) != 3 {
		t.Fatalf("hourly buckets = %d, want 3", len(hourly.Buckets))
	}
	if hourly.Buckets[0].Bucket != "2026-05-01T10" {
		t.Fatalf("hourly bucket = %q", hourly.Buckets[0].Bucket)
	}
}

// TestSummaryRespectsRange makes sure aggregation and filtering compose; an
// aggregate over the wrong window is worse than a slow correct one.
func TestSummaryRespectsRange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	for _, r := range []Record{
		{ID: "before", Timestamp: start.Add(-time.Hour), APIKey: "k", Model: "m"},
		{ID: "in", Timestamp: start.Add(time.Hour), APIKey: "k", Model: "m"},
		{ID: "after", Timestamp: end.Add(time.Hour), APIKey: "k", Model: "m"},
	} {
		if err := store.Insert(ctx, r); err != nil {
			t.Fatalf("Insert %s: %v", r.ID, err)
		}
	}
	result, err := store.Summary(ctx, SummaryQuery{Range: QueryRange{Start: &start, End: &end}, Bucket: BucketDay})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(result.Buckets) != 1 || result.Buckets[0].Requests != 1 {
		t.Fatalf("range filter = %+v", result.Buckets)
	}
}

// TestRecordsKeysetPagination walks every page and asserts the union is exactly
// the input with no duplicates and no gaps -- the two ways keyset paging breaks.
func TestRecordsKeysetPagination(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	total := 25
	bulkInsert(t, store, total, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))

	seen := make([]string, 0, total)
	cursor := ""
	for page := 0; page < 100; page++ {
		q := RecordsQuery{Limit: 10}
		if cursor != "" {
			afterTS, afterID, ok := decodeCursor(cursor)
			if !ok {
				t.Fatalf("cursor %q did not decode", cursor)
			}
			q.AfterTS, q.AfterID = afterTS, afterID
		}
		result, err := store.Records(ctx, q)
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		for _, r := range result.Records {
			seen = append(seen, r.ID)
		}
		if !result.HasMore {
			break
		}
		if result.NextCursor == "" {
			t.Fatalf("has_more with empty cursor on page %d", page)
		}
		cursor = result.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged %d ids, want %d", len(seen), total)
	}
	unique := map[string]bool{}
	for i, id := range seen {
		if unique[id] {
			t.Fatalf("duplicate id %s at %d", id, i)
		}
		unique[id] = true
		if want := fmt.Sprintf("bulk-%06d", i); id != want {
			t.Fatalf("position %d = %s, want %s", i, id, want)
		}
	}
}

func TestRecordsFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, r := range []Record{
		{ID: "ok", Timestamp: now, APIKey: "k1", Provider: "p1", Model: "m1"},
		{ID: "bad", Timestamp: now.Add(time.Second), APIKey: "k1", Provider: "p1", Model: "m1", Failed: true, FailureStatusCode: 500},
		{ID: "other", Timestamp: now.Add(2 * time.Second), APIKey: "k2", Provider: "p2", Model: "m2"},
	} {
		if err := store.Insert(ctx, r); err != nil {
			t.Fatalf("Insert %s: %v", r.ID, err)
		}
	}

	byKey, err := store.Records(ctx, RecordsQuery{APIKey: "k1"})
	if err != nil {
		t.Fatalf("Records by key: %v", err)
	}
	if len(byKey.Records) != 2 {
		t.Fatalf("api_key filter = %d records", len(byKey.Records))
	}
	if byKey.Records[0].APIKey != "k1" {
		t.Fatalf("records endpoint should expose api_key inline, got %+v", byKey.Records[0])
	}

	failed, err := store.Records(ctx, RecordsQuery{FailedOnly: true})
	if err != nil {
		t.Fatalf("Records failed: %v", err)
	}
	if len(failed.Records) != 1 || failed.Records[0].ID != "bad" {
		t.Fatalf("failed filter = %+v", failed.Records)
	}

	byModel, err := store.Records(ctx, RecordsQuery{Model: "m2", Provider: "p2"})
	if err != nil {
		t.Fatalf("Records by model: %v", err)
	}
	if len(byModel.Records) != 1 || byModel.Records[0].ID != "other" {
		t.Fatalf("model filter = %+v", byModel.Records)
	}
}

// TestRecordsExcerptVersusSingleLookup: list responses must stay small, but the
// full error text has to remain reachable or failures become undiagnosable.
func TestRecordsExcerptVersusSingleLookup(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	body := strings.Repeat("e", 2000)
	if err := store.Insert(ctx, Record{ID: "long", Timestamp: time.Now(), APIKey: "k", Model: "m",
		Failed: true, FailureStatusCode: 500, FailureBody: body}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	list, err := store.Records(ctx, RecordsQuery{})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if got := len(list.Records[0].FailureBody); got != failureExcerptBytes {
		t.Fatalf("list excerpt = %d bytes, want %d", got, failureExcerptBytes)
	}

	single, err := store.Records(ctx, RecordsQuery{ID: "long"})
	if err != nil {
		t.Fatalf("Records by id: %v", err)
	}
	if len(single.Records) != 1 {
		t.Fatalf("id lookup = %d records", len(single.Records))
	}
	if got := len(single.Records[0].FailureBody); got != len(body) {
		t.Fatalf("single lookup body = %d bytes, want %d", got, len(body))
	}
}

func TestInsertTruncatesFailureBody(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Record{ID: "huge", Timestamp: time.Now(), APIKey: "k", Model: "m",
		FailureBody: strings.Repeat("x", maxFailureBodyBytes*3)}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	var stored int
	if err := store.db.QueryRow("SELECT length(failure_body) FROM usage_records WHERE id = 'huge'").Scan(&stored); err != nil {
		t.Fatalf("length: %v", err)
	}
	if stored != maxFailureBodyBytes {
		t.Fatalf("stored = %d, want %d", stored, maxFailureBodyBytes)
	}
}

// TestTruncateUTF8KeepsRunesIntact: byte-slicing a multi-byte rune would write
// invalid UTF-8 into the database and break JSON encoding of the response.
func TestTruncateUTF8KeepsRunesIntact(t *testing.T) {
	// Each CJK rune is 3 bytes, so a 10-byte cap lands mid-rune at byte 9..10.
	got := truncateUTF8("用量统计插件", 10)
	if len(got) != 9 {
		t.Fatalf("len = %d, want 9 (3 whole runes)", len(got))
	}
	if got != "用量统" {
		t.Fatalf("got %q", got)
	}
	if short := truncateUTF8("abc", 10); short != "abc" {
		t.Fatalf("under-limit input was modified: %q", short)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	ts := formatTimestamp(time.Date(2026, 5, 2, 10, 30, 0, 0, time.UTC))
	gotTS, gotID, ok := decodeCursor(encodeCursor(ts, "record-1"))
	if !ok || gotTS != ts || gotID != "record-1" {
		t.Fatalf("round trip = %q %q ok=%v", gotTS, gotID, ok)
	}
	if _, _, ok := decodeCursor("not-base64!!"); ok {
		t.Fatalf("malformed cursor accepted")
	}
	if _, _, ok := decodeCursor(encodeCursor("", "")); ok {
		t.Fatalf("empty cursor accepted")
	}
}

// TestDeleteBeforeSpansMultipleBatches proves the chunked delete does not stop
// after its first batch, which is the obvious way batching regresses.
func TestDeleteBeforeSpansMultipleBatches(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	total := pruneBatchRows*2 + 7
	base := time.Now().UTC().Add(-72 * time.Hour)
	bulkInsert(t, store, total, base)
	if err := store.Insert(ctx, Record{ID: "keep", Timestamp: time.Now(), APIKey: "k", Model: "m"}); err != nil {
		t.Fatalf("Insert keep: %v", err)
	}

	deleted, err := store.DeleteBefore(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteBefore: %v", err)
	}
	if deleted != int64(total) {
		t.Fatalf("deleted = %d, want %d", deleted, total)
	}
	usage, _, err := store.Query(ctx, QueryRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := countDetails(usage); got != 1 {
		t.Fatalf("remaining = %d, want 1", got)
	}
}

func TestRouteSuffix(t *testing.T) {
	cases := map[string]string{
		"/v0/management/plugins/usage-statistics/usage":         "usage",
		"/v0/management/plugins/usage-statistics/usage/summary": "summary",
		"/v0/management/plugins/usage-statistics/usage/records": "records",
		"/plugins/usage-statistics/usage/records/":              "records",
		"/plugins/usage-statistics/usage/summary":               "summary",
		// Hosts that send no path at all must keep reaching the legacy route.
		"": "usage",
	}
	for path, want := range cases {
		if got := routeSuffix(path); got != want {
			t.Fatalf("routeSuffix(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestRecordsPagesThroughTiedTimestamps: every row shares one timestamp, so
// paging depends entirely on the id half of the (timestamp, id) cursor. The
// unique-timestamp fixture in TestRecordsKeysetPagination would still pass if
// the tie-breaker were dropped; this one would loop forever or skip rows.
func TestRecordsPagesThroughTiedTimestamps(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	total := 25
	for i := 0; i < total; i++ {
		if err := store.Insert(ctx, Record{
			ID: fmt.Sprintf("tied-%03d", i), Timestamp: ts, APIKey: "k", Model: "m",
		}); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	seen := make([]string, 0, total)
	cursor := ""
	for page := 0; page < 100; page++ {
		q := RecordsQuery{Limit: 10}
		if cursor != "" {
			afterTS, afterID, ok := decodeCursor(cursor)
			if !ok {
				t.Fatalf("cursor %q did not decode", cursor)
			}
			q.AfterTS, q.AfterID = afterTS, afterID
		}
		result, err := store.Records(ctx, q)
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		if len(result.Records) == 0 {
			t.Fatalf("page %d returned nothing; cursor is not advancing", page)
		}
		for _, r := range result.Records {
			seen = append(seen, r.ID)
		}
		if !result.HasMore {
			break
		}
		cursor = result.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged %d ids, want %d", len(seen), total)
	}
	for i, id := range seen {
		if want := fmt.Sprintf("tied-%03d", i); id != want {
			t.Fatalf("position %d = %s, want %s", i, id, want)
		}
	}
}

// TestRecordsExcerptIsByteBounded: SQLite's substr() counts characters, not
// bytes, so a CJK failure body would blow past the documented 500-byte excerpt
// if the Go-side bound were removed. An ASCII fixture cannot catch that.
func TestRecordsExcerptIsByteBounded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	body := strings.Repeat("\u9519", 1000) // 3 bytes per rune
	if err := store.Insert(ctx, Record{ID: "cjk", Timestamp: time.Now(), APIKey: "k", Model: "m",
		Failed: true, FailureStatusCode: 500, FailureBody: body}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	list, err := store.Records(ctx, RecordsQuery{})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	excerpt := list.Records[0].FailureBody
	if len(excerpt) > failureExcerptBytes {
		t.Fatalf("excerpt = %d bytes, want <= %d", len(excerpt), failureExcerptBytes)
	}
	if !utf8.ValidString(excerpt) {
		t.Fatalf("excerpt is not valid UTF-8: %q", excerpt)
	}
	// The stored value is itself capped, so the single lookup must respect that
	// bound too rather than returning the original 3000 bytes.
	single, err := store.Records(ctx, RecordsQuery{ID: "cjk"})
	if err != nil {
		t.Fatalf("Records by id: %v", err)
	}
	full := single.Records[0].FailureBody
	if len(full) > maxFailureBodyBytes {
		t.Fatalf("stored body = %d bytes, want <= %d", len(full), maxFailureBodyBytes)
	}
	if !utf8.ValidString(full) {
		t.Fatalf("stored body is not valid UTF-8")
	}
	if len(full) <= len(excerpt) {
		t.Fatalf("single lookup (%d) should return more than the excerpt (%d)", len(full), len(excerpt))
	}
}

// TestCleanupWorkerRestartsAndJoins covers the lifecycle closeStore depends on:
// stopCleanupWorker must WAIT for the loop to exit, otherwise the database could
// be closed while a retention prune is mid-batch.
func TestCleanupWorkerRestartsAndJoins(t *testing.T) {
	t.Cleanup(stopCleanupWorker)

	startCleanupWorker()
	cleanupMu.Lock()
	first := cleanupDone
	cleanupMu.Unlock()
	if first == nil {
		t.Fatal("worker did not start")
	}

	// A second configure must replace the worker, not leak a second one.
	startCleanupWorker()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not stop the previous worker")
	}
	cleanupMu.Lock()
	second := cleanupDone
	cleanupMu.Unlock()
	if second == nil || second == first {
		t.Fatal("restart did not install a new worker")
	}

	stopCleanupWorker()
	select {
	case <-second:
	default:
		t.Fatal("stopCleanupWorker returned before the loop exited")
	}
	// Must be safe when nothing is running (closeStore can be called twice).
	stopCleanupWorker()
}

// callManagement drives a request through the real ABI entry point and unwraps
// the envelope exactly as the host does.
func callManagement(t *testing.T, req managementRequest) managementResponse {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleMethod("management.handle", raw)
	if err != nil {
		t.Fatalf("handleMethod: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	return resp
}

// TestManagementRoutesThroughABI exercises registration, path dispatch, the
// response envelope and the truncation headers through handleMethod -- the
// actual entry point the host calls. TestRouteSuffix alone only covers string
// classification and would pass even if no handler were wired up.
func TestManagementRoutesThroughABI(t *testing.T) {
	if err := ensureStore(pluginConfig{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("ensureStore: %v", err)
	}
	t.Cleanup(closeStore)

	// Every declared route must be reachable, or the host never mounts it.
	declared := map[string]bool{}
	for _, route := range managementRegistration().Routes {
		declared[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		"GET " + managementUsagePath,
		"GET " + managementUsageSummaryPath,
		"GET " + managementUsageRecordsPath,
		"DELETE " + managementUsagePath,
	} {
		if !declared[want] {
			t.Fatalf("route %q not registered: %v", want, declared)
		}
	}

	store, release := acquireStore()
	if store == nil {
		t.Fatal("store unavailable after ensureStore")
	}
	bulkInsert(t, store, legacyQueryMaxRows+1, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	release()

	summary := callManagement(t, managementRequest{Method: "GET", Path: managementUsageSummaryPath,
		Query: map[string][]string{"bucket": {"day"}}})
	if summary.StatusCode != 200 {
		t.Fatalf("summary status = %d body=%s", summary.StatusCode, summary.Body)
	}
	var summaryBody SummaryResult
	if err := json.Unmarshal(summary.Body, &summaryBody); err != nil {
		t.Fatalf("summary body: %v", err)
	}
	if len(summaryBody.Buckets) == 0 || summaryBody.BucketBy != BucketDay {
		t.Fatalf("summary body = %+v", summaryBody)
	}
	// The whole point of the endpoint: it must not ship the raw records.
	if len(summary.Body) > 64*1024 {
		t.Fatalf("summary body = %d bytes; aggregation is not happening", len(summary.Body))
	}

	records := callManagement(t, managementRequest{Method: "GET", Path: managementUsageRecordsPath,
		Query: map[string][]string{"limit": {"10"}}})
	var page RecordsPage
	if err := json.Unmarshal(records.Body, &page); err != nil {
		t.Fatalf("records body: %v", err)
	}
	if len(page.Records) != 10 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("records page = %d records has_more=%v cursor=%q", len(page.Records), page.HasMore, page.NextCursor)
	}

	badCursor := callManagement(t, managementRequest{Method: "GET", Path: managementUsageRecordsPath,
		Query: map[string][]string{"cursor": {"!!!not-base64!!!"}}})
	if badCursor.StatusCode != 400 {
		t.Fatalf("bad cursor status = %d, want 400", badCursor.StatusCode)
	}

	legacy := callManagement(t, managementRequest{Method: "GET", Path: managementUsagePath})
	if legacy.StatusCode != 200 {
		t.Fatalf("legacy status = %d", legacy.StatusCode)
	}
	if got := legacy.Headers["X-Usage-Truncated"]; len(got) == 0 || got[0] != "true" {
		t.Fatalf("X-Usage-Truncated = %v, want [true] at %d rows", got, legacyQueryMaxRows+1)
	}
	if got := legacy.Headers["X-Usage-Row-Limit"]; len(got) == 0 || got[0] != "10000" {
		t.Fatalf("X-Usage-Row-Limit = %v", got)
	}

	notAllowed := callManagement(t, managementRequest{Method: "PATCH", Path: managementUsagePath})
	if notAllowed.StatusCode != 405 {
		t.Fatalf("PATCH status = %d, want 405", notAllowed.StatusCode)
	}
}
