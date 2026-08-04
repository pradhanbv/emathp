//go:build !duckdb

package exec

// Without -tags duckdb only the cgo-free engine is compiled in, so the
// equivalence tests degrade to self-consistency checks on GoJoin rather than
// silently passing against a stub that returns errors.
func engines() []JoinEngine {
	return []JoinEngine{GoJoin{}}
}
