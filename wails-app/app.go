package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"prosodyalign/diagnostic"
)

// App is the Wails binding around the same deterministic domain and web
// diagnostic handler used by cmd/diag.
type App struct {
	ctx        context.Context
	server     *diagnostic.Server
	listener   net.Listener
	diagnostic string
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	workdir := filepath.Join(os.TempDir(), "prosodyalign-wails")
	server, err := diagnostic.NewServer(workdir)
	if err != nil {
		fmt.Printf("diagnostic startup failed: %v\n", err)
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = server.Close()
		fmt.Printf("diagnostic listener failed: %v\n", err)
		return
	}
	a.server = server
	a.listener = listener
	a.diagnostic = "http://" + listener.Addr().String()
	go func() {
		if err := http.Serve(listener, server.Handler()); err != nil && err != http.ErrServerClosed {
			fmt.Printf("diagnostic server stopped: %v\n", err)
		}
	}()
}

func (a *App) shutdown(_ context.Context) {
	if a.listener != nil {
		_ = a.listener.Close()
	}
	if a.server != nil {
		_ = a.server.Close()
	}
}

func (a *App) DiagnosticURL() string {
	return a.diagnostic
}
