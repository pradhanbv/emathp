package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/pradhanbv/emathp/internal/mockzd"
)

func main() {
	addr := flag.String("addr", ":8082", "listen address")
	tickets := flag.Int("tickets", 50000, "number of synthetic tickets to serve")
	status := flag.String("status", "open", "status every synthetic ticket gets")
	maxInList := flag.Int("max-in-list", 200, "max organization_id values accepted in one request")
	delay := flag.Duration("delay", 0, "fixed delay before every response, for timeout testing (e.g. 5s)")
	flag.Parse()

	s := mockzd.New(
		mockzd.Tickets(*tickets, *status),
		mockzd.MaxInList(*maxInList),
		mockzd.Delay(*delay),
	)
	log.Printf("mockzd listening on %s (tickets=%d status=%q max-in-list=%d delay=%s)", *addr, *tickets, *status, *maxInList, *delay)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
