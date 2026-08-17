package main

import "time"

// TokenStats holds token accounting for one request, aligned with upstream
// usage.Detail field semantics.
type TokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

// Record is one persisted usage row. Column names mirror upstream
// usage.Record / usage.Detail naming to avoid ambiguity.
type Record struct {
	ID                string
	Timestamp         time.Time
	APIKey            string
	Provider          string
	Model             string
	Alias             string
	Source            string
	AuthID            string
	AuthIndex         string
	AuthType          string
	ExecutorType      string
	ReasoningEffort   string
	ServiceTier       string
	LatencyMs         int64
	TTFTMs            int64
	Tokens            TokenStats
	Failed            bool
	FailureStatusCode int
	FailureBody       string
}

// RequestDetail is the outward JSON shape for one raw record. The legacy grouped
// endpoint leaves APIKey empty because api_key and model live in its map keys;
// the records endpoint sets APIKey so a flat list stays self-describing.
type RequestDetail struct {
	ID                string     `json:"id"`
	Timestamp         time.Time  `json:"timestamp"`
	Provider          string     `json:"provider,omitempty"`
	Model             string     `json:"model,omitempty"`
	Alias             string     `json:"alias,omitempty"`
	Source            string     `json:"source"`
	APIKey            string     `json:"api_key,omitempty"`
	AuthID            string     `json:"auth_id,omitempty"`
	AuthIndex         string     `json:"auth_index"`
	AuthType          string     `json:"auth_type,omitempty"`
	ExecutorType      string     `json:"executor_type,omitempty"`
	ReasoningEffort   string     `json:"reasoning_effort"`
	ServiceTier       string     `json:"service_tier"`
	LatencyMs         int64      `json:"latency_ms"`
	TTFTMs            int64      `json:"ttft_ms"`
	Tokens            TokenStats `json:"tokens"`
	Failed            bool       `json:"failed"`
	FailureStatusCode int        `json:"failure_status_code,omitempty"`
	FailureBody       string     `json:"failure_body,omitempty"`
}

// APIUsage groups details by grouping key (api_key or provider) then by model.
type APIUsage map[string]map[string][]RequestDetail

// QueryRange bounds a usage query by timestamp.
type QueryRange struct {
	Start *time.Time
	End   *time.Time
}

// DeleteResult reports the outcome of a delete-by-id request.
type DeleteResult struct {
	Deleted int64    `json:"deleted"`
	Missing []string `json:"missing"`
}

// BucketSize selects the summary time-bucket granularity.
type BucketSize string

const (
	// BucketFifteenMin exists because a service-health grid needs sub-hour cells;
	// hourly buckets cannot be split back into quarters client-side.
	BucketFifteenMin BucketSize = "15m"
	BucketHour       BucketSize = "hour"
	BucketDay        BucketSize = "day"
	BucketMonth      BucketSize = "month"
)

// bucketExpr returns the SQL expression mapping a stored timestamp to this
// bucket's key. The stored value is a fixed-width RFC3339Nano UTC string
// (2026-08-17T06:32:58.679824136Z), so most granularities are a plain prefix.
//
// The quarter-hour case floors the minute instead: characters 1..14 are
// "2026-08-17T06:" and characters 15..16 are the minute, so dividing by 15 and
// multiplying back yields 2026-08-17T06:30.
func (b BucketSize) bucketExpr() string {
	switch b {
	case BucketFifteenMin:
		return `substr(timestamp, 1, 14) || printf('%02d', (CAST(substr(timestamp, 15, 2) AS INTEGER) / 15) * 15)`
	case BucketHour:
		return `substr(timestamp, 1, 13)`
	case BucketMonth:
		return `substr(timestamp, 1, 7)`
	default:
		return `substr(timestamp, 1, 10)`
	}
}

// normalizeBucket maps free-form input to a supported bucket, defaulting to day.
func normalizeBucket(raw string) BucketSize {
	switch BucketSize(raw) {
	case BucketFifteenMin:
		return BucketFifteenMin
	case BucketHour:
		return BucketHour
	case BucketMonth:
		return BucketMonth
	default:
		return BucketDay
	}
}

// SummaryQuery bounds an aggregated usage query.
type SummaryQuery struct {
	Range  QueryRange
	Bucket BucketSize
}

// SummaryBucket is one pre-aggregated row. Aggregation happens in SQL, so the
// response size is bounded by distinct dimension combinations rather than by the
// number of underlying requests.
//
// source and auth_index are part of the key because callers attribute usage to a
// credential, not just to an API key: one key can be served by many auth entries.
type SummaryBucket struct {
	Bucket    string `json:"bucket"`
	APIKey    string `json:"api_key"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Source    string `json:"source"`
	AuthIndex string `json:"auth_index"`

	Requests int64      `json:"requests"`
	Failures int64      `json:"failures"`
	Tokens   TokenStats `json:"tokens"`

	// Averages are taken over the rows that actually reported a value, and the
	// sample counts are returned so callers can re-weight across buckets. Without
	// them a client cannot combine two buckets' averages correctly, and rows that
	// never reported a timing would silently drag the mean toward zero.
	AvgLatencyMs   int64 `json:"avg_latency_ms"`
	MaxLatencyMs   int64 `json:"max_latency_ms"`
	LatencySamples int64 `json:"latency_samples"`
	AvgTTFTMs      int64 `json:"avg_ttft_ms"`
	MaxTTFTMs      int64 `json:"max_ttft_ms"`
	TTFTSamples    int64 `json:"ttft_samples"`
}

// SummaryResult wraps the buckets plus the truncation flag.
type SummaryResult struct {
	Buckets   []SummaryBucket `json:"buckets"`
	BucketBy  BucketSize      `json:"bucket_by"`
	Truncated bool            `json:"truncated"`
}

// RecordsQuery selects raw records for drill-down. Paging is keyset based on
// (timestamp, id), which is the table's sort order, so page N costs the same as
// page 1 -- unlike OFFSET, which re-walks every skipped row.
type RecordsQuery struct {
	Range      QueryRange
	ID         string
	APIKey     string
	Provider   string
	Model      string
	FailedOnly bool
	AfterTS    string
	AfterID    string
	Limit      int
}

// RecordsPage is one keyset page of raw records.
type RecordsPage struct {
	Records    []RequestDetail `json:"records"`
	NextCursor string          `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`
}
