package exec_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/exec"
	"github.com/pradhanbv/emathp/internal/plan"
	"github.com/pradhanbv/emathp/internal/policy"
)

// personaRole/personaAttrs mirror Cycle 3's buildFor pattern: short
// persona names (matching harness.Token) mapped to the policy role and
// the principal attributes identity.Resolve would have produced.
var personaRole = map[string]string{"support": "support_agent"}
var personaAttrs = map[string]map[string]string{
	"support": {"region": "EMEA"},
}

func defaultRows() []connector.Row {
	return []connector.Row{
		{"id": "a001", "email": "dana@acme-corp.example", "region": "EMEA", "status": "open"},
		{"id": "a002", "email": "someone@other.example", "region": "APAC", "status": "open"},
	}
}

// fakeSource is a minimal connector.Source test double, honest by
// construction (lying is Cycle 6's own test). The real HTTP-backed mock
// (pagination, ETag, 429, --lie-about) is the connector SDK cycle's
// deliverable; this proves exec's logic against the same Source interface
// that mock will later implement.
type fakeSource struct {
	rows          []connector.Row
	hiddenColumns map[string]bool
}

func newFakeSource(rows []connector.Row) *fakeSource {
	return &fakeSource{rows: rows, hiddenColumns: map[string]bool{}}
}

func (s *fakeSource) Fetch(_ context.Context, req connector.FetchRequest) ([]connector.Row, error) {
	for _, col := range req.Columns {
		if s.hiddenColumns[col] {
			return nil, &connector.ColumnUnavailableError{Column: col}
		}
	}

	var out []connector.Row
	for _, r := range s.rows {
		keep := true
		for col, want := range req.Filters {
			if r[col] != want {
				keep = false
				break
			}
		}
		if !keep {
			continue
		}
		row := connector.Row{}
		for _, col := range req.Columns {
			row[col] = r[col]
		}
		out = append(out, row)
	}
	return out, nil
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// enforcedRegionCatalog is a temp-dir catalog fixture where region is
// ENFORCED, mirroring the pattern from plan_test.go - needed to prove
// over-projection applies to a pushed predicate too, not just a residual
// one, without waiting on the real mocksf's Capability() knob (Cycle 5).
func enforcedRegionCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	fixture := `{
		"table": "sf.accounts",
		"predicates": { "region": { "ops": ["="], "enforcement": "ENFORCED" } }
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sf.accounts.json"), []byte(fixture), 0o644))
	cat, err := catalog.Load(dir)
	require.NoError(t, err)
	return cat
}

// queryResult mimics the HTTP envelope shape (Code, Error.Code) without
// wiring the gateway handler this cycle - that lands once a real
// connector exists (Cycle 5/6) and identity is wired end-to-end.
type queryResult struct {
	Code           int
	Columns        []string
	Rows           []map[string]string
	ErrorCode      string
	FetchedColumns []string
}

func runQuery(t *testing.T, persona, sql string, cat *catalog.Catalog, source connector.Source) queryResult {
	t.Helper()

	pol, err := policy.Load("../../testdata/policy")
	require.NoError(t, err)

	table, err := plan.ParseTable(sql)
	require.NoError(t, err)

	role := personaRole[persona]

	// residuals stay unbound ($principal.region, not 'EMEA') - Build
	// produces a plan safe to share across principals in the same role.
	// exec.Run resolves attrs at comparison time instead (Cycle 7,
	// ADR-003: binding happens after a plan-cache lookup, not before).
	residuals, err := pol.ResidualsFor(role, table)
	require.NoError(t, err)

	masks, err := pol.MasksFor(role, table)
	require.NoError(t, err)

	p, err := plan.Build(sql, cat, residuals, masks)
	require.NoError(t, err)

	result, err := exec.Run(context.Background(), p, source, personaAttrs[persona])
	if err != nil {
		if errors.Is(err, plan.ErrEntitlementDenied) {
			return queryResult{Code: 403, ErrorCode: "ENTITLEMENT_DENIED"}
		}
		t.Fatalf("exec.Run: %v", err)
	}

	return queryResult{
		Code:           200,
		Columns:        result.Columns,
		Rows:           result.Rows,
		FetchedColumns: result.Debug.FetchedColumns,
	}
}

func defaultCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load("../../testdata/catalog")
	require.NoError(t, err)
	return cat
}

// TestCLSMaskApplied proves CLS masking runs on the way out: email is
// hashed, never returned raw.
func TestCLSMaskApplied(t *testing.T) {
	res := runQuery(t, "support", "SELECT email FROM sf.accounts", defaultCatalog(t), newFakeSource(defaultRows()))

	require.Equal(t, sha256hex("dana@acme-corp.example"), res.Rows[0]["email"])
}

// TestResidualColumnStripped: region is fetched to satisfy the local RLS
// filter, then trimmed from the output the user actually asked for.
func TestResidualColumnStripped(t *testing.T) {
	res := runQuery(t, "support", "SELECT id FROM sf.accounts", defaultCatalog(t), newFakeSource(defaultRows()))

	require.NotContains(t, res.Columns, "region")
	require.Contains(t, res.FetchedColumns, "region")
}

// TestOverProjectionAppliesToEnforcedToo: region is ENFORCED here (pushed,
// not local), but the verification filter (Cycle 6) will still need to
// re-check it locally after fetch, so it must be fetched even though it's
// never in the output.
func TestOverProjectionAppliesToEnforcedToo(t *testing.T) {
	res := runQuery(t, "support", "SELECT id FROM sf.accounts", enforcedRegionCatalog(t), newFakeSource(defaultRows()))

	require.Contains(t, res.FetchedColumns, "region")
	require.NotContains(t, res.Columns, "region")
}

// TestMissingPredicateColumnFailsClosed: the source hides the column the
// RLS rule depends on (e.g. Salesforce field-level security). Not: 200
// with zero rows - a distinguishable, actionable failure.
func TestMissingPredicateColumnFailsClosed(t *testing.T) {
	source := newFakeSource(defaultRows())
	source.hiddenColumns["region"] = true

	res := runQuery(t, "support", "SELECT id FROM sf.accounts", defaultCatalog(t), source)

	require.Equal(t, 403, res.Code)
	require.Equal(t, "ENTITLEMENT_DENIED", res.ErrorCode)
}
