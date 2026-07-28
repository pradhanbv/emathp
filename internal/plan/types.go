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

// Join is a two-table equi-join. v1's only strategy is semi-join: Left is
// always the build side (scanned in full) and Right the probe side (its
// join-key values pushed as a chunked IN-list) - a positional rule (FROM
// table = build, JOIN table = probe) rather than a cost-based choice,
// since there are no cardinality stats to choose from. MaxInList is the
// probe table's catalog-declared IN-list capacity, resolved at build time
// since it's a static capability, not a runtime value.
type Join struct {
	Left, Right Node
	On          Equi
	MaxInList   int
}

func (j *Join) Children() []Node { return []Node{j.Left, j.Right} }
