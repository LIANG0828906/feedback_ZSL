// Package web serves the local-only diagnostic dashboard.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"audioforge/internal/app"
)

//go:embed static/index.html
var assets embed.FS

type Server struct {
	report  func() *app.Report
	server  *http.Server
	address string
}

func New(report func() *app.Report) *Server {
	return &Server{report: report}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	index, _ := fs.Sub(assets, "static")
	mux.Handle("/", http.FileServer(http.FS(index)))
	mux.HandleFunc("/api/report", s.apiReport)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return mux
}

func (s *Server) Start(port int) (string, error) {
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("invalid ui port %d", port)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf("bind UI listener: %w", err)
	}
	s.address = "http://" + listener.Addr().String()
	s.server = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = s.server.Serve(listener)
	}()
	return s.address, nil
}

func (s *Server) Address() string { return s.address }

func (s *Server) Close() error {
	if s.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *Server) apiReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	report := s.report()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, report)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
