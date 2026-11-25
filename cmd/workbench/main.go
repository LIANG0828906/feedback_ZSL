package main

import (
	"flag"
	"fmt"
	"log"

	"feedback/maskworkbench/maskqa"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "local HTTP listen address")
	flag.Parse()

	server, err := maskqa.NewServer()
	if err != nil {
		log.Fatalf("initialize workbench: %v", err)
	}
	fmt.Printf("mask QA workbench: http://%s\n", *addr)
	fmt.Printf("seed=%d samples=%d\n", server.Set.Seed, len(server.Set.Fixtures))
	if err := server.ListenAndServe(*addr); err != nil {
		log.Fatal(err)
	}
}
