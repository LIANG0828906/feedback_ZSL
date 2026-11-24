//go:build !wails

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"occlusionlab/internal/httpui"
	"occlusionlab/internal/workbench"
)

func main() {
	dataDefault := os.Getenv("OCCLUSIONLAB_DATA")
	if dataDefault == "" {
		dataDefault = filepath.Join(".", "data")
	}
	dataDir := flag.String("data", dataDefault, "local workbench data directory")
	address := flag.String("addr", "127.0.0.1:34115", "headless diagnostic UI address")
	flag.Parse()

	service := workbench.NewService(*dataDir)
	dashboard, err := service.Bootstrap()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("occlusionlab: %d/%d windows committed, project=%s\n",
		dashboard.Progress.Committed, dashboard.Progress.Total, dashboard.Project.ContentHash[:12])
	server := httpui.New(service, *address, httpui.FindAssetDir())
	listener, listenErr := net.Listen("tcp", *address)
	if listenErr != nil {
		fmt.Fprintln(os.Stderr, listenErr)
		os.Exit(1)
	}
	fmt.Printf("diagnostic workbench: http://%s\n", *address)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if serveErr := server.Serve(listener); serveErr != nil {
		if friendly := httpui.FriendlyListenError(serveErr); friendly != nil {
			fmt.Fprintln(os.Stderr, friendly)
			os.Exit(1)
		}
	}
	service.Close()
}
