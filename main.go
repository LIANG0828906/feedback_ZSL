//go:build web

package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"strata/internal/service"
	"strata/internal/web"
)

//go:embed all:frontend/dist
var embeddedAssets embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:4173", "local browser preview address")
	root := flag.String("root", ".", "workspace root")
	flag.Parse()
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Fatal(err)
	}
	svc, err := service.New(absRoot)
	if err != nil {
		log.Fatal(err)
	}
	defer svc.Close()
	assets, err := fs.Sub(embeddedAssets, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}
	server := &web.Server{Service: svc, Assets: assets, Root: absRoot}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("STRATA ready at http://%s\n", *addr)
	fmt.Printf("workspace %s\n", absRoot)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
