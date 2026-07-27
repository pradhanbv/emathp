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
// for the source to apply itself.
type FetchRequest struct {
	Table   string
	Columns []string
	Filters map[string]string
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
	Fetch(ctx context.Context, req FetchRequest) ([]Row, error)
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
