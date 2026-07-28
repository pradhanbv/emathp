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
	flag.Parse()

	s := mockzd.New(
		mockzd.Tickets(*tickets, *status),
		mockzd.MaxInList(*maxInList),
	)
	log.Printf("mockzd listening on %s (tickets=%d status=%q max-in-list=%d)", *addr, *tickets, *status, *maxInList)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
