package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"example.com/dualring-config/internal/model"
	"example.com/dualring-config/internal/server"
	"example.com/dualring-config/internal/store"
)

type demoItem struct {
	Key   string
	Value string
}

func main() {
	var (
		addr               = flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
		dbPath             = flag.String("db", "data/config.db", "SQLite database path")
		logPath            = flag.String("log", "logs/configd.log", "service log path")
		seedCount          = flag.Int("seed", 0, "seed a demo namespace with this many parameters if empty")
		seedNamespace      = flag.String("seed-namespace", "payments", "demo namespace")
		canaryPercent      = flag.Float64("canary-percent", 10, "initial canary client percentage")
		observationSeconds = flag.Float64("observation-seconds", 10, "default canary observation period")
		errorThreshold     = flag.Float64("error-threshold", 0.005, "automatic rollback error threshold")
		historyLimit       = flag.Int("history-limit", 20, "retained versions per item")
		observerInterval   = flag.Duration("observer-interval", 100*time.Millisecond, "release observation scheduler interval")
		maxProcs           = flag.Int("maxprocs", 0, "GOMAXPROCS override; 0 keeps the environment value")
		memoryLimitMB      = flag.Int64("memory-limit-mb", 768, "Go heap soft limit in MiB; 0 disables")
	)
	flag.Parse()

	if *maxProcs > 0 {
		runtime.GOMAXPROCS(*maxProcs)
	}
	if *memoryLimitMB > 0 {
		debug.SetMemoryLimit(*memoryLimitMB << 20)
	}

	logger, closeLog, err := openLogger(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	configStore, err := store.Open(*dbPath, store.Options{
		CanaryPercent:     *canaryPercent,
		ErrorThreshold:    *errorThreshold,
		HistoryLimit:      *historyLimit,
		ObservationPeriod: time.Duration(*observationSeconds * float64(time.Second)),
	})
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer configStore.Close()

	if *seedCount > 0 {
		if err := ensureDemoSeed(configStore, *seedNamespace, *seedCount, logger); err != nil {
			logger.Fatalf("seed demo data: %v", err)
		}
	}

	api := server.New(configStore, logger, *observerInterval)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go api.RunObserver(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Printf("configd listening on http://%s db=%s canary=%.1f%% observation=%s threshold=%.3f%%",
		*addr, *dbPath, *canaryPercent, time.Duration(*observationSeconds*float64(time.Second)),
		*errorThreshold*100)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("serve: %v", err)
	}
	logger.Printf("configd stopped")
}

func openLogger(path string) (*log.Logger, func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	writer := io.MultiWriter(os.Stderr, file)
	return log.New(writer, "configd ", log.LstdFlags|log.Lmicroseconds|log.LUTC), func() {
		_ = file.Close()
	}, nil
}

func ensureDemoSeed(configStore *store.Store, namespace string, count int, logger *log.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := configStore.HeadConfig(ctx, namespace); err == nil {
		logger.Printf("demo namespace %s already exists; seed skipped", namespace)
		return nil
	} else {
		biz := model.AsBusiness(err)
		if biz.Code != model.CodeNotFound {
			return err
		}
	}
	if count < 1 {
		return errors.New("seed count must be positive")
	}
	items := make(map[string]string, count+1)
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("payment.param.%03d", i)
		items[key] = demoValue(i, 0)
	}
	items["_sim.effective_error_rate"] = "0"
	config, err := configStore.Bootstrap(ctx, namespace, "system:seed", items)
	if err != nil {
		return err
	}
	logger.Printf("seeded namespace=%s items=%d snapshot=%d digest=%s",
		namespace, len(config.Items), config.Version, config.Digest)
	return nil
}

func demoValue(index, generation int) string {
	payload := strings.Repeat("p", 300)
	return fmt.Sprintf(
		`{"gen":%d,"timeout_ms":%d,"rate_limit":%d,"enabled":true,"description":"payment parameter %03d","payload":"%s"}`,
		generation, 100+index, 1000+index, index, payload,
	)
}
