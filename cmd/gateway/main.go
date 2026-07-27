package main

import (
	"log"
	"net/http"

	"github.com/pradhanbv/emathp/internal/server"
)

func main() {
	s := server.New()
	log.Println("gateway listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", s.Handler()))
}
