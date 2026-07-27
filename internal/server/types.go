package server

// QueryRequest is the /v1/query request body.
type QueryRequest struct {
	SQL          string `json:"sql"`
	MaxStaleness string `json:"max_staleness,omitempty"`
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
