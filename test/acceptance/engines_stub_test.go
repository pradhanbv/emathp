//go:build !duckdb

package acceptance

import "github.com/pradhanbv/emathp/internal/exec"

// Without -tags duckdb only the cgo-free engine is compiled in. The N-way
// test still runs - proving the default engine does 4-way joins end to end,
// which is the claim that matters most in the default build.
func joinEngines() []exec.JoinEngine {
	return []exec.JoinEngine{exec.GoJoin{}}
}
