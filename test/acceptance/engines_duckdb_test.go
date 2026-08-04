//go:build duckdb

package acceptance

import "github.com/pradhanbv/emathp/internal/exec"

// joinEngines is what the N-way end-to-end test runs the same query through.
// With -tags duckdb that is both implementations, so identical output is
// asserted across engines rather than just within one.
func joinEngines() []exec.JoinEngine {
	return []exec.JoinEngine{exec.GoJoin{}, exec.DuckJoin{MemoryLimit: "256MB", Threads: 1}}
}
