// Package connector defines the seam between exec and a data source. The
// HTTP-backed Salesforce/Zendesk implementations (pagination, ETag, rate
// limits) are the connector SDK cycle's own deliverable; this interface
// lets exec's logic be proven against a fake now and a real mock server
// later without exec changing at all.
package connector

import (
	"context"
	"fmt"
)

// Row is one fetched record. Values stay string-typed in v1 - no
// numeric/date coercion needed for the MVP SQL surface.
type Row map[string]string

// FetchRequest is what exec asks a Source for: Columns is the
// over-projected required-columns set computed at plan time (Scan.Project
// - see plan.Build), and Filters is every predicate that was pushed down
// for the source to apply itself. A single-value equality filter is a
// one-element slice; the semi-join rewrite (Cycle 10, ADR-007) needs the
// same field to carry a build-side key chunk as an IN-list, which is why
// this is map[string][]string rather than map[string]string - one type for
// both shapes rather than a second field only one join side ever uses.
// ETag is optional: set it to make the request conditional
// (If-None-Match), which the freshness cache (Cycle 9, ADR-005) uses to
// revalidate a stale entry without re-fetching unchanged data.
type FetchRequest struct {
	Table   string
	Columns []string
	Filters map[string][]string
	ETag    string
}

// FetchMeta carries the fetch's caching-relevant metadata back to the
// caller: the ETag a subsequent request could use for conditional fetch,
// and whether this response was a 304 (ETag matched, no new data - Rows is
// empty in that case; the caller already has the current data).
type FetchMeta struct {
	ETag        string
	NotModified bool
}

// Source fetches rows for one table, filtered and projected as asked. One
// Source is one connector (e.g. "the Salesforce connection"), not one
// table - multiple tables from the same source is already handled by
// FetchRequest.Table (two Fetch calls to the same Source), not a gap or a
// deviation from anything documented.
//
// What this interface does not do: express join pushdown. A same-source
// join (e.g. sf.accounts JOIN sf.opportunities) gets no special treatment
// here - it's still two independent Fetch calls, joined in the gateway
// like a cross-connector join would be. See DESIGN.md ADR-007 for why
// (real, e.g. SOQL relationship subqueries, but rejected as scope).
type Source interface {
	Fetch(ctx context.Context, req FetchRequest) ([]Row, FetchMeta, error)
}

// ColumnUnavailableError signals a source cannot supply Column at all -
// e.g. Salesforce field-level security hiding it from this principal's
// grant. exec treats this as fail-closed when the column belongs to a
// security predicate.
type ColumnUnavailableError struct {
	Column string
}

func (e *ColumnUnavailableError) Error() string {
	return fmt.Sprintf("connector: column %q unavailable", e.Column)
}
