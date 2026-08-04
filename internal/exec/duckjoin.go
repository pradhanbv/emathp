//go:build duckdb

// Package build tag: DuckDB is reached through cgo, so importing it forces
// CGO_ENABLED=1 and a libc-bearing runtime image. The default build stays
// pure Go - `CGO_ENABLED=0` into distroless/static, as Dockerfile does - and
// this file compiles only under `-tags duckdb`. That keeps the cgo cost an
// opt-in for whoever wants tier 1's real engine, rather than a tax on every
// build of the gateway.

package exec

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sort"
	"strings"

	"github.com/marcboeker/go-duckdb"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/plan"
)

// DuckJoin is ADR-007 tier 1: the merge runs in an embedded DuckDB rather
// than the hand-rolled hash join.
//
// Three properties the design depends on, each costing a line here:
//
//   - One instance per query. `memory_limit` binds a database instance's
//     buffer manager, not a connection, so a shared instance would give K
//     concurrent joins one pool to divide rather than K independent
//     ceilings - and DESIGN.md Section 6.2's `K x 256 MB` presumes the
//     latter. This opens and closes a fresh in-memory database per Join.
//   - `threads` capped low. DuckDB parallelises within a query; K concurrent
//     joins each claiming a core oversubscribes the pod (Section 6.3 sizes
//     memory but not CPU). The vectorised execution model still wins
//     single-threaded.
//   - Everything is VARCHAR. connector.Row is map[string]string, so no type
//     information survives the connector boundary to reconstruct. This is a
//     real limitation, not a simplification: cross-source joins on a numeric
//     key would compare "01" and "1" as different. Fixing it needs the
//     schema registry the Afterthought describes, not a change here.
//
// cgo is the cost. Every call blocks an OS thread for the merge's duration,
// and DuckDB's buffers live outside the Go heap where GOGC cannot see them
// (Section 6.3). That is why this is opt-in rather than the default.
type DuckJoin struct {
	// MemoryLimit is passed to DuckDB verbatim, e.g. "256MB". Empty leaves
	// DuckDB's own default in place.
	MemoryLimit string
	// Threads caps intra-query parallelism. Zero means 1.
	Threads int
}

func (DuckJoin) Name() string { return "duckdb" }

func (d DuckJoin) Join(ctx context.Context, sides []JoinInput, links []plan.Link) ([]connector.Row, error) {
	if len(sides) == 0 {
		return nil, nil
	}
	for _, s := range sides {
		if len(s.Rows) == 0 {
			return nil, nil // an inner join with an empty side has no output
		}
	}

	// NewConnector rather than sql.Open: the appender binds to a raw
	// driver.Conn, which only a Connector-backed *sql.DB can hand back. An
	// empty DSN is an in-memory database, created and destroyed per call -
	// which is what makes memory_limit a per-join ceiling rather than a pool
	// K concurrent joins divide.
	c, err := duckdb.NewConnector("", nil)
	if err != nil {
		return nil, fmt.Errorf("exec: duckdb connector: %w", err)
	}
	defer c.Close()
	db := sql.OpenDB(c)
	defer db.Close()

	// Pin one connection for the whole join: CREATE TABLE, the appends and
	// the query all have to see the same in-memory database.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("exec: duckdb conn: %w", err)
	}
	defer conn.Close()

	threads := d.Threads
	if threads <= 0 {
		threads = 1
	}
	pragmas := []string{fmt.Sprintf("SET threads=%d", threads)}
	if d.MemoryLimit != "" {
		pragmas = append(pragmas, fmt.Sprintf("SET memory_limit='%s'", d.MemoryLimit))
	}
	for _, p := range pragmas {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			return nil, fmt.Errorf("exec: duckdb %q: %w", p, err)
		}
	}

	// One table per side, named t0..tN-1 rather than by alias: an alias is
	// user-controlled and would otherwise reach DuckDB as an identifier.
	cols := make([][]string, len(sides))
	sel := make([]string, 0, 16)
	out := make([]string, 0, 16)
	for i, side := range sides {
		cols[i] = columnsOf(side.Rows)
		tbl := fmt.Sprintf("t%d", i)
		if err := loadSide(ctx, conn, tbl, cols[i], side.Rows); err != nil {
			return nil, err
		}
		for _, c := range cols[i] {
			sel = append(sel, fmt.Sprintf("%s.%s", tbl, quoteIdent(c)))
			out = append(out, sideKey(side.Alias, c))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s FROM t0", strings.Join(sel, ", "))
	for _, l := range links {
		if l.From >= len(sides) || l.To >= len(sides) {
			return nil, fmt.Errorf("exec: duckdb join: link out of range")
		}
		if !contains(cols[l.From], l.FromCol) || !contains(cols[l.To], l.ToCol) {
			return nil, fmt.Errorf("exec: duckdb join: key column missing from a fetched side")
		}
		fmt.Fprintf(&b, " JOIN t%d ON t%d.%s = t%d.%s",
			l.To, l.To, quoteIdent(l.ToCol), l.From, quoteIdent(l.FromCol))
	}

	rows, err := conn.QueryContext(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("exec: duckdb join: %w", err)
	}
	defer rows.Close()

	var merged []connector.Row
	scan := make([]any, len(out))
	holders := make([]sql.NullString, len(out))
	for i := range holders {
		scan[i] = &holders[i]
	}
	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			return nil, fmt.Errorf("exec: duckdb scan: %w", err)
		}
		r := make(connector.Row, len(out))
		for i, name := range out {
			r[name] = holders[i].String
		}
		merged = append(merged, r)
	}
	return merged, rows.Err()
}

// loadSide creates an all-VARCHAR table and bulk-loads it through DuckDB's
// appender.
//
// The appender writes directly into DuckDB's internal data chunks and
// flushes them in vector-sized blocks, so the cgo boundary is crossed per
// chunk rather than per row or per statement. An earlier version issued
// batched multi-row INSERTs, which reached the same asymptotics but paid SQL
// parsing and placeholder marshalling on every batch - and ADR-007 described
// an ingestion path the code did not have.
//
// The Arrow interface would be the other option, but ingesting through it
// needs an array.RecordReader, so building one from map[string]string rows
// means taking on apache/arrow-go to describe data that is entirely VARCHAR.
// The appender is DuckDB's native bulk path and costs no extra dependency.
func loadSide(ctx context.Context, conn *sql.Conn, table string, cols []string, rows []connector.Row) error {
	defs := make([]string, len(cols))
	for i, c := range cols {
		defs[i] = quoteIdent(c) + " VARCHAR"
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s (%s)", table, strings.Join(defs, ", "))); err != nil {
		return fmt.Errorf("exec: duckdb create %s: %w", table, err)
	}

	return conn.Raw(func(dc any) error {
		driverConn, ok := dc.(driver.Conn)
		if !ok {
			return fmt.Errorf("exec: duckdb: connection does not expose driver.Conn")
		}
		app, err := duckdb.NewAppenderFromConn(driverConn, "", table)
		if err != nil {
			return fmt.Errorf("exec: duckdb appender %s: %w", table, err)
		}
		defer app.Close()

		vals := make([]driver.Value, len(cols))
		for _, r := range rows {
			for i, c := range cols {
				vals[i] = r[c]
			}
			if err := app.AppendRow(vals...); err != nil {
				return fmt.Errorf("exec: duckdb append %s: %w", table, err)
			}
		}
		return app.Flush()
	})
}

// columnsOf returns the union of keys across rows, sorted so the generated
// SQL and the output column order are deterministic.
func columnsOf(rows []connector.Row) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		for k := range r {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// quoteIdent double-quotes an identifier for DuckDB. Column names come from
// the catalog and the user's SQL, never raw from a connector response, but
// quoting keeps a column named e.g. "order" from becoming a syntax error.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
