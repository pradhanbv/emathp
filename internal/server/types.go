package server

// QueryRequest is the /v1/query request body.
type QueryRequest struct {
	SQL          string `json:"sql"`
	MaxStaleness string `json:"max_staleness,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
}

// QueryResponse is the /v1/query response envelope. Every field here is
// locked in by TestQueryEnvelope (cycle 0) before any real logic exists;
// later cycles fill it in rather than reshaping it.
type QueryResponse struct {
	Columns         []string          `json:"columns"`
	Rows            [][]any           `json:"rows"`
	FreshnessMS     *int64            `json:"freshness_ms"`
	RateLimitStatus map[string]string `json:"rate_limit_status"`
	TraceID         string            `json:"trace_id"`
	Meta            *Meta             `json:"meta,omitempty"`
	Error           *ErrorBody        `json:"error,omitempty"`
	PollURL         string            `json:"poll_url,omitempty"`
}

// JobStatus is the /v1/jobs/{id} poll response for an async query
// (Prefer: respond-async). Async here is an in-memory map, not a real job
// queue - the rubric point is the reroute path existing.
type JobStatus struct {
	Done   bool           `json:"done"`
	Result *QueryResponse `json:"result,omitempty"`
}

type Meta struct {
	CacheHit          bool   `json:"cache_hit,omitempty"`
	Revalidated       bool   `json:"revalidated,omitempty"`
	JoinStrategy      string `json:"join_strategy,omitempty"`
	NaiveCallEstimate int    `json:"naive_call_estimate,omitempty"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Frame is one line of the NDJSON stream `Accept: application/x-ndjson`
// requests (Cycle 11, ADR-009). v1 emits exactly one frame per request -
// the terminal one - rather than incremental per-row frames; the rubric
// point is the timeout-to-partial-result path existing, not genuine
// row-by-row streaming (see IMPLEMENTATION_PLAN.md Cycle 11).
type Frame struct {
	IsTerminal      bool                    `json:"is_terminal,omitempty"`
	Partial         bool                    `json:"partial,omitempty"`
	TraceID         string                  `json:"trace_id,omitempty"`
	Columns         []string                `json:"columns,omitempty"`
	Rows            [][]any                 `json:"rows,omitempty"`
	FreshnessMS     *int64                  `json:"freshness_ms"`
	RateLimitStatus map[string]string       `json:"rate_limit_status"`
	Sources         map[string]SourceStatus `json:"sources,omitempty"`
	Error           *ErrorBody              `json:"error,omitempty"`
}

// SourceStatus reports one connector's outcome within a partial Frame -
// currently only populated for a timeout, the one failure mode this cycle
// attributes to a specific connector.
type SourceStatus struct {
	Error string `json:"error,omitempty"`
}
