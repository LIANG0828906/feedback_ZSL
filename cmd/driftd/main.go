package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"syscall"
	"time"

	"driftconverge/internal/api"
	"driftconverge/internal/config"
	"driftconverge/internal/engine"
	"driftconverge/internal/gitsource"
	"driftconverge/internal/metrics"
	"driftconverge/internal/model"
	"driftconverge/internal/store"
)

func main() {
	runtime.GOMAXPROCS(2)

	once := flag.Bool("once", false, "run exactly one reconciliation round and print a JSON report")
	jsonOutput := flag.Bool("json", true, "print JSON from --once")
	httpAddr := flag.String("http-addr", "", "override DRIFT_HTTP_ADDR")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stateStore, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer stateStore.Close()
	if err := stateStore.Migrate(ctx); err != nil {
		log.Fatalf("database migration: %v", err)
	}

	clusters := []engine.ClusterSpec{
		{
			Name:       cfg.DevBranch,
			Branch:     cfg.DevBranch,
			Role:       "dev",
			Namespace:  cfg.DevNamespace,
			Kubeconfig: cfg.DevKubeconfig,
		},
		{
			Name:       cfg.ProdBranch,
			Branch:     cfg.ProdBranch,
			Role:       "prod",
			Namespace:  cfg.ProdNamespace,
			Kubeconfig: cfg.ProdKubeconfig,
		},
	}
	registry := metrics.New([]string{cfg.DevBranch, cfg.ProdBranch})
	for _, cluster := range clusters {
		status, statusErr := stateStore.LatestClusterStatus(ctx, cluster.Name)
		if statusErr == nil {
			registry.Update(status)
		} else if !errors.Is(statusErr, store.ErrNotFound) {
			log.Printf("cannot initialize metrics for %s: %v", cluster.Name, statusErr)
		}
	}

	source := gitsource.Source{
		RepoDir:      cfg.GitRepo,
		ManifestPath: cfg.ManifestPath,
	}
	roundEngine, err := engine.New(engine.Options{
		Clusters:         clusters,
		Source:           source,
		Store:            stateStore,
		Metrics:          registry,
		AdmissionTimeout: cfg.AdmissionTimeout,
		AdmissionPoll:    cfg.AdmissionPoll,
	})
	if err != nil {
		log.Fatalf("engine: %v", err)
	}

	if *once {
		report, runErr := roundEngine.RunRound(ctx)
		if *jsonOutput {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetEscapeHTML(false)
			encoder.SetIndent("", "  ")
			_ = encoder.Encode(report)
		} else {
			for _, cluster := range report.Clusters {
				fmt.Printf("%s conclusion=%s differences=%d policy_blocks=%d blocked=%d\n",
					cluster.Cluster, cluster.Convergence, cluster.DiffCount,
					cluster.PolicyBlockCount, cluster.BlockedCount)
			}
		}
		if runErr != nil {
			log.Printf("round not committed: %v", runErr)
			os.Exit(1)
		}
		return
	}

	apiServer := api.New(roundEngine, stateStore, registry, []string{cfg.DevBranch, cfg.ProdBranch})
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("drift convergence API listening on %s", cfg.HTTPAddr)
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("http server: %v", serveErr)
			stop()
		}
	}()

	go runPeriodically(ctx, roundEngine, cfg.Interval)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	log.Printf("drift convergence service stopped")
}

func runPeriodically(ctx context.Context, roundEngine *engine.Engine, interval time.Duration) {
	run := func() {
		roundCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		report, err := roundEngine.RunRound(roundCtx)
		if err != nil {
			if !errors.Is(err, engine.ErrBusy) {
				log.Printf("round failed to commit: %v", err)
			}
			return
		}
		clusters := append([]model.ClusterReport(nil), report.Clusters...)
		sort.Slice(clusters, func(i, j int) bool { return clusters[i].Cluster < clusters[j].Cluster })
		for _, cluster := range clusters {
			log.Printf(
				"round=%s cluster=%s observable=%t expected=%d diffs=%d policy_blocks=%d blocked=%d converged_resources=%d conclusion=%s",
				report.RoundID,
				cluster.Cluster,
				cluster.Observable,
				cluster.ExpectedCount,
				cluster.DiffCount,
				cluster.PolicyBlockCount,
				cluster.BlockedCount,
				cluster.ConvergedResources,
				cluster.Convergence,
			)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
