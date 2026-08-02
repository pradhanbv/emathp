package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/pradhanbv/emathp/internal/mockzd"
)

func main() {
	addr := flag.String("addr", ":8082", "listen address")
	tickets := flag.Int("tickets", 50000, "number of synthetic tickets to serve")
	status := flag.String("status", "open", "status every synthetic ticket gets")
	maxInList := flag.Int("max-in-list", 200, "max organization_id values accepted in one request")
	delay := flag.Duration("delay", 0, "fixed delay before every response, for timeout testing (e.g. 5s)")
	latencyMax := flag.Duration("latency-max", 0, "if > -delay, each response sleeps uniformly in [-delay, -latency-max] (stand-in for real connector latency, assumption A4)")
	flag.Parse()

	s := mockzd.New(
		mockzd.Tickets(*tickets, *status),
		mockzd.MaxInList(*maxInList),
		mockzd.DelayJitter(*delay, maxDelay(*delay, *latencyMax)),
	)
	log.Printf("mockzd listening on %s (tickets=%d status=%q max-in-list=%d latency=%v..%v)", *addr, *tickets, *status, *maxInList, *delay, *latencyMax)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}

// maxDelay keeps DelayJitter's contract (max >= min) even when only -delay
// is set, in which case the sleep is fixed at min - the pre-jitter behaviour.
func maxDelay(min, max time.Duration) time.Duration {
	if max < min {
		return min
	}
	return max
}
