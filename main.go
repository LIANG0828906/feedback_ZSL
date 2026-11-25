package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"edgerefine/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "diagnostic dashboard listen address")
	samples := flag.String("samples", "samples", "samples root directory")
	dbPath := flag.String("db", "checkpoints/edge.db", "SQLite checkpoint path")
	flag.Parse()

	srv, err := web.New(*samples, *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start dashboard failed: %v\n", err)
		os.Exit(1)
	}
	defer srv.Close()

	host := *addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	fmt.Printf("edge refinement dashboard: http://%s\n", host)
	if err := srv.ListenAndServe(*addr); err != nil {
		fmt.Fprintf(os.Stderr, "server stopped: %v\n", err)
		os.Exit(1)
	}
}
