package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/freshness"
	"github.com/pradhanbv/emathp/internal/identity/fixtures"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/internal/plancache"
	"github.com/pradhanbv/emathp/internal/policy"
	"github.com/pradhanbv/emathp/internal/ratelimit"
	"github.com/pradhanbv/emathp/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	catalogDir := flag.String("catalog", "testdata/catalog", "catalog fixture directory")
	policyDir := flag.String("policy", "testdata/policy", "policy fixture directory")
	sfURL := flag.String("sf-url", "http://localhost:8081", "Salesforce mock connector URL")
	sfLimit := flag.Int("sf-limit", 0, "sf connector call budget for this process's lifetime (0 = unlimited)")
	zdURL := flag.String("zd-url", "http://localhost:8082", "Zendesk mock connector URL")
	zdLimit := flag.Int("zd-limit", 0, "zd connector call budget for this process's lifetime (0 = unlimited)")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/HTTP collector for traces, host:port with no scheme (e.g. jaeger:4318); empty disables tracing")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *otlpEndpoint != "" {
		shutdown, err := obs.InitTracing(ctx, *otlpEndpoint)
		if err != nil {
			log.Fatalf("init tracing: %v", err)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				log.Printf("tracer shutdown: %v", err)
			}
		}()
		log.Printf("tracing enabled -> %s", *otlpEndpoint)
	}

	cat, err := catalog.Load(*catalogDir)
	if err != nil {
		log.Fatalf("load catalog: %v", err)
	}
	pol, err := policy.Load(*policyDir)
	if err != nil {
		log.Fatalf("load policy: %v", err)
	}

	limiter := ratelimit.New()
	if *sfLimit > 0 {
		limiter.SetLimit("sf", *sfLimit)
	}
	if *zdLimit > 0 {
		limiter.SetLimit("zd", *zdLimit)
	}

	// The issuer registry is control-plane state that would come from the
	// tenant registry in a real deployment; the fixtures package is the
	// same one the test suite uses (see internal/identity/fixtures).
	s := server.New(server.Deps{
		Catalog:   cat,
		Policy:    pol,
		Identity:  fixtures.IssuerRegistry(),
		PlanCache: plancache.New(),
		RateLimit: limiter,
		Freshness: freshness.New(),
		Sources: map[string]connector.Source{
			"sf": connector.NewHTTPSource(*sfURL),
			"zd": connector.NewHTTPSource(*zdURL),
		},
	})

	httpServer := &http.Server{Addr: *addr, Handler: s.Handler()}

	go func() {
		log.Printf("gateway listening on %s (sf connector -> %s, zd connector -> %s)", *addr, *sfURL, *zdURL)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Wait for SIGINT/SIGTERM, then shut down gracefully so the deferred
	// tracer shutdown above actually runs and flushes pending spans -
	// log.Fatal would have skipped every defer in this function.
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown: %v", err)
	}
}
