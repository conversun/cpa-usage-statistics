package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	abiVersion uint32 = 1
	pluginID          = "usage-statistics"
	pluginName        = "Usage Statistics"
	// managementUsagePath is the plugin-owned management route path. The host
	// mounts it under /v0/management, producing
	// /v0/management/plugins/usage-statistics/usage.
	managementUsagePath        = "/plugins/" + pluginID + "/usage"
	managementUsageSummaryPath = managementUsagePath + "/summary"
	managementUsageRecordsPath = managementUsagePath + "/records"

	insertTimeout = 5 * time.Second
	// queryTimeout is deliberately separate from insertTimeout. Sharing one
	// budget meant a slow read and a dropped usage record competed for the same
	// deadline; a read may legitimately take longer than a write.
	queryTimeout = 30 * time.Second
	// cleanupTimeout bounds one full retention pass. It is generous because the
	// pass now runs on a background ticker rather than in front of a user request.
	cleanupTimeout      = 10 * time.Minute
	cleanupInterval     = time.Hour
	cleanupInitialDelay = time.Minute
)

// Overridable at build time via -ldflags "-X main.pluginVersion=...".
var (
	pluginVersion    = "0.2.0"
	pluginAuthor     = "Fwindy"
	pluginRepository = "https://github.com/Fwindy/cpa-usage-statistics"
)

// ---- ABI envelope ----------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {}

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ---- method dispatch -------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configurePlugin(request); err != nil {
			return nil, err
		}
		return okEnvelope(registerResponse())
	case "usage.handle":
		return handleUsage(request)
	case "management.register":
		return okEnvelope(managementRegistration())
	case "management.handle":
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---- registration metadata -------------------------------------------------

type registerResponsePayload struct {
	SchemaVersion int              `json:"schema_version"`
	Metadata      metadataInfo     `json:"metadata"`
	Capabilities  capabilitiesInfo `json:"capabilities"`
}

type metadataInfo struct {
	Name             string            `json:"Name"`
	Version          string            `json:"Version"`
	Author           string            `json:"Author"`
	GitHubRepository string            `json:"GitHubRepository"`
	Logo             string            `json:"Logo"`
	ConfigFields     []configFieldInfo `json:"ConfigFields"`
}

type configFieldInfo struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type capabilitiesInfo struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

func registerResponse() registerResponsePayload {
	return registerResponsePayload{
		SchemaVersion: 1,
		Metadata: metadataInfo{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields: []configFieldInfo{
				{Name: "data_dir", Type: "string", Description: "Directory for usage.db. Defaults to ~/.cli-proxy-api/plugins/usage-statistics."},
				{Name: "retention_days", Type: "integer", Description: "Delete usage records older than this many days. 0 disables cleanup."},
			},
		},
		Capabilities: capabilitiesInfo{UsagePlugin: true, ManagementAPI: true},
	}
}

type managementRegistrationPayload struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

func managementRegistration() managementRegistrationPayload {
	return managementRegistrationPayload{
		Routes: []managementRoute{
			{Method: "GET", Path: managementUsagePath, Description: "Query persisted usage grouped by api key and model. Returns the newest records up to a fixed cap; prefer /usage/summary for wide ranges."},
			{Method: "GET", Path: managementUsageSummaryPath, Description: "Aggregated usage per time bucket, api key, provider and model. bucket=hour|day|month."},
			{Method: "GET", Path: managementUsageRecordsPath, Description: "Keyset-paginated raw usage records. Filters: id, api_key, provider, model, failed, limit, cursor."},
			{Method: "DELETE", Path: managementUsagePath, Description: "Delete persisted usage records by id."},
		},
	}
}

// ---- usage ingestion -------------------------------------------------------

type usageRecord struct {
	Provider        string       `json:"Provider"`
	ExecutorType    string       `json:"ExecutorType"`
	Model           string       `json:"Model"`
	Alias           string       `json:"Alias"`
	APIKey          string       `json:"APIKey"`
	AuthID          string       `json:"AuthID"`
	AuthIndex       string       `json:"AuthIndex"`
	AuthType        string       `json:"AuthType"`
	Source          string       `json:"Source"`
	ReasoningEffort string       `json:"ReasoningEffort"`
	ServiceTier     string       `json:"ServiceTier"`
	RequestedAt     time.Time    `json:"RequestedAt"`
	Latency         int64        `json:"Latency"`
	TTFT            int64        `json:"TTFT"`
	Failed          bool         `json:"Failed"`
	Failure         usageFailure `json:"Failure"`
	Detail          usageDetail  `json:"Detail"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

func handleUsage(request []byte) ([]byte, error) {
	if len(request) == 0 {
		return okEnvelope(map[string]any{"ignored": true})
	}
	var rec usageRecord
	if err := json.Unmarshal(request, &rec); err != nil {
		return okEnvelope(map[string]any{"ignored": true})
	}
	store, release := acquireStore()
	if store == nil {
		return okEnvelope(map[string]any{"ignored": true})
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	if err := store.Insert(ctx, toRecord(rec)); err != nil {
		return nil, err
	}
	return okEnvelope(map[string]any{"stored": true})
}

func toRecord(rec usageRecord) Record {
	ts := rec.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	return Record{
		ID:              uuid.NewString(),
		Timestamp:       ts.UTC(),
		APIKey:          strings.TrimSpace(rec.APIKey),
		Provider:        strings.TrimSpace(rec.Provider),
		Model:           normalizeModel(rec.Model),
		Alias:           strings.TrimSpace(rec.Alias),
		Source:          strings.TrimSpace(rec.Source),
		AuthID:          strings.TrimSpace(rec.AuthID),
		AuthIndex:       strings.TrimSpace(rec.AuthIndex),
		AuthType:        strings.TrimSpace(rec.AuthType),
		ExecutorType:    strings.TrimSpace(rec.ExecutorType),
		ReasoningEffort: strings.TrimSpace(rec.ReasoningEffort),
		ServiceTier:     strings.TrimSpace(rec.ServiceTier),
		LatencyMs:       nsToMs(rec.Latency),
		TTFTMs:          nsToMs(rec.TTFT),
		Tokens: TokenStats{
			InputTokens:         rec.Detail.InputTokens,
			OutputTokens:        rec.Detail.OutputTokens,
			ReasoningTokens:     rec.Detail.ReasoningTokens,
			CachedTokens:        rec.Detail.CachedTokens,
			CacheReadTokens:     rec.Detail.CacheReadTokens,
			CacheCreationTokens: rec.Detail.CacheCreationTokens,
			TotalTokens:         rec.Detail.TotalTokens,
		},
		Failed:            rec.Failed || rec.Failure.StatusCode >= 400,
		FailureStatusCode: rec.Failure.StatusCode,
		FailureBody:       strings.TrimSpace(rec.Failure.Body),
	}
}

func nsToMs(ns int64) int64 {
	if ns <= 0 {
		return 0
	}
	return ns / int64(time.Millisecond)
}

// ---- management handling ---------------------------------------------------

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type deleteUsageRequest struct {
	IDs []string `json:"ids"`
}

func handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	switch strings.ToUpper(strings.TrimSpace(req.Method)) {
	case "GET":
		switch routeSuffix(req.Path) {
		case "summary":
			return usageSummaryGet(req)
		case "records":
			return usageRecordsGet(req)
		default:
			return usageGet(req)
		}
	case "DELETE":
		return usageDelete(req)
	default:
		return okEnvelope(jsonManagementResponse(405, map[string]string{"error": "method not allowed"}))
	}
}

// routeSuffix classifies a request by its trailing path segments. The host may
// present the path with or without its /v0/management prefix, so match on the
// suffix rather than on equality. Anything unrecognized falls through to the
// legacy usage route, which is also what hosts that send no path at all get.
func routeSuffix(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	switch {
	case strings.HasSuffix(trimmed, "/usage/summary"):
		return "summary"
	case strings.HasSuffix(trimmed, "/usage/records"):
		return "records"
	default:
		return "usage"
	}
}

func usageGet(req managementRequest) ([]byte, error) {
	rng, errResp := parseUsageRange(req.Query)
	if errResp != nil {
		return okEnvelope(*errResp)
	}
	store, release := acquireStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(200, APIUsage{}))
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	result, truncated, err := store.Query(ctx, rng)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to query usage"}))
	}
	resp := jsonManagementResponse(200, result)
	if truncated {
		// Advisory only: the range hit the row cap, so the caller is seeing the
		// newest records and older ones in range were left out. /usage/summary and
		// /usage/records cover wide ranges without dropping anything.
		resp.Headers["X-Usage-Truncated"] = []string{"true"}
		resp.Headers["X-Usage-Row-Limit"] = []string{strconv.Itoa(legacyQueryMaxRows)}
	}
	return okEnvelope(resp)
}

func usageDelete(req managementRequest) ([]byte, error) {
	store, release := acquireStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "usage store unavailable"}))
	}
	defer release()
	var body deleteUsageRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "invalid body"}))
		}
	}
	ids := make([]string, 0, len(body.IDs))
	seen := make(map[string]struct{}, len(body.IDs))
	for _, id := range body.IDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		ids = append(ids, trimmed)
	}
	if len(ids) == 0 {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "ids required"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := store.Delete(ctx, ids)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to delete usage records"}))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

// usageSummaryGet answers with SQL-side aggregates instead of raw rows. This is
// the route a dashboard should poll: its response scales with the number of
// distinct bucket/key/model combinations, not with request volume.
func usageSummaryGet(req managementRequest) ([]byte, error) {
	rng, errResp := parseUsageRange(req.Query)
	if errResp != nil {
		return okEnvelope(*errResp)
	}
	store, release := acquireStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(200, SummaryResult{Buckets: []SummaryBucket{}, BucketBy: BucketDay}))
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	result, err := store.Summary(ctx, SummaryQuery{
		Range:  rng,
		Bucket: normalizeBucket(strings.TrimSpace(firstValue(req.Query, "bucket"))),
	})
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to summarize usage"}))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

// usageRecordsGet serves raw-record drill-down one keyset page at a time. Pass
// the returned next_cursor back verbatim to continue; supply id to fetch a
// single record with its full, untruncated failure body.
func usageRecordsGet(req managementRequest) ([]byte, error) {
	rng, errResp := parseUsageRange(req.Query)
	if errResp != nil {
		return okEnvelope(*errResp)
	}
	q := RecordsQuery{
		Range:      rng,
		ID:         strings.TrimSpace(firstValue(req.Query, "id")),
		APIKey:     strings.TrimSpace(firstValue(req.Query, "api_key")),
		Provider:   strings.TrimSpace(firstValue(req.Query, "provider")),
		Model:      strings.TrimSpace(firstValue(req.Query, "model")),
		FailedOnly: isTruthy(firstValue(req.Query, "failed")),
		Limit:      parseLimit(firstValue(req.Query, "limit")),
	}
	if raw := strings.TrimSpace(firstValue(req.Query, "cursor")); raw != "" {
		afterTS, afterID, ok := decodeCursor(raw)
		if !ok {
			return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "invalid cursor"}))
		}
		q.AfterTS, q.AfterID = afterTS, afterID
	}
	store, release := acquireStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(200, RecordsPage{Records: []RequestDetail{}}))
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	page, err := store.Records(ctx, q)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to query usage records"}))
	}
	return okEnvelope(jsonManagementResponse(200, page))
}

func isTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// parseLimit returns 0 for anything unusable so the store applies its default.
func parseLimit(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func parseUsageRange(query map[string][]string) (QueryRange, *managementResponse) {
	var rng QueryRange
	if raw := strings.TrimSpace(firstValue(query, "start")); raw != "" {
		start, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			resp := jsonManagementResponse(400, map[string]string{"error": "invalid start"})
			return rng, &resp
		}
		start = start.UTC()
		rng.Start = &start
	}
	if raw := strings.TrimSpace(firstValue(query, "end")); raw != "" {
		end, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			resp := jsonManagementResponse(400, map[string]string{"error": "invalid end"})
			return rng, &resp
		}
		end = end.UTC()
		rng.End = &end
	}
	return rng, nil
}

func firstValue(values map[string][]string, key string) string {
	if len(values[key]) > 0 {
		return values[key][0]
	}
	return ""
}

func jsonManagementResponse(status int, payload any) managementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":"failed to encode response"}`)
		status = 500
	}
	return managementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
}

// ---- store lifecycle -------------------------------------------------------

var (
	storeMu       sync.RWMutex
	globalStore   *SQLiteStore
	currentDBPath string

	retentionDays atomic.Int64

	// cleanupMu guards the retention worker's lifecycle handles. cancel stops the
	// loop; done is closed by the loop on exit so shutdown can join it instead of
	// pulling the database out from under an in-flight prune.
	cleanupMu     sync.Mutex
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}
)

// acquireStore returns the live store together with a release func, holding the
// read lock for the whole database operation.
//
// Returning a bare pointer (the previous currentStore) let a caller keep using a
// store that ensureStore or closeStore had already closed, surfacing as
// "sql: database is closed" and a dropped usage record. Holding the read lock
// instead makes reconfigure and shutdown wait for active operations to finish.
// Callers MUST defer the returned release func.
func acquireStore() (*SQLiteStore, func()) {
	storeMu.RLock()
	if globalStore == nil {
		storeMu.RUnlock()
		return nil, func() {}
	}
	return globalStore, storeMu.RUnlock
}

func closeStore() {
	// Stop and join the retention worker BEFORE taking the write lock. The worker
	// takes the read lock, so cancelling while holding the write lock would
	// deadlock; cancelling first lets an in-flight prune unwind and release it.
	stopCleanupWorker()
	storeMu.Lock()
	defer storeMu.Unlock()
	if globalStore != nil {
		_ = globalStore.Close()
		globalStore = nil
		currentDBPath = ""
	}
}

func ensureStore(cfg pluginConfig) error {
	path, err := resolveDBPath(cfg)
	if err != nil {
		return err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if globalStore != nil && currentDBPath == path {
		return nil
	}
	store, err := NewSQLiteStore(path)
	if err != nil {
		return err
	}
	if globalStore != nil {
		_ = globalStore.Close()
	}
	globalStore = store
	currentDBPath = path
	return nil
}

// startCleanupWorker (re)starts retention pruning on a background ticker.
//
// Retention used to run inline on the ingestion path, which placed a bulk DELETE
// directly in front of a user's proxied request. On the single shared connection
// that meant a large prune could stall live traffic for seconds. Running it on
// its own goroutine makes ingestion latency independent of how much there is to
// prune.
//
// Restarting on every configure is deliberate: it re-arms the short initial
// delay so a newly enabled retention policy takes effect promptly instead of
// waiting out the remainder of an hourly tick.
func startCleanupWorker() {
	stopCleanupWorker()

	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cleanupCancel = cancel
	cleanupDone = done
	go cleanupLoop(ctx, done)
}

// stopCleanupWorker cancels the worker and waits for it to exit. Safe to call
// when no worker is running, and safe to call repeatedly.
func stopCleanupWorker() {
	cleanupMu.Lock()
	cancel, done := cleanupCancel, cleanupDone
	cleanupCancel, cleanupDone = nil, nil
	cleanupMu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func cleanupLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(cleanupInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runRetentionCleanup(ctx)
			timer.Reset(cleanupInterval)
		}
	}
}

func runRetentionCleanup(parent context.Context) {
	days := retentionDays.Load()
	if days <= 0 {
		return
	}
	store, release := acquireStore()
	if store == nil {
		return
	}
	defer release()
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(parent, cleanupTimeout)
	defer cancel()
	_, _ = store.DeleteBefore(ctx, cutoff)
}

// ---- config ----------------------------------------------------------------

type lifecycleRequest struct {
	ConfigYAML json.RawMessage `json:"config_yaml"`
}

func configurePlugin(request []byte) error {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return err
		}
	}
	raw, err := lifecycleConfigYAML(req.ConfigYAML)
	if err != nil {
		return err
	}
	cfg := parseConfig(raw)
	retentionDays.Store(int64(cfg.RetentionDays))
	if err := ensureStore(cfg); err != nil {
		return err
	}
	startCleanupWorker()
	return nil
}

// lifecycleConfigYAML accepts the host's config_yaml which may arrive as a
// base64 string, a plain YAML string, or a byte array.
func lifecycleConfigYAML(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if decoded, errDecode := base64.StdEncoding.DecodeString(text); errDecode == nil && strings.Contains(string(decoded), ":") {
			return decoded, nil
		}
		return []byte(text), nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		return bytes, nil
	}
	return nil, errors.New("config_yaml must be a string or byte array")
}
