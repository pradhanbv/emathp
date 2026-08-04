//go:build duckdb

package exec

// engines is the set the cross-engine equivalence tests run against. With
// -tags duckdb that is both implementations, so every contract those tests
// assert is checked on both.
func engines() []JoinEngine {
	return []JoinEngine{GoJoin{}, DuckJoin{MemoryLimit: "256MB", Threads: 1}}
}
