package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const sqliteTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

const (
	// legacyQueryMaxRows caps the untyped /usage dump. It exists purely as an
	// out-of-memory backstop: every stage downstream of Query (JSON marshal, the
	// base64 ABI envelope, the host-side decode) allocates in proportion to the
	// row count, so an unbounded range on a small host is a self-inflicted OOM.
	// Callers that need more than this should page through /usage/records.
	legacyQueryMaxRows = 10000

	// summaryMaxBuckets bounds the aggregated response. Distinct
	// (bucket, api_key, provider, model) combinations are normally in the low
	// hundreds; this only guards against an absurd range at hour granularity.
	summaryMaxBuckets = 20000

	// recordsDefaultLimit / recordsMaxLimit bound one keyset page.
	recordsDefaultLimit = 100
	recordsMaxLimit     = 1000

	// maxFailureBodyBytes bounds the stored upstream error text. Without this a
	// single pathological multi-megabyte error would bloat both the database and
	// every response that lists it.
	maxFailureBodyBytes = 4096

	// failureExcerptBytes is how much of failure_body list endpoints return. The
	// full text is available from the single-record lookup.
	failureExcerptBytes = 500

	// pruneBatchRows / pruneBatchPause chunk retention deletes so a large prune
	// never holds a write lock long enough to stall an in-flight insert.
	pruneBatchRows  = 500
	pruneBatchPause = 50 * time.Millisecond
)

// sqlitePragmas are applied through the DSN rather than db.Exec because
// busy_timeout, cache_size and temp_store are per-connection settings. Running
// them as statements would configure only whichever pooled connection happened
// to serve that call, which silently breaks as soon as the pool grows.
//
// journal_mode=WAL is persisted in the file header and is safe here: the store
// lives on a local filesystem (WAL requires working flock/mmap and must not be
// used over NFS/SMB). Note that WAL adds usage.db-wal and usage.db-shm sidecar
// files, so any backup must use sqlite3 .backup / VACUUM INTO rather than
// copying usage.db alone.
var sqlitePragmas = []string{
	"_pragma=journal_mode(WAL)",
	"_pragma=synchronous(NORMAL)",
	"_pragma=busy_timeout(5000)",
	"_pragma=cache_size(-4000)",
	"_pragma=temp_store(MEMORY)",
}

// SQLiteStore persists usage records in a pure-Go SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (creating if needed) the usage database at path.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("usage sqlite path is empty")
	}
	if err := prepareSQLitePath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?"+strings.Join(sqlitePragmas, "&"))
	if err != nil {
		return nil, fmt.Errorf("usage sqlite open: %w", err)
	}
	// WAL lets readers run concurrently with the single writer, so a slow panel
	// query no longer blocks usage ingestion on the proxied-request hot path.
	// Concurrent writers are serialized by SQLite and wait out busy_timeout.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	store := &SQLiteStore{db: db}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func prepareSQLitePath(path string) error {
	dir := filepath.Clean(filepath.Dir(path))
	if dir != "." && filepath.Dir(dir) != dir {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("usage sqlite mkdir: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("usage sqlite create: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("usage sqlite close created file: %w", err)
	}
	return nil
}

// initSchema creates the table and indexes. Column names are aligned with
// upstream usage.Record / usage.Detail; there is no legacy migration path.
func (s *SQLiteStore) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS usage_records (
	id TEXT PRIMARY KEY,
	timestamp TEXT NOT NULL,
	api_key TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	alias TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	auth_id TEXT NOT NULL DEFAULT '',
	auth_index TEXT NOT NULL DEFAULT '',
	auth_type TEXT NOT NULL DEFAULT '',
	executor_type TEXT NOT NULL DEFAULT '',
	reasoning_effort TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
	ttft_ms INTEGER NOT NULL DEFAULT 0 CHECK (ttft_ms >= 0),
	input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
	output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
	reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
	cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
	cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_creation_tokens >= 0),
	total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
	failed INTEGER NOT NULL DEFAULT 0 CHECK (failed IN (0, 1)),
	failure_status_code INTEGER NOT NULL DEFAULT 0 CHECK (failure_status_code >= 0),
	failure_body TEXT NOT NULL DEFAULT ''
)`,
		// id is TEXT PRIMARY KEY, not the rowid, so a timestamp-only index cannot
		// satisfy ORDER BY timestamp, id -- SQLite fell back to a temp b-tree and
		// sorted every matching row before LIMIT could apply. Indexing both columns
		// makes the scan already-ordered, so keyset paging is O(page) not O(range).
		`CREATE INDEX IF NOT EXISTS idx_usage_ts_id ON usage_records(timestamp, id)`,
		// Redundant leading-column prefix of idx_usage_ts_id.
		`DROP INDEX IF EXISTS idx_usage_records_timestamp`,
		// Near-zero selectivity: a handful of distinct keys/providers/models over
		// tens of thousands of rows, so it never beat seeking the timestamp index
		// and filtering. Records' optional api_key/model filters do the same. Keeping
		// it only cost a b-tree write on every ingestion.
		`DROP INDEX IF EXISTS idx_usage_records_api_model`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("usage sqlite init schema: %w", err)
		}
	}
	return s.migrateSchema(ctx)
}

// migrateSchema 让老版本 db 的字段自愈到最新：用 table_info 读出现有列，
// 对最新 schema 中缺失的列逐个 ALTER TABLE ADD COLUMN 补齐（SQLite 不能重复
// ADD，同名列跳过）。id/timestamp 是建表原始列（任何版本都有）且不可 ADD，
// 不在补齐范围。ADD COLUMN 只带 NOT NULL DEFAULT、不带 CHECK（SQLite 对
// ADD COLUMN 的约束限制），数值非负由写入侧 nonNegative 保证。
func (s *SQLiteStore) migrateSchema(ctx context.Context) error {
	existing, err := s.existingColumns(ctx)
	if err != nil {
		return err
	}
	additions := []struct {
		name string
		ddl  string
	}{
		{"api_key", `ALTER TABLE usage_records ADD COLUMN api_key TEXT NOT NULL DEFAULT ''`},
		{"provider", `ALTER TABLE usage_records ADD COLUMN provider TEXT NOT NULL DEFAULT ''`},
		{"model", `ALTER TABLE usage_records ADD COLUMN model TEXT NOT NULL DEFAULT ''`},
		{"alias", `ALTER TABLE usage_records ADD COLUMN alias TEXT NOT NULL DEFAULT ''`},
		{"source", `ALTER TABLE usage_records ADD COLUMN source TEXT NOT NULL DEFAULT ''`},
		{"auth_id", `ALTER TABLE usage_records ADD COLUMN auth_id TEXT NOT NULL DEFAULT ''`},
		{"auth_index", `ALTER TABLE usage_records ADD COLUMN auth_index TEXT NOT NULL DEFAULT ''`},
		{"auth_type", `ALTER TABLE usage_records ADD COLUMN auth_type TEXT NOT NULL DEFAULT ''`},
		{"executor_type", `ALTER TABLE usage_records ADD COLUMN executor_type TEXT NOT NULL DEFAULT ''`},
		{"reasoning_effort", `ALTER TABLE usage_records ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`},
		{"service_tier", `ALTER TABLE usage_records ADD COLUMN service_tier TEXT NOT NULL DEFAULT ''`},
		{"latency_ms", `ALTER TABLE usage_records ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT 0`},
		{"ttft_ms", `ALTER TABLE usage_records ADD COLUMN ttft_ms INTEGER NOT NULL DEFAULT 0`},
		{"input_tokens", `ALTER TABLE usage_records ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`},
		{"output_tokens", `ALTER TABLE usage_records ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`},
		{"reasoning_tokens", `ALTER TABLE usage_records ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cached_tokens", `ALTER TABLE usage_records ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cache_read_tokens", `ALTER TABLE usage_records ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cache_creation_tokens", `ALTER TABLE usage_records ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`},
		{"total_tokens", `ALTER TABLE usage_records ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`},
		{"failed", `ALTER TABLE usage_records ADD COLUMN failed INTEGER NOT NULL DEFAULT 0`},
		{"failure_status_code", `ALTER TABLE usage_records ADD COLUMN failure_status_code INTEGER NOT NULL DEFAULT 0`},
		{"failure_body", `ALTER TABLE usage_records ADD COLUMN failure_body TEXT NOT NULL DEFAULT ''`},
	}
	for _, addition := range additions {
		if _, ok := existing[addition.name]; ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, addition.ddl); err != nil {
			return fmt.Errorf("usage sqlite migrate add %s: %w", addition.name, err)
		}
	}
	return nil
}

// existingColumns 返回 usage_records 当前已有的列名集合。
func (s *SQLiteStore) existingColumns(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(usage_records)")
	if err != nil {
		return nil, fmt.Errorf("usage sqlite table_info: %w", err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("usage sqlite table_info scan: %w", err)
		}
		columns[name] = struct{}{}
	}
	return columns, rows.Err()
}

// Insert stores one usage record.
func (s *SQLiteStore) Insert(ctx context.Context, record Record) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is nil")
	}
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("usage record id is empty")
	}

	tokens := nonNegativeTokenStats(record.Tokens)
	tokens.TotalTokens = normalizeTotalTokens(tokens)

	_, err := s.db.ExecContext(ctx, `
INSERT INTO usage_records (
	id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, executor_type,
	reasoning_effort, service_tier, latency_ms, ttft_ms,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
	failed, failure_status_code, failure_body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
		strings.TrimSpace(record.ID),
		formatRecordTimestamp(record.Timestamp),
		strings.TrimSpace(record.APIKey),
		strings.TrimSpace(record.Provider),
		normalizeModel(record.Model),
		strings.TrimSpace(record.Alias),
		strings.TrimSpace(record.Source),
		strings.TrimSpace(record.AuthID),
		strings.TrimSpace(record.AuthIndex),
		strings.TrimSpace(record.AuthType),
		strings.TrimSpace(record.ExecutorType),
		strings.TrimSpace(record.ReasoningEffort),
		strings.TrimSpace(record.ServiceTier),
		nonNegative(record.LatencyMs),
		nonNegative(record.TTFTMs),
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.CacheReadTokens,
		tokens.CacheCreationTokens,
		tokens.TotalTokens,
		boolToInt(record.Failed),
		nonNegativeInt(record.FailureStatusCode),
		truncateUTF8(strings.TrimSpace(record.FailureBody), maxFailureBodyBytes),
	)
	if err != nil {
		return fmt.Errorf("usage sqlite insert: %w", err)
	}
	return nil
}

// recordColumns is the shared projection for raw-record reads. failure_body is
// appended separately by each caller because list endpoints return only an
// excerpt while the single-record lookup returns the full text.
const recordColumns = `id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, executor_type,
       reasoning_effort, service_tier, latency_ms, ttft_ms,
       input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
       failed, failure_status_code`

// scanRecord reads one recordColumns + failure_body row. api_key is returned
// separately because the legacy grouped shape carries it as a map key rather
// than a field; callers that want it inline assign it themselves.
func scanRecord(rows *sql.Rows) (RequestDetail, string, error) {
	var (
		timestampText string
		apiKey        string
		failedInt     int
		detail        RequestDetail
	)
	if err := rows.Scan(
		&detail.ID, &timestampText, &apiKey, &detail.Provider, &detail.Model, &detail.Alias,
		&detail.Source, &detail.AuthID, &detail.AuthIndex, &detail.AuthType, &detail.ExecutorType,
		&detail.ReasoningEffort, &detail.ServiceTier, &detail.LatencyMs, &detail.TTFTMs,
		&detail.Tokens.InputTokens, &detail.Tokens.OutputTokens, &detail.Tokens.ReasoningTokens,
		&detail.Tokens.CachedTokens, &detail.Tokens.CacheReadTokens, &detail.Tokens.CacheCreationTokens,
		&detail.Tokens.TotalTokens, &failedInt, &detail.FailureStatusCode, &detail.FailureBody,
	); err != nil {
		return detail, "", fmt.Errorf("usage sqlite scan: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestampText)
	if err != nil {
		return detail, "", fmt.Errorf("usage sqlite parse timestamp: %w", err)
	}
	detail.Timestamp = parsed.UTC()
	detail.LatencyMs = nonNegative(detail.LatencyMs)
	detail.TTFTMs = nonNegative(detail.TTFTMs)
	detail.Failed = failedInt != 0
	return detail, apiKey, nil
}

// rangeConditions builds the timestamp predicates shared by every read path.
func rangeConditions(rng QueryRange) ([]string, []any) {
	conds := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if rng.Start != nil && !rng.Start.IsZero() {
		conds = append(conds, "timestamp >= ?")
		args = append(args, formatTimestamp(*rng.Start))
	}
	if rng.End != nil && !rng.End.IsZero() {
		conds = append(conds, "timestamp < ?")
		args = append(args, formatTimestamp(*rng.End))
	}
	return conds, args
}

func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// Query returns usage grouped by api_key (or provider) then model.
//
// The row count is capped at legacyQueryMaxRows, and the cap keeps the NEWEST
// rows: a usage panel that silently lost recent traffic would render stale.
// The scan therefore walks the range descending and each group is reversed at
// the end to restore the ascending order the legacy shape promises.
//
// One row beyond the cap is requested so truncation is detected exactly rather
// than inferred from "we got exactly the cap", which would flag a range holding
// precisely legacyQueryMaxRows rows as truncated when nothing was dropped.
func (s *SQLiteStore) Query(ctx context.Context, rng QueryRange) (APIUsage, bool, error) {
	if s == nil || s.db == nil {
		return APIUsage{}, false, nil
	}
	conds, args := rangeConditions(rng)
	args = append(args, legacyQueryMaxRows+1)
	query := `SELECT ` + recordColumns + `, failure_body
FROM usage_records` + whereClause(conds) + `
ORDER BY timestamp DESC, id DESC
LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("usage sqlite query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := APIUsage{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned == legacyQueryMaxRows {
			// The probe row exists, so older rows in range were left out.
			truncated = true
			break
		}
		detail, apiKey, err := scanRecord(rows)
		if err != nil {
			return nil, false, err
		}
		detail.FailureBody = truncateUTF8(detail.FailureBody, maxFailureBodyBytes)
		scanned++
		key := groupingKey(apiKey, detail.Provider)
		modelKey := normalizeModel(detail.Model)
		if result[key] == nil {
			result[key] = map[string][]RequestDetail{}
		}
		result[key][modelKey] = append(result[key][modelKey], detail)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("usage sqlite rows: %w", err)
	}
	for _, models := range result {
		for _, details := range models {
			reverseDetails(details)
		}
	}
	return result, truncated, nil
}

// reverseDetails flips a descending scan back to ascending in place.
func reverseDetails(details []RequestDetail) {
	for i, j := 0, len(details)-1; i < j; i, j = i+1, j-1 {
		details[i], details[j] = details[j], details[i]
	}
}

// Summary aggregates usage in SQL instead of shipping raw rows. The response is
// bounded by distinct dimension combinations, which is what makes it cheap: a
// week-long window collapses from thousands of records to a few hundred buckets,
// and every downstream cost (JSON marshal, the base64 ABI envelope, the
// host-side decode) shrinks with it.
//
// Averages deliberately exclude rows that reported no timing: a request with
// ttft_ms = 0 never measured a first byte, and folding those zeros into the mean
// would understate it. avg() skips NULL, so the CASE yields exactly that, and
// the paired sample counts let a caller re-weight averages across buckets.
//
// Like Query, the cap keeps the newest buckets and asks for one extra row so
// truncation is exact.
func (s *SQLiteStore) Summary(ctx context.Context, q SummaryQuery) (SummaryResult, error) {
	result := SummaryResult{Buckets: []SummaryBucket{}, BucketBy: normalizeBucket(string(q.Bucket))}
	if s == nil || s.db == nil {
		return result, nil
	}
	conds, args := rangeConditions(q.Range)
	args = append(args, summaryMaxBuckets+1)
	// The bucket expression comes from a closed enum, never from caller input, so
	// inlining it carries no injection risk and keeps the plan cacheable.
	query := `SELECT ` + result.BucketBy.bucketExpr() + ` AS bucket,
       api_key, provider, model, source, auth_index,
       count(*), sum(failed),
       sum(input_tokens), sum(output_tokens), sum(reasoning_tokens), sum(cached_tokens),
       sum(cache_read_tokens), sum(cache_creation_tokens), sum(total_tokens),
       COALESCE(CAST(avg(CASE WHEN latency_ms > 0 THEN latency_ms END) AS INTEGER), 0),
       COALESCE(max(latency_ms), 0),
       sum(CASE WHEN latency_ms > 0 THEN 1 ELSE 0 END),
       COALESCE(CAST(avg(CASE WHEN ttft_ms > 0 THEN ttft_ms END) AS INTEGER), 0),
       COALESCE(max(ttft_ms), 0),
       sum(CASE WHEN ttft_ms > 0 THEN 1 ELSE 0 END)
FROM usage_records` + whereClause(conds) + `
GROUP BY bucket, api_key, provider, model, source, auth_index
ORDER BY bucket DESC, api_key DESC, provider DESC, model DESC, source DESC, auth_index DESC
LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return result, fmt.Errorf("usage sqlite summary: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(result.Buckets) == summaryMaxBuckets {
			result.Truncated = true
			break
		}
		var b SummaryBucket
		if err := rows.Scan(
			&b.Bucket, &b.APIKey, &b.Provider, &b.Model, &b.Source, &b.AuthIndex,
			&b.Requests, &b.Failures,
			&b.Tokens.InputTokens, &b.Tokens.OutputTokens, &b.Tokens.ReasoningTokens, &b.Tokens.CachedTokens,
			&b.Tokens.CacheReadTokens, &b.Tokens.CacheCreationTokens, &b.Tokens.TotalTokens,
			&b.AvgLatencyMs, &b.MaxLatencyMs, &b.LatencySamples,
			&b.AvgTTFTMs, &b.MaxTTFTMs, &b.TTFTSamples,
		); err != nil {
			return result, fmt.Errorf("usage sqlite summary scan: %w", err)
		}
		result.Buckets = append(result.Buckets, b)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("usage sqlite summary rows: %w", err)
	}
	// Restore the ascending composite-key order the descending scan inverted.
	for i, j := 0, len(result.Buckets)-1; i < j; i, j = i+1, j-1 {
		result.Buckets[i], result.Buckets[j] = result.Buckets[j], result.Buckets[i]
	}
	return result, nil
}

// Records returns one keyset page of raw records for drill-down. Paging seeks on
// (timestamp, id) -- the index order -- so page N costs the same as page 1,
// unlike OFFSET which re-walks every skipped row.
func (s *SQLiteStore) Records(ctx context.Context, q RecordsQuery) (RecordsPage, error) {
	page := RecordsPage{Records: []RequestDetail{}}
	if s == nil || s.db == nil {
		return page, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = recordsDefaultLimit
	}
	if limit > recordsMaxLimit {
		limit = recordsMaxLimit
	}

	conds, args := rangeConditions(q.Range)
	single := strings.TrimSpace(q.ID) != ""
	if single {
		conds = append(conds, "id = ?")
		args = append(args, strings.TrimSpace(q.ID))
	}
	if key := strings.TrimSpace(q.APIKey); key != "" {
		conds = append(conds, "api_key = ?")
		args = append(args, key)
	}
	if provider := strings.TrimSpace(q.Provider); provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, provider)
	}
	if model := strings.TrimSpace(q.Model); model != "" {
		conds = append(conds, "model = ?")
		args = append(args, model)
	}
	if q.FailedOnly {
		conds = append(conds, "failed = 1")
	}
	descending := q.Order == OrderDesc
	if ts := strings.TrimSpace(q.AfterTS); ts != "" {
		// Row-value comparison so SQLite turns the cursor into an index seek. The
		// direction must match the sort order or the cursor walks the wrong way and
		// the caller either loops on one page or skips the rest.
		if descending {
			conds = append(conds, "(timestamp, id) < (?, ?)")
		} else {
			conds = append(conds, "(timestamp, id) > (?, ?)")
		}
		args = append(args, ts, strings.TrimSpace(q.AfterID))
	}

	// List responses carry only an excerpt of failure_body; the single-record
	// lookup returns the full text. This keeps one pathological upstream error
	// from bloating an entire page.
	//
	// SQLite's substr() counts CHARACTERS, not bytes, so this alone would let a
	// CJK excerpt reach 3x failureExcerptBytes. It stays because it cuts the bytes
	// actually read out of the database; the exact byte bound is enforced on the
	// scanned value below.
	bodyExpr := "substr(failure_body, 1, " + strconv.Itoa(failureExcerptBytes) + ")"
	if single {
		bodyExpr = "failure_body"
	}
	// One extra row tells us whether another page exists without a second query.
	args = append(args, limit+1)
	query := `SELECT ` + recordColumns + `, ` + bodyExpr + `
FROM usage_records` + whereClause(conds) + `
ORDER BY timestamp ` + orderKeyword(descending) + `, id ` + orderKeyword(descending) + `
LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, fmt.Errorf("usage sqlite records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		detail, apiKey, err := scanRecord(rows)
		if err != nil {
			return page, err
		}
		detail.APIKey = apiKey
		if single {
			detail.FailureBody = truncateUTF8(detail.FailureBody, maxFailureBodyBytes)
		} else {
			detail.FailureBody = truncateUTF8(detail.FailureBody, failureExcerptBytes)
		}
		page.Records = append(page.Records, detail)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("usage sqlite records rows: %w", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.HasMore = true
		last := page.Records[limit-1]
		page.NextCursor = encodeCursor(formatTimestamp(last.Timestamp), last.ID)
	}
	return page, nil
}

// Delete removes records by id and reports which ids were absent.
func (s *SQLiteStore) Delete(ctx context.Context, ids []string) (DeleteResult, error) {
	result := DeleteResult{Missing: []string{}}
	if s == nil || s.db == nil {
		result.Missing = append(result.Missing, ids...)
		return result, nil
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		res, err := s.db.ExecContext(ctx, "DELETE FROM usage_records WHERE id = ?", id)
		if err != nil {
			return result, fmt.Errorf("usage sqlite delete %s: %w", id, err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("usage sqlite rows affected: %w", err)
		}
		if rows == 0 {
			result.Missing = append(result.Missing, id)
			continue
		}
		result.Deleted += rows
	}
	return result, nil
}

// DeleteBefore removes records older than cutoff and returns the deleted count.
//
// The delete is chunked. A single unbounded DELETE journals every touched table
// and index page in one transaction, which on a large prune holds the write lock
// long enough to stall concurrent ingestion; batching with a short pause between
// chunks keeps each transaction small and yields the lock in between.
//
// The `rowid IN (SELECT ... LIMIT n)` form is used because plain DELETE ... LIMIT
// requires SQLITE_ENABLE_UPDATE_DELETE_LIMIT, which is not compiled in by default.
func (s *SQLiteStore) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	cutoffText := formatTimestamp(cutoff)
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `
DELETE FROM usage_records
WHERE rowid IN (SELECT rowid FROM usage_records WHERE timestamp < ? LIMIT ?)`,
			cutoffText, pruneBatchRows)
		if err != nil {
			return total, fmt.Errorf("usage sqlite delete before: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("usage sqlite rows affected: %w", err)
		}
		total += rows
		if rows < pruneBatchRows {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(pruneBatchPause):
		}
	}
}

// Close closes the underlying database.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func formatTimestamp(timestamp time.Time) string {
	return timestamp.UTC().Format(sqliteTimestampLayout)
}

func formatRecordTimestamp(timestamp time.Time) string {
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return formatTimestamp(timestamp)
}

func groupingKey(apiKey, provider string) string {
	if trimmed := strings.TrimSpace(apiKey); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(provider); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func normalizeModel(model string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func normalizeTotalTokens(tokens TokenStats) int64 {
	if tokens.TotalTokens != 0 {
		return tokens.TotalTokens
	}
	total := tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	if total != 0 {
		return total
	}
	return tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
}

func nonNegativeTokenStats(tokens TokenStats) TokenStats {
	tokens.InputTokens = nonNegative(tokens.InputTokens)
	tokens.OutputTokens = nonNegative(tokens.OutputTokens)
	tokens.ReasoningTokens = nonNegative(tokens.ReasoningTokens)
	tokens.CachedTokens = nonNegative(tokens.CachedTokens)
	tokens.CacheReadTokens = nonNegative(tokens.CacheReadTokens)
	tokens.CacheCreationTokens = nonNegative(tokens.CacheCreationTokens)
	tokens.TotalTokens = nonNegative(tokens.TotalTokens)
	return tokens
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func nonNegativeInt(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// encodeCursor packs a keyset position into one opaque token so callers page by
// echoing it back without knowing the sort key.
func encodeCursor(timestamp, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(timestamp + "\x00" + id))
}

// decodeCursor unpacks a token from encodeCursor. It reports false for anything
// malformed so a bad cursor becomes a 400 rather than a silently wrong page.
func decodeCursor(cursor string) (string, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return "", "", false
	}
	timestamp, id, found := strings.Cut(string(raw), "\x00")
	if !found || id == "" {
		return "", "", false
	}
	// The cursor timestamp is compared against the stored column, so it must be
	// byte-identical to what formatTimestamp writes. Round-tripping enforces
	// exactly that and turns a fabricated or mangled cursor into a clean 400
	// instead of a silently wrong page.
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil || formatTimestamp(parsed) != timestamp {
		return "", "", false
	}
	return timestamp, id, true
}

// truncateUTF8 caps s at limit bytes without splitting a multi-byte rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
