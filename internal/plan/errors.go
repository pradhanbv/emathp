package plan

import "errors"

// ErrUnsupportedPredicate maps to UNSUPPORTED_PREDICATE: the SQL surface
// only supports simple `column op literal` conjuncts.
var ErrUnsupportedPredicate = errors.New("unsupported predicate")

// ErrUnsupportedStatement maps to the same error family: only single-table
// SELECT is handled in this cycle.
var ErrUnsupportedStatement = errors.New("unsupported statement")
