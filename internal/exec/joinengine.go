package exec

import (
	"context"
	"fmt"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/plan"
)

// JoinInput is one already-fetched, already-filtered side of a join, tagged
// with the SQL alias that namespaces its columns in the merged output.
type JoinInput struct {
	Alias string
	Rows  []connector.Row
}

// JoinEngine merges N already-fetched row sets into one.
//
// The seam sits deliberately narrow. Everything before it - fanout, the
// semi-join cascade, OPA residual filtering and the runtime verification
// filter - is engine-independent and runs per side in fetchScanRows, so a
// join engine never sees a row the calling principal isn't entitled to.
// Everything after it - masking and projection - is engine-independent too.
// Swapping engines therefore cannot change what a query is allowed to
// return, only how fast the merge is and how large a merge is possible.
//
// Output contract: every column is namespaced by its side's alias via
// sideKey, because several sources can expose the same column name and
// plan.ProjectCol.Side is what disambiguates them at projection time. An
// engine returning a flat merge would silently resolve `a.id` to whichever
// side was written last - the bug TestJoinKeepsBothSidesOfCollidingColumns
// pins.
type JoinEngine interface {
	Join(ctx context.Context, sides []JoinInput, links []plan.Link) ([]connector.Row, error)
	Name() string
}

// GoJoin is the hand-rolled in-process hash join, applied left-deep across
// N sides. It is the default because it needs no cgo.
//
// Namespacing is applied to each side as it is folded in, never to the
// accumulator, which is what lets the fold compose: the accumulated rows
// keep every column under its own alias, so a later link finds its key by
// sideKey(alias, col) no matter how many joins have already happened.
type GoJoin struct{}

func (GoJoin) Name() string { return "go" }

func (GoJoin) Join(_ context.Context, sides []JoinInput, links []plan.Link) ([]connector.Row, error) {
	if len(sides) == 0 {
		return nil, nil
	}
	acc := namespaceRows(sides[0].Alias, sides[0].Rows)
	for _, l := range links {
		if l.To >= len(sides) || l.From >= len(sides) {
			return nil, fmt.Errorf("exec: join link out of range")
		}
		right := namespaceRows(sides[l.To].Alias, sides[l.To].Rows)
		acc = hashJoinOn(acc, right,
			sideKey(sides[l.From].Alias, l.FromCol),
			sideKey(sides[l.To].Alias, l.ToCol))
		if len(acc) == 0 {
			return nil, nil // inner join: nothing downstream can match
		}
	}
	return acc, nil
}

// namespaceRows rewrites every column to sideKey(alias, col).
func namespaceRows(alias string, rows []connector.Row) []connector.Row {
	out := make([]connector.Row, 0, len(rows))
	for _, r := range rows {
		n := make(connector.Row, len(r))
		for c, v := range r {
			n[sideKey(alias, c)] = v
		}
		out = append(out, n)
	}
	return out
}

// hashJoinOn is an inner equi-join on two already-namespaced key names.
// Build from the left (the accumulator), probe with the right.
func hashJoinOn(left, right []connector.Row, leftKey, rightKey string) []connector.Row {
	byKey := make(map[string][]connector.Row, len(left))
	for _, r := range left {
		k := r[leftKey]
		byKey[k] = append(byKey[k], r)
	}
	var out []connector.Row
	for _, rr := range right {
		for _, lr := range byKey[rr[rightKey]] {
			m := make(connector.Row, len(lr)+len(rr))
			for c, v := range lr {
				m[c] = v
			}
			for c, v := range rr {
				m[c] = v
			}
			out = append(out, m)
		}
	}
	return out
}

// option configures a Run call. Used instead of widening Run's signature so
// the many existing callers - server's sync and async paths, plancache's
// tests - keep compiling unchanged.
type option func(*runConfig)

type runConfig struct {
	engine JoinEngine
}

func newRunConfig(opts []option) runConfig {
	c := runConfig{engine: GoJoin{}}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithJoinEngine selects the engine used for the merge step of a join.
func WithJoinEngine(e JoinEngine) option {
	return func(c *runConfig) {
		if e != nil {
			c.engine = e
		}
	}
}
