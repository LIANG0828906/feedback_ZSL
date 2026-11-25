package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"ballistic/internal/diagnostic"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "loopback listen address")
	dbPath := flag.String("db", ".diagnostic/ballistic.sqlite", "SQLite snapshot path")
	flag.Parse()

	server, err := diagnostic.NewServer(*dbPath)
	if err != nil {
		log.Fatalf("initialize diagnostic workbench: %v", err)
	}
	defer server.Close()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("ballistic diagnostic workbench: http://%s/", listener.Addr().String())

	httpServer := &http.Server{Handler: server.Handler()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()
	<-ctx.Done()
	log.Printf("shutting down")
	_ = httpServer.Close()
}
