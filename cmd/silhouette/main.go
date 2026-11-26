package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"example.com/silhouette"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "local listen address")
	flag.Parse()
	server := &http.Server{
		Addr:              *addr,
		Handler:           silhouette.NewHTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("silhouette diagnostics listening on http://%s", *addr)
	log.Fatal(server.ListenAndServe())
}
