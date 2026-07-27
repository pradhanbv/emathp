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
	flag.Parse()

	opts := []mocksf.Option{
		mocksf.Rows(*rows),
		mocksf.PageSize(*pageSize),
		mocksf.Capability("status", catalog.Enforced),
	}
	if *lieAbout != "" {
		opts = append(opts, mocksf.Capability(*lieAbout, catalog.Enforced), mocksf.LieAbout(*lieAbout))
	}

	s := mocksf.New(opts...)
	log.Printf("mocksf listening on %s (rows=%d page-size=%d lie-about=%q)", *addr, *rows, *pageSize, *lieAbout)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
