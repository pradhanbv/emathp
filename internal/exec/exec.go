// Package exec runs a built plan against a connector.Source: fetch the
// over-projected scan, apply every locally-retained Filter, mask, then
// trim to the final output projection.
package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/internal/plan"
)

type Result struct {
	Columns []string
	Rows    []map[string]string
	Debug   Debug
}

type Debug struct {
	FetchedColumns []string
}

// Run executes p against source, resolving any $principal.<attr>
// predicate values against attrs at comparison time rather than having
// them baked into p.
//
// This matters once a plan can be cached and shared across principals
// (Cycle 7, ADR-003): p is not mutated here, and never should be - two
// principals in the same role produce the same cache key (tenant, role,
// policy version/shape, capability shape, sql shape all match) but can
// have different attribute values, so binding has to happen per call,
// against a read-only plan, never by writing into the shared Filter
// nodes.
func Run(ctx context.Context, p *plan.Plan, source connector.Source, attrs map[string]string) (*Result, error) {
	scan := p.PrimaryScan()
	if scan == nil {
		return nil, fmt.Errorf("exec: plan has no scan")
	}

	filters := p.Filters()

	pushed := make(map[string]string, len(filters))
	for _, f := range filters {
		if f.Site == plan.Pushed {
			v, err := resolveValue(f.Pred.Value, attrs)
			if err != nil {
				return nil, err
			}
			pushed[f.Pred.Column] = v
		}
	}

	rows, err := source.Fetch(ctx, connector.FetchRequest{
		Table:   scan.Table,
		Columns: scan.Project,
		Filters: pushed,
	})
	if err != nil {
		var colErr *connector.ColumnUnavailableError
		if errors.As(err, &colErr) && securityPredicateNeedsColumn(filters, colErr.Column) {
			return nil, fmt.Errorf("%w: source cannot supply column %q, required by tenant policy",
				plan.ErrEntitlementDenied, colErr.Column)
		}
		return nil, err
	}

	// The verification filter (ADR-002): the plan-time invariant only
	// catches a predicate that never made it into the plan. It can't
	// catch a connector that declared a predicate ENFORCED and then
	// ignored it - that plan is perfectly valid, the lie is only visible
	// in the rows that came back. Re-apply every PUSHED security
	// predicate locally; any row that fails it means the connector's
	// real behaviour diverged from its declared capability.
	if err := verifyPushedSecurityPredicates(rows, filters, attrs); err != nil {
		return nil, err
	}

	rows, err = applyLocalFilters(rows, filters, attrs)
	if err != nil {
		return nil, err
	}

	outCols := p.OutputColumns()
	outRows, err := project(rows, outCols)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(outCols))
	for i, c := range outCols {
		names[i] = c.Name
	}

	return &Result{
		Columns: names,
		Rows:    outRows,
		Debug:   Debug{FetchedColumns: scan.Project},
	}, nil
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
func verifyPushedSecurityPredicates(rows []connector.Row, filters []*plan.Filter, attrs map[string]string) error {
	for _, f := range filters {
		if f.Pred.Origin != plan.SecurityOrigin || f.Site != plan.Pushed {
			continue
		}
		want, err := resolveValue(f.Pred.Value, attrs)
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

func applyLocalFilters(rows []connector.Row, filters []*plan.Filter, attrs map[string]string) ([]connector.Row, error) {
	var out []connector.Row
	for _, r := range rows {
		keep := true
		for _, f := range filters {
			if f.Site != plan.Local {
				continue
			}
			want, err := resolveValue(f.Pred.Value, attrs)
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
// against a fetched row: a literal gets unquoted, a $principal.<attr>
// placeholder gets looked up in attrs. Missing attribute fails closed
// (PRINCIPAL_UNRESOLVED territory) rather than comparing against an empty
// string, which would silently match or silently drop every row
// depending on data - the same bug class over-projection exists to avoid.
func resolveValue(value string, attrs map[string]string) (string, error) {
	const prefix = "$principal."
	if attr, ok := strings.CutPrefix(value, prefix); ok {
		v, ok := attrs[attr]
		if !ok {
			return "", fmt.Errorf("%w: missing principal attribute %q", plan.ErrEntitlementDenied, attr)
		}
		return v, nil
	}
	return unquote(value), nil
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}
