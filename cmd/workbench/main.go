package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"roomworkbench/internal/store"
	"roomworkbench/internal/wb"
	"roomworkbench/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "data/workbench.db", "SQLite checkpoint path")
	seed := flag.Int64("seed", wb.DefaultSeed, "deterministic sample seed")
	flag.Parse()

	logger := log.New(os.Stderr, "workbench ", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	sqliteStore, err := store.Open(ctx, *dbPath)
	if err != nil {
		logger.Fatalf("ERROR code=E_INTERNAL object=sqlite message=%q", err.Error())
	}
	defer sqliteStore.Close()

	sample := wb.GenerateSample(*seed)
	engine, apiErr := wb.NewEngine(sample, sqliteStore)
	if apiErr != nil {
		logger.Printf("ERROR code=%s object=%s field=%s message=%q",
			apiErr.Code, apiErr.Object, apiErr.Field, apiErr.Message)
		logger.Fatal("deterministic sample failed validation")
	}

	if apiErr := engine.Recover(ctx); apiErr != nil {
		logger.Printf("ERROR code=%s object=%s message=%q", apiErr.Code, apiErr.Object, apiErr.Message)
	}
	view := engine.View()
	if view.Recovery.Discarded {
		logger.Printf("ERROR code=E_DIFF object=checkpoint message=%q", view.Recovery.Reason)
		logger.Printf("checkpoint snapshot discarded; performing full recompute")
	} else if view.Recovery.Recovered > 0 {
		logger.Printf("recovered %d room checkpoints from %s", view.Recovery.Recovered, *dbPath)
	}

	if _, apiErr := engine.Resume(ctx); apiErr != nil {
		if apiErr.Code == wb.CodeState {
			logger.Printf("ERROR code=%s object=%s message=%q", apiErr.Code, apiErr.Object, apiErr.Message)
		} else {
			logger.Printf("ERROR code=%s object=%s message=%q", apiErr.Code, apiErr.Object, apiErr.Message)
		}
	}

	server := web.NewServer(engine, logger)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		displayAddr := *addr
		if strings.HasPrefix(displayAddr, ":") {
			displayAddr = "127.0.0.1" + displayAddr
		}
		logger.Printf("room reverb workbench listening on http://%s", displayAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("ERROR code=E_INTERNAL object=http message=%q", err.Error())
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
