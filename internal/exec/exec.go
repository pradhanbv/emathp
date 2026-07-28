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
	"strconv"
	"strings"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/internal/plan"
)

type Result struct {
	Columns []string
	Rows    []map[string]string
	Debug   Debug

	// JoinStrategy and NaiveCallEstimate are set only when the plan had a
	// Join (Cycle 10, ADR-007) - the pushdown-creativity evidence the
	// gateway reports back in the response envelope's Meta.
	JoinStrategy      string
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
func Run(ctx context.Context, p *plan.Plan, sources map[string]connector.Source, attrs map[string]string, params []string) (*Result, error) {
	if join := p.PrimaryJoin(); join != nil {
		return runJoin(ctx, p, join, sources, attrs, params)
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

// runJoin is the semi-join rewrite (ADR-007): fetch the build side (Left)
// in full, chunk its distinct join-key values by the probe side's
// catalog-declared IN-list capacity, and fetch the probe side (Right) one
// chunk at a time instead of once per build row. The two fetched row sets
// are then joined in memory - pushdown reduced which probe rows came back,
// this just pairs what did.
func runJoin(ctx context.Context, p *plan.Plan, join *plan.Join, sources map[string]connector.Source, attrs map[string]string, params []string) (*Result, error) {
	buildScan := plan.ScanIn(join.Left)
	probeScan := plan.ScanIn(join.Right)
	if buildScan == nil || probeScan == nil {
		return nil, fmt.Errorf("exec: join side has no scan")
	}

	buildSource, err := sourceFor(sources, buildScan.Table)
	if err != nil {
		return nil, err
	}
	probeSource, err := sourceFor(sources, probeScan.Table)
	if err != nil {
		return nil, err
	}

	buildRows, err := fetchScanRows(ctx, buildSource, buildScan, plan.FiltersIn(join.Left), attrs, params, nil)
	if err != nil {
		return nil, err
	}

	keys := distinctValues(buildRows, join.On.LeftCol)
	naiveCallEstimate := len(keys) // one call per build-side key, unbatched

	chunkSize := join.MaxInList
	if chunkSize <= 0 {
		chunkSize = len(keys) // catalog declares no cap: one chunk
	}

	chunks := chunkStrings(keys, chunkSize)
	probeFilters := plan.FiltersIn(join.Right)
	var probeRows []connector.Row
	for _, chunk := range chunks {
		rows, err := fetchScanRows(ctx, probeSource, probeScan, probeFilters, attrs, params,
			map[string][]string{join.On.RightCol: chunk})
		if err != nil {
			return nil, err
		}
		probeRows = append(probeRows, rows...)
	}

	joined := hashJoin(buildRows, probeRows, join.On)

	logSemiJoin(buildScan.Table, len(buildRows), keys, chunks, len(probeRows))

	outCols := p.OutputColumns()
	outRows, err := project(joined, outCols)
	if err != nil {
		return nil, err
	}

	return &Result{
		Columns:           colNames(outCols),
		Rows:              outRows,
		Debug:             Debug{FetchedColumns: append(append([]string{}, buildScan.Project...), probeScan.Project...)},
		JoinStrategy:      "semi_join",
		NaiveCallEstimate: naiveCallEstimate,
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

// distinctValues returns col's non-empty values from rows, deduplicated,
// in first-seen order - the build side's join keys before chunking.
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
func logSemiJoin(buildTable string, buildRows int, keys []string, chunks [][]string, probeRowsMatched int) {
	totalCalls := 1 + len(chunks)
	totalCallsNaive := 1 + len(keys)
	reduction := float64(totalCallsNaive) / float64(totalCalls)
	selectivity := 0.0
	if len(keys) > 0 {
		selectivity = float64(len(chunks)) / float64(len(keys))
	}
	log.Printf("join.strategy=semi_join build=%s build_rows=%d keys=%d chunks=%d "+
		"probe_calls=%d probe_calls_naive=%d probe_rows_matched=%d total_calls=%d total_calls_naive=%d "+
		"selectivity=%.3f reduction_total=%.1fx",
		buildTable, buildRows, len(keys), len(chunks),
		len(chunks), len(keys), probeRowsMatched, totalCalls, totalCallsNaive,
		selectivity, reduction)
}

// hashJoin matches build and probe rows on their equi-join keys, merging
// each matched pair's fields into one row. Build and probe columns are
// merged into a single un-namespaced map, so a same-named column on both
// sides would collide - not reachable in v1, since every join's output
// columns are alias-qualified at plan time and no fixture gives both
// tables a predicate/mask/key column of the same name that also appears
// in the output.
func hashJoin(build, probe []connector.Row, on plan.Equi) []connector.Row {
	byKey := make(map[string][]connector.Row)
	for _, r := range build {
		k := r[on.LeftCol]
		byKey[k] = append(byKey[k], r)
	}

	var out []connector.Row
	for _, pr := range probe {
		k := pr[on.RightCol]
		for _, br := range byKey[k] {
			merged := make(connector.Row, len(br)+len(pr))
			for c, v := range br {
				merged[c] = v
			}
			for c, v := range pr {
				merged[c] = v
			}
			out = append(out, merged)
		}
	}
	return out
}

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

func project(rows []connector.Row, cols []plan.ProjectCol) ([]map[string]string, error) {
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		o := make(map[string]string, len(cols))
		for _, c := range cols {
			v := r[c.Name]
			if c.Mask != nil {
				masked, err := applyMask(*c.Mask, v)
				if err != nil {
					return nil, err
				}
				v = masked
			}
			o[c.Name] = v
		}
		out = append(out, o)
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
