package plan

// Origin says whether a predicate came from the query text or from
// injected tenant policy. Only Security predicates get the runtime
// verification treatment in the exec package.
type Origin int

const (
	UserOrigin Origin = iota
	SecurityOrigin
)

// Site says where a predicate is actually evaluated.
type Site int

const (
	Pushed Site = iota // sent to the connector as a query parameter
	Local              // evaluated in-process after fetch
)

// Disposition is the classification verdict for one predicate against one
// table's capability profile.
type Disposition int

const (
	PushedEnforced Disposition = iota // connector declares it enforces this predicate
	Residual                          // connector doesn't enforce it (or doesn't support the column/op); keep it local
	Unsupported                       // no capability entry covers this predicate at all
)

type Predicate struct {
	Column string
	Op     string
	Value  string
	Origin Origin
}

type Verdict struct {
	Disposition Disposition
	Site        Site
}

// Node is one node in the plan tree.
type Node interface {
	Children() []Node
}

// Scan reads one table's rows. Pushed holds the columns whose predicate
// was sent to the connector as a filter.
type Scan struct {
	Table   string
	Project []string
	Pushed  []string
}

func (s *Scan) Children() []Node { return nil }

// Filter is one predicate node, tagged with where it's evaluated and why
// it exists (user WHERE clause vs. injected security policy).
type Filter struct {
	Child Node
	Pred  Predicate
	Site  Site
}

func (f *Filter) Children() []Node { return []Node{f.Child} }

// ProjectCol is one output column. Mask is set when tenant policy applies
// column-level masking (CLS) to it.
type ProjectCol struct {
	Name string
	Mask *string
	// Side is the SQL alias of the join input this column came from - "a",
	// "t" - and is empty for a single-table plan. A join merges every side's
	// rows into one map, so a column name present on more than one side
	// (mocksf and mockzd both expose "id") would otherwise resolve to
	// whichever side was written last. The alias is known at plan time
	// (projectionQualified requires one for any joined query); Side is how
	// it survives to projection instead of being discarded there.
	//
	// This was "L"/"R" while joins were two-table. An N-way join has no
	// left and right, and the alias was already available - collapsing it
	// to a side marker was what made the merge non-composable.
	Side string
}

type Project struct {
	Child Node
	Cols  []ProjectCol
}

func (p *Project) Children() []Node { return []Node{p.Child} }

// Equi is an equi-join key pair.
type Equi struct {
	LeftCol  string
	RightCol string
}

// JoinSide is one input to a join: an alias, the table it reads, and the
// filtered subtree that produces its rows.
type JoinSide struct {
	Alias string
	Table string
	Root  Node
}

// Link joins one side to a side that is already part of the accumulated
// result. From must be an index that appears earlier in the join order than
// To, which is what makes the semi-join cascade possible: the keys pushed
// into To's connector come from rows already fetched.
//
// MaxInList is To's catalog-declared IN-list capacity, resolved at build
// time since it is a static capability rather than a runtime value.
type Link struct {
	From      int
	FromCol   string
	To        int
	ToCol     string
	MaxInList int
}

// Join is an N-way equi-join: Sides in the order they appear in the FROM
// clause, and len(Sides)-1 Links describing how each side attaches to one
// already joined.
//
// The strategy is a left-deep semi-join cascade. Sides[0] is scanned in
// full; every later side is a probe whose join-key values are pushed as a
// chunked IN-list built from rows already in hand. That is a positional
// rule (FROM order) rather than a cost-based one, since there are no
// cardinality statistics to order by - ADR-007 names choosing a fetch order
// across a join graph as the optimisation that remains.
type Join struct {
	Sides []JoinSide
	Links []Link
}

func (j *Join) Children() []Node {
	out := make([]Node, 0, len(j.Sides))
	for _, s := range j.Sides {
		out = append(out, s.Root)
	}
	return out
}
