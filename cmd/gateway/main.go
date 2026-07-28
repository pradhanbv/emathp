package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/identity/fixtures"
	"github.com/pradhanbv/emathp/internal/policy"
	"github.com/pradhanbv/emathp/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	catalogDir := flag.String("catalog", "testdata/catalog", "catalog fixture directory")
	policyDir := flag.String("policy", "testdata/policy", "policy fixture directory")
	sfURL := flag.String("sf-url", "http://localhost:8081", "Salesforce mock connector URL")
	flag.Parse()

	cat, err := catalog.Load(*catalogDir)
	if err != nil {
		log.Fatalf("load catalog: %v", err)
	}
	pol, err := policy.Load(*policyDir)
	if err != nil {
		log.Fatalf("load policy: %v", err)
	}

	// The issuer registry is control-plane state that would come from the
	// tenant registry in a real deployment; the fixtures package is the
	// same one the test suite uses (see internal/identity/fixtures).
	s := server.New(server.Deps{
		Catalog:  cat,
		Policy:   pol,
		Identity: fixtures.IssuerRegistry(),
		Sources: map[string]connector.Source{
			"sf": connector.NewHTTPSource(*sfURL),
		},
	})

	log.Printf("gateway listening on %s (sf connector -> %s)", *addr, *sfURL)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
