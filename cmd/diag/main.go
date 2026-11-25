package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"prosodyalign/internal/diag"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "diagnostic interface listen address")
	workdir := flag.String("workdir", "artifacts/diag", "local SQLite and checkpoint directory")
	flag.Parse()

	server, err := diag.NewServer(*workdir)
	if err != nil {
		log.Fatalf("start diagnostic server: %v", err)
	}
	defer server.Close()

	fmt.Printf("prosody diagnostic UI: http://%s\n", *addr)
	fmt.Printf("workdir: %s\n", *workdir)
	if err := http.ListenAndServe(*addr, server.Handler()); err != nil {
		log.Fatal(err)
	}
}
