//go:build !desktop

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"texlab/internal/webui"
	"texlab/internal/workbench"
)

func main() {
	addr := flag.String("addr", envOr("TEXLAB_ADDR", "127.0.0.1:34115"), "HTTP listen address")
	samples := flag.String("samples", envOr("TEXLAB_SAMPLES", "samples"), "deterministic sample directory")
	data := flag.String("data", envOr("TEXLAB_DATA", filepath.Join(".texlab", "data")), "checkpoint and report directory")
	workers := flag.Int("workers", 4, "maximum worker count")
	pixels := flag.Int("pixels", 262144, "per-worker pixel scratch limit")
	flag.Parse()
	svc, err := workbench.NewService(*samples, *data, *workers, *pixels)
	if err != nil {
		log.Fatal(err)
	}
	assets := envOr("TEXLAB_ASSETS", filepath.Join("frontend", "dist"))
	server := &webui.Server{Service: svc, AssetDir: assets}
	fmt.Printf("TexLab headless workbench: http://%s\n", *addr)
	fmt.Printf("samples=%s data=%s assets=%s\n", *samples, *data, assets)
	if err := http.ListenAndServe(*addr, server.Handler()); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
