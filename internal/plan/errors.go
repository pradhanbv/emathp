package plan

import "errors"

// ErrUnsupportedPredicate maps to UNSUPPORTED_PREDICATE: the SQL surface
// only supports simple `column op literal` conjuncts.
var ErrUnsupportedPredicate = errors.New("unsupported predicate")

// ErrUnsupportedStatement maps to the same error family: only single-table
// SELECT is handled in this cycle.
var ErrUnsupportedStatement = errors.New("unsupported statement")

// ErrMaskingUnsupported fires when a catalog entry declares masking
// support this planner has no way to use: v1 only implements local CLS
// masking, so a connector claiming it can mask itself is a configuration
// error to catch at build time, not a pushdown opportunity - see
// DESIGN.md ADR-002 on why source-side masking is the exception for the
// SaaS-REST-API connector category this design targets.
var ErrMaskingUnsupported = errors.New("plan: connector declares masking support this planner cannot use")
