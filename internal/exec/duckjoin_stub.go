//go:build !duckdb

package exec

import (
	"context"
	"fmt"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/plan"
)

// DuckJoin is the tier-1 engine's stub in a cgo-free build. The real
// implementation lives in duckjoin.go behind `-tags duckdb`, because
// importing go-duckdb forces CGO_ENABLED=1 and a libc runtime image, and the
// default gateway build is CGO_ENABLED=0 into distroless/static.
//
// It exists rather than being absent so `--join-engine=duckdb` fails with a
// sentence telling you what to do, instead of an unknown-flag error that
// says nothing about why.
type DuckJoin struct {
	MemoryLimit string
	Threads     int
}

func (DuckJoin) Name() string { return "duckdb-unavailable" }

func (DuckJoin) Join(context.Context, []JoinInput, []plan.Link) ([]connector.Row, error) {
	return nil, fmt.Errorf("exec: this binary was built without DuckDB (cgo). Rebuild with `go build -tags duckdb ./cmd/gateway` to use --join-engine=duckdb")
}
