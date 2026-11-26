package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"warmcache/internal/gen"
	"warmcache/internal/httpui"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "local HTTP listen address")
	seed := flag.Int64("seed", gen.DefaultSeed, "deterministic sample seed")
	sampleDir := flag.String("samples", "samples", "sample output directory")
	writeSamples := flag.Bool("write-samples", false, "write deterministic v1/v2/v3 samples and exit")
	verify := flag.Bool("verify", false, "run local acceptance checks and exit")
	flag.Parse()

	if *writeSamples {
		paths, err := gen.WriteSamples(*sampleDir, *seed)
		if err != nil {
			log.Fatal(err)
		}
		for _, path := range paths {
			fmt.Println(path)
		}
		return
	}
	if *verify {
		if err := verifyInstallation(*seed); err != nil {
			log.Fatal(err)
		}
		return
	}
	server, err := httpui.New(*seed)
	if err != nil {
		log.Fatal(err)
	}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	log.Printf("warmcache control surface: http://%s", *addr)
	log.Printf("seed=%d deterministic samples enabled", *seed)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
