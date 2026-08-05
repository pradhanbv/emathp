// Package exec runs a built plan against connector.Source(s): fetch the
// over-projected scan(s), apply every locally-retained Filter, join if the
// plan has one, mask, then trim to the final output projection.
package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/internal/plan"
)

type Result struct {
	Columns []string
	Rows    [][]string
	Debug   Debug

	// JoinStrategy, JoinEngine and NaiveCallEstimate are set only when the
	// plan had a Join (Cycle 10, ADR-007) - the pushdown-creativity evidence
	// the gateway reports back in the response envelope's Meta. JoinEngine
	// names which JoinEngine performed the merge, so a caller can tell the
	// Go and DuckDB paths apart from the response rather than from the flag
	// the operator happened to set.
	JoinStrategy      string
	JoinEngine        string
	NaiveCallEstimate int
}

type Debug struct {
	FetchedColumns []string
}

// SourceTimeoutError signals a connector fetch didn't complete within the
// request's timeout budget (Cycle 11, ADR-009). Distinct from a generic
// fetch error so a caller can attribute the failure to one connector -
// the NDJSON streaming path reports this as a partial result rather than
// failing the whole request opaquely.
type SourceTimeoutError struct {
	Connector string
}

func (e *SourceTimeoutError) Error() string {
	return fmt.Sprintf("exec: connector %q timed out", e.Connector)
}

func (e *SourceTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// Run executes p against sources (keyed by connector prefix, e.g. "sf"),
// resolving any $principal.<attr> predicate value against attrs and any
// ordinary WHERE-clause literal ($param.N) against params, both at
// comparison time rather than having them baked into p.
//
// This matters once a plan can be cached and shared across requests
// (Cycle 7, ADR-003): p is not mutated here, and never should be - two
// requests of the same shape (same tenant, role, policy version/shape,
// capability shape, sql shape) share the exact same cached Plan, whether
// they're different principals (different attrs) or the same principal
// running the query with different WHERE-clause literals (different
// params) - binding has to happen per call, against a read-only plan,
// never by writing into the shared Filter nodes. params comes from
// plan.ExtractParams(sql) run against the CURRENT request's own SQL text,
// never the plan's - see whereCond's doc comment in the plan package for
// the bug this indirection exists to prevent.
func Run(ctx context.Context, p *plan.Plan, sources map[string]connector.Source, attrs map[string]string, params []string, opts ...option) (*Result, error) {
	if join := p.PrimaryJoin(); join != nil {
		return runJoin(ctx, p, join, sources, attrs, params, newRunConfig(opts).engine)
	}

	scan := p.PrimaryScan()
	if scan == nil {
		return nil, fmt.Errorf("exec: plan has no scan")
	}

	source, err := sourceFor(sources, scan.Table)
	if err != nil {
		return nil, err
	}

	rows, err := fetchScanRows(ctx, source, scan, p.Filters(), attrs, params, nil)
	if err != nil {
		return nil, err
	}

	outCols := p.OutputColumns()
	outRows, err := project(rows, outCols)
	if err != nil {
		return nil, err
	}

	return &Result{
		Columns: colNames(outCols),
		Rows:    outRows,
		Debug:   Debug{FetchedColumns: scan.Project},
	}, nil
}

// runJoin is the semi-join cascade (ADR-007). Sides[0] is fetched in full;
// each later side is a probe whose join-key values come from rows already in
// hand, chunked by that side's catalog-declared IN-list capacity and pushed
// as a filter - so the probe returns only rows that can possibly match,
// instead of one call per key.
//
// The cascade is left-deep and follows FROM order. plan.buildJoin enforces
// that every link reaches backwards (Link.From < Link.To), which guarantees
// the keys for a probe are in hand when its turn comes. Choosing a better
// order is the optimisation ADR-007 defers.
func runJoin(ctx context.Context, p *plan.Plan, join *plan.Join, sources map[string]connector.Source, attrs map[string]string, params []string, engine JoinEngine) (*Result, error) {
	n := len(join.Sides)
	fetched := make([][]connector.Row, n)
	inputs := make([]JoinInput, n)
	var fetchedCols []string
	naive := 0

	for i, side := range join.Sides {
		scan := plan.ScanIn(side.Root)
		if scan == nil {
			return nil, fmt.Errorf("exec: join side %q has no scan", side.Alias)
		}
		src, err := sourceFor(sources, scan.Table)
		if err != nil {
			return nil, err
		}
		filters := plan.FiltersIn(side.Root)

		if i == 0 {
			rows, err := fetchScanRows(ctx, src, scan, filters, attrs, params, nil)
			if err != nil {
				return nil, err
			}
			fetched[i] = rows
		} else {
			var link *plan.Link
			for k := range join.Links {
				if join.Links[k].To == i {
					link = &join.Links[k]
					break
				}
			}
			if link == nil {
				return nil, fmt.Errorf("exec: join side %q has no link", side.Alias)
			}
			keys := distinctValues(fetched[link.From], link.FromCol)
			naive += len(keys)
			size := link.MaxInList
			if size <= 0 {
				size = len(keys)
			}
			chunks := chunkStrings(keys, size)
			var rows []connector.Row
			for _, chunk := range chunks {
				got, err := fetchScanRows(ctx, src, scan, filters, attrs, params,
					map[string][]string{link.ToCol: chunk})
				if err != nil {
					return nil, err
				}
				rows = append(rows, got...)
			}
			fetched[i] = rows
			logSemiJoin(scan.Table, len(fetched[link.From]), keys, chunks, len(rows))
		}
		inputs[i] = JoinInput{Alias: side.Alias, Rows: fetched[i]}
		fetchedCols = append(fetchedCols, scan.Project...)
	}

	joined, err := engine.Join(ctx, inputs, join.Links)
	if err != nil {
		return nil, err
	}

	outCols := p.OutputColumns()
	outRows, err := project(joined, outCols)
	if err != nil {
		return nil, err
	}

	return &Result{
		Columns:           colNames(outCols),
		Rows:              outRows,
		Debug:             Debug{FetchedColumns: fetchedCols},
		JoinStrategy:      "semi_join",
		JoinEngine:        engine.Name(),
		NaiveCallEstimate: naive,
	}, nil
}

// fetchScanRows fetches scan's rows from source - filters' pushed
// predicates plus any extra filter (a semi-join chunk's IN-list) - then
// re-verifies every pushed security predicate and applies every
// locally-retained one. Shared by the single-scan path and each side of a
// join: the fail-closed sequence doesn't change depending on whether this
// is the query's only scan.
func fetchScanRows(ctx context.Context, source connector.Source, scan *plan.Scan, filters []*plan.Filter, attrs map[string]string, params []string, extra map[string][]string) ([]connector.Row, error) {
	pushed := make(map[string][]string, len(filters)+len(extra))
	for _, f := range filters {
		if f.Site == plan.Pushed {
			v, err := resolveValue(f.Pred.Value, attrs, params)
			if err != nil {
				return nil, err
			}
			pushed[f.Pred.Column] = []string{v}
		}
	}
	for col, vals := range extra {
		pushed[col] = vals
	}

	rows, _, err := source.Fetch(ctx, connector.FetchRequest{
		Table:   scan.Table,
		Columns: scan.Project,
		Filters: pushed,
	})
	if err != nil {
		// ADR-009: a connector fetch that outlives the request's timeout
		// budget is a distinct failure mode from a generic connector error
		// - attributed to the specific connector so a caller (the NDJSON
		// streaming path) can report a partial result rather than failing
		// the whole request opaquely.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &SourceTimeoutError{Connector: connectorPrefix(scan.Table)}
		}
		var colErr *connector.ColumnUnavailableError
		if errors.As(err, &colErr) && securityPredicateNeedsColumn(filters, colErr.Column) {
			return nil, fmt.Errorf("%w: source cannot supply column %q, required by tenant policy",
				plan.ErrEntitlementDenied, colErr.Column)
		}
		return nil, err
	}

	// The verification filter (ADR-002): the plan-time invariant only
	// catches a predicate that never made it into the plan. It can't catch
	// a connector that declared a predicate ENFORCED and then ignored it -
	// that plan is perfectly valid, the lie is only visible in the rows
	// that came back. Re-apply every PUSHED security predicate locally;
	// any row that fails it means the connector's real behaviour diverged
	// from its declared capability.
	if err := verifyPushedSecurityPredicates(rows, filters, attrs, params); err != nil {
		return nil, err
	}

	return applyLocalFilters(rows, filters, attrs, params)
}

func sourceFor(sources map[string]connector.Source, table string) (connector.Source, error) {
	name := connectorPrefix(table)
	src, ok := sources[name]
	if !ok {
		return nil, fmt.Errorf("exec: no connector configured for %q", table)
	}
	return src, nil
}

func connectorPrefix(table string) string {
	if i := strings.Index(table, "."); i >= 0 {
		return table[:i]
	}
	return table
}

func colNames(cols []plan.ProjectCol) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

// distinctValues returns col's non-empty values from rows, deduplicated
// and sorted - the build side's join keys before chunking.
//
// Sorted, not first-seen, because chunkStrings slices this list into
// fixed-size groups and each group becomes part of a probe-side result
// cache key (freshness.cacheKey folds the bound filter values in). That
// key sorts values *within* a chunk, which canonicalizes how a chunk is
// written but not which keys land in which chunk. First-seen order is the
// build side's row order, and a SaaS list endpoint guarantees no ordering
// without an explicit sort - so the same probe data, reached through a
// build side that came back in a different order, produced entirely
// different cache keys and missed every probe-side entry. Sorting makes
// chunk membership a function of the key set alone.
//
// It does not make chunking stable under insertion: add one build row and
// every boundary after it still shifts. Content-defined boundaries would
// fix that too, and are not worth it here.
func distinctValues(rows []connector.Row, col string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, r := range rows {
		v := r[col]
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// chunkStrings splits vals into groups of at most size - the probe side's
// catalog-declared IN-list capacity, so no single fetch exceeds what the
// connector accepts.
func chunkStrings(vals []string, size int) [][]string {
	if size <= 0 || len(vals) == 0 {
		return nil
	}
	var chunks [][]string
	for i := 0; i < len(vals); i += size {
		end := min(i+size, len(vals))
		chunks = append(chunks, vals[i:end])
	}
	return chunks
}

// logSemiJoin emits the pushdown-creativity evidence line
// (IMPLEMENTATION_PLAN.md Cycle 10): one legible line showing the naive
// baseline (one probe call per build-side key) against what chunking
// actually cost. reduction_total is total-to-total, not probe-to-total -
// the build side is a fixed cost paid in both plans, which is what dilutes
// the probe side's own reduction once folded into the total. selectivity
// here is chunks/keys (the fraction of the naive one-call-per-key baseline
// chunking still required), not the classic matched-rows/corpus-size
// ratio - exec never learns the probe table's total corpus size, only
// what came back for the keys it asked about.
func logSemiJoin(probeTable string, buildRows int, keys []string, chunks [][]string, probeRowsMatched int) {
	totalCalls := 1 + len(chunks)
	totalCallsNaive := 1 + len(keys)
	reduction := float64(totalCallsNaive) / float64(totalCalls)
	selectivity := 0.0
	if len(keys) > 0 {
		selectivity = float64(len(chunks)) / float64(len(keys))
	}
	log.Printf("join.strategy=semi_join probe=%s build_rows=%d keys=%d chunks=%d "+
		"probe_calls=%d probe_calls_naive=%d probe_rows_matched=%d total_calls=%d total_calls_naive=%d "+
		"selectivity=%.3f reduction_total=%.1fx",
		probeTable, buildRows, len(keys), len(chunks),
		len(chunks), len(keys), probeRowsMatched, totalCalls, totalCallsNaive,
		selectivity, reduction)
}

// sideKey namespaces a join row's column by the side it came from. Single
// -table plans never call it - their ProjectCol.Side is empty and project
// reads the bare name.
func sideKey(side, col string) string { return side + "\x00" + col }

// securityPredicateNeedsColumn is the fail-closed check from
// TestMissingPredicateColumnFailsClosed: a source hiding a column a user
// predicate depends on is a different problem (UNSUPPORTED_PREDICATE
// territory), but hiding a column an RLS rule depends on is an
// entitlement failure - we can no longer prove the policy holds, so we
// refuse rather than serve rows we can't verify are properly filtered.
func securityPredicateNeedsColumn(filters []*plan.Filter, column string) bool {
	for _, f := range filters {
		if f.Pred.Origin == plan.SecurityOrigin && f.Pred.Column == column {
			return true
		}
	}
	return false
}

// verifyPushedSecurityPredicates re-checks every security predicate that
// was pushed to the connector (meaning the catalog declared it ENFORCED).
// A trustworthy connector drops zero rows on this re-check; any row that
// fails it means the predicate "appears pushed, does nothing" - the
// realistic failure isn't vendor dishonesty, it's our own connector
// sending the wrong query param and the source silently ignoring it.
func verifyPushedSecurityPredicates(rows []connector.Row, filters []*plan.Filter, attrs map[string]string, params []string) error {
	for _, f := range filters {
		if f.Pred.Origin != plan.SecurityOrigin || f.Site != plan.Pushed {
			continue
		}
		want, err := resolveValue(f.Pred.Value, attrs, params)
		if err != nil {
			return err
		}
		violations := 0
		for _, r := range rows {
			if r[f.Pred.Column] != want {
				violations++
			}
		}
		if violations > 0 {
			obs.EnforcedPredicateViolations.Add(int64(violations))
			return fmt.Errorf("%w: connector declared %q enforced but returned %d row(s) violating it",
				plan.ErrEntitlementDenied, f.Pred.Column, violations)
		}
	}
	return nil
}

func applyLocalFilters(rows []connector.Row, filters []*plan.Filter, attrs map[string]string, params []string) ([]connector.Row, error) {
	var out []connector.Row
	for _, r := range rows {
		keep := true
		for _, f := range filters {
			if f.Site != plan.Local {
				continue
			}
			want, err := resolveValue(f.Pred.Value, attrs, params)
			if err != nil {
				return nil, err
			}
			if r[f.Pred.Column] != want {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out, nil
}

// project trims each row to the output columns, in projection order, and
// applies any mask. Rows are positional - a []string parallel to Columns -
// not a map keyed by column name: a join can legitimately project the same
// name from both sides (SELECT a.id, t.id), which a map cannot represent at
// all, since the second entry overwrites the first. Position carries the
// identity instead, which is also the shape the response envelope already
// uses, so nothing has to be flattened later.
func project(rows []connector.Row, cols []plan.ProjectCol) ([][]string, error) {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		vals := make([]string, len(cols))
		for i, c := range cols {
			name := c.Name
			if c.Side != "" {
				name = sideKey(c.Side, c.Name)
			}
			v := r[name]
			if c.Mask != nil {
				masked, err := applyMask(*c.Mask, v)
				if err != nil {
					return nil, err
				}
				v = masked
			}
			vals[i] = v
		}
		out = append(out, vals)
	}
	return out, nil
}

func applyMask(fn, value string) (string, error) {
	switch fn {
	case "sha256":
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:]), nil
	default:
		return "", fmt.Errorf("exec: unsupported mask function %q", fn)
	}
}

// resolveValue turns a predicate value into a concrete string to compare
// against a fetched row: a $principal.<attr> placeholder gets looked up in
// attrs, a $param.N placeholder gets looked up in params (the CURRENT
// request's own WHERE-clause literals, re-extracted fresh by
// plan.ExtractParams rather than read from the plan - see whereCond's doc
// comment in the plan package), anything else is a literal and gets
// unquoted directly. Missing attribute/param fails closed
// (PRINCIPAL_UNRESOLVED / UNSUPPORTED_PREDICATE territory) rather than
// comparing against an empty string, which would silently match or
// silently drop every row depending on data - the same bug class
// over-projection exists to avoid.
// principalPrefix has no cross-package counterpart to share (nothing else
// writes it), so it stays local, unlike plan.ParamPrefix below.
const principalPrefix = "$principal."

func resolveValue(value string, attrs map[string]string, params []string) (string, error) {
	if attr, ok := strings.CutPrefix(value, principalPrefix); ok {
		v, ok := attrs[attr]
		if !ok {
			return "", fmt.Errorf("%w: missing principal attribute %q", plan.ErrEntitlementDenied, attr)
		}
		return v, nil
	}
	if idxStr, ok := strings.CutPrefix(value, plan.ParamPrefix); ok {
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return "", fmt.Errorf("exec: malformed param placeholder %q", value)
		}
		if idx < 0 || idx >= len(params) {
			return "", fmt.Errorf("exec: param %d out of range (query has %d parameter(s)) - the current request's SQL no longer matches the shape its cached plan was built from", idx, len(params))
		}
		return unquote(params[idx]), nil
	}
	return unquote(value), nil
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}
