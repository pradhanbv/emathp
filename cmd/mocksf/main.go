package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/mocksf"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	rows := flag.Int("rows", 250, "number of synthetic rows to serve")
	pageSize := flag.Int("page-size", 100, "max rows per page")
	lieAbout := flag.String("lie-about", "", "column to declare enforced but silently ignore when filtering")
	accountsRegion := flag.String("accounts-region", "", "if set, serve join-ready accounts (name, external_id) all in this region, instead of the default alternating-region rows")
	latencyMin := flag.Duration("latency-min", 0, "minimum artificial response latency (stand-in for real connector latency, assumption A4)")
	latencyMax := flag.Duration("latency-max", 0, "maximum artificial response latency; with -latency-min, each response sleeps uniformly in [min,max]")
	flag.Parse()

	var opts []mocksf.Option
	if *accountsRegion != "" {
		opts = append(opts, mocksf.Accounts(*rows, *accountsRegion))
	} else {
		opts = append(opts, mocksf.Rows(*rows))
	}
	opts = append(opts, mocksf.PageSize(*pageSize), mocksf.Capability("status", catalog.Enforced))
	if *latencyMin > 0 {
		max := *latencyMax
		if max < *latencyMin {
			max = *latencyMin
		}
		opts = append(opts, mocksf.DelayJitter(*latencyMin, max))
	}
	if *lieAbout != "" {
		opts = append(opts, mocksf.Capability(*lieAbout, catalog.Enforced), mocksf.LieAbout(*lieAbout))
	}

	s := mocksf.New(opts...)
	log.Printf("mocksf listening on %s (rows=%d page-size=%d lie-about=%q latency=%v..%v)", *addr, *rows, *pageSize, *lieAbout, *latencyMin, *latencyMax)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
