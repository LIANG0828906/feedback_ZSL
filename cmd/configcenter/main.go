package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"configcenter/internal/business"
	"configcenter/internal/notify"
	"configcenter/internal/seed"
	"configcenter/internal/server"
	"configcenter/internal/store"
	"configcenter/internal/stress"
	"configcenter/internal/telemetry"
)

const version = "1.0.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = runServer(os.Args[2:])
	case "client":
		err = runClient(os.Args[2:])
	case "stress":
		err = runStress(os.Args[2:])
	case "version":
		fmt.Printf("configcenter %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Printf("ERROR %v", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `configcenter commands:
  server   start the embedded SQLite configuration center
  client   run one long-polling simulated business process
  stress   publish the fixed-seed 23-item change storm and verify convergence
  version  print the build version

Run "configcenter <command> -h" for command flags.`)
}

func runServer(args []string) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	addr := flags.String("addr", envOr("CONFIGCENTER_ADDR", "127.0.0.1:18473"), "HTTP listen address")
	dbPath := flags.String("db", envOr("CONFIGCENTER_DB", "./data/configcenter.db"), "SQLite database path")
	mergeWindow := flags.Duration("merge-window", 500*time.Millisecond, "coalescing debounce window (minimum 200ms)")
	mergeMaxWait := flags.Duration("merge-max-wait", 2*time.Second, "maximum delay before a continuous storm is broadcast")
	pollTimeout := flags.Duration("poll-timeout", 20*time.Second, "default long-poll timeout")
	seedOrders := flags.Bool("seed-orders", true, "seed the built-in orders namespace when the database is empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *mergeWindow < 200*time.Millisecond {
		return fmt.Errorf("merge-window must be at least 200ms")
	}
	if *mergeMaxWait < *mergeWindow {
		return fmt.Errorf("merge-max-wait must not be shorter than merge-window")
	}
	if *pollTimeout < time.Second || *pollTimeout > 30*time.Second {
		return fmt.Errorf("poll-timeout must be between 1s and 30s")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	configStore, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer configStore.Close()

	metrics := telemetry.New()
	var notifier *notify.Notifier
	notifier = notify.New(
		*mergeWindow,
		*mergeMaxWait,
		configStore.LatestVersion,
		func(broadcast notify.Broadcast) {
			metrics.RecordMerge(broadcast.Namespace, broadcast.RawChanges)
		},
	)
	defer notifier.Close()

	if *seedOrders {
		seededVersion, _, err := configStore.EnsureNamespace(ctx, seed.DefaultNamespace, seed.DefaultOrderConfig())
		if err != nil {
			return fmt.Errorf("seed orders namespace: %w", err)
		}
		metrics.EnsureNamespace(seed.DefaultNamespace)
		notifier.Prime(seed.DefaultNamespace, seededVersion)
	}
	namespaces, err := configStore.ListNamespaces(ctx)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces {
		metrics.EnsureNamespace(namespace.Namespace)
		notifier.Prime(namespace.Namespace, namespace.Version)
	}
	sqliteVersion, err := configStore.SQLiteVersion(ctx)
	if err != nil {
		return err
	}

	handler := server.New(configStore, notifier, metrics, server.Options{
		MergeWindow:        *mergeWindow,
		MergeMaxWait:       *mergeMaxWait,
		DefaultPollTimeout: *pollTimeout,
		MaxPollTimeout:     30 * time.Second,
	})
	httpServer := &http.Server{
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	log.Printf(
		"CONFIGCENTER listening=%s sqlite=%s merge_window=%s max_wait=%s",
		listener.Addr(),
		sqliteVersion,
		*mergeWindow,
		*mergeMaxWait,
	)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func runClient(args []string) error {
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	baseURL := flags.String("base-url", envOr("CONFIGCENTER_URL", "http://127.0.0.1:18473"), "configuration center base URL")
	namespace := flags.String("namespace", seed.DefaultNamespace, "configuration namespace")
	clientID := flags.String("client-id", "client-1", "client identity used in reports and metrics")
	pollTimeout := flags.Duration("poll-timeout", 10*time.Second, "long-poll timeout sent to the server")
	applyDelay := flags.Duration("apply-delay", 30*time.Millisecond, "simulated hot-reload delay")
	maxPolls := flags.Int("max-polls", 0, "stop after this many polls; zero means run until interrupted")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*clientID) == "" {
		return fmt.Errorf("client-id is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(os.Stdout, "client="+*clientID+" ", log.LstdFlags|log.LUTC)
	runner := business.NewClient(business.ClientOptions{
		BaseURL:     *baseURL,
		Namespace:   *namespace,
		ClientID:    *clientID,
		PollTimeout: *pollTimeout,
		ApplyDelay:  *applyDelay,
		MaxPolls:    *maxPolls,
		Logger:      logger,
	})
	return runner.Run(ctx)
}

func runStress(args []string) error {
	flags := flag.NewFlagSet("stress", flag.ContinueOnError)
	baseURL := flags.String("base-url", envOr("CONFIGCENTER_URL", "http://127.0.0.1:18473"), "configuration center base URL")
	namespace := flags.String("namespace", seed.DefaultNamespace, "configuration namespace")
	clientCount := flags.Int("clients", 3, "expected client count")
	fixedSeed := flags.Int64("seed", seed.DefaultSeed, "fixed storm generator seed")
	mergeWindow := flags.Duration("merge-window", 500*time.Millisecond, "server merge window, used to bound post-rollback observation")
	publishGap := flags.Duration("publish-gap", 8*time.Millisecond, "delay between the 23 atomic publishes")
	convergeWait := flags.Duration("converge-wait", 15*time.Second, "maximum wait for all clients to report the final version")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	return stress.Run(ctx, stress.Options{
		BaseURL:      *baseURL,
		Namespace:    *namespace,
		ClientCount:  *clientCount,
		Seed:         *fixedSeed,
		MergeWindow:  *mergeWindow,
		PublishGap:   *publishGap,
		ConvergeWait: *convergeWait,
	}, logger)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
