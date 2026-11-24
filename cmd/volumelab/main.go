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

	"volumelab/pkg/httpserver"
	"volumelab/pkg/service"
	"volumelab/pkg/webui"
)

func main() {
	var (
		addr            = flag.String("addr", "127.0.0.1:8090", "local HTTP listen address")
		manifestPath    = flag.String("manifest", "", "default local sample manifest")
		volumeID        = flag.String("volume", "", "default volume id")
		sessionDir      = flag.String("session-dir", "", "session snapshot directory")
		snapshotPath    = flag.String("snapshot", "", "explicit snapshot file path")
		maxParallelism  = flag.Int("max-parallel", 3, "maximum resampling parallelism")
		maxBufferPixels = flag.Int("max-buffer-pixels", 16384, "pixel buffer limit per worker task")
	)
	flag.Parse()

	svc := service.New(service.Options{
		ManifestPath:    *manifestPath,
		VolumeID:        *volumeID,
		SessionDir:      *sessionDir,
		SnapshotPath:    *snapshotPath,
		MaxParallelism:  *maxParallelism,
		MaxBufferPixels: *maxBufferPixels,
	})
	handler := httpserver.Handler(svc, webui.FS)
	server := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Printf("VolumeLab local workstation: http://%s\n", *addr)
	if *manifestPath != "" {
		fmt.Printf("Default manifest: %s\n", *manifestPath)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-signals:
		log.Printf("received %s, closing committed sessions", sig)
	case err := <-errCh:
		log.Fatalf("HTTP server failed: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	_ = svc.Invoke(context.Background(), "close", "{}")
}
