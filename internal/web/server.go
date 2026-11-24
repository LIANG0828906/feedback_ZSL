// Package web serves the local diagnostic dashboard using only the standard
// library and embedded static files. No external network resource is loaded.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"collectionrecon/internal/model"
)

//go:embed static/*
var staticFS embed.FS

// Dashboard is the complete snapshot rendered by the dashboard.
type Dashboard struct {
	Region   *model.Region        `json:"region"`
	Report   model.PipelineReport `json:"report"`
	Checks   any                  `json:"checks"`
	QueueLog any                  `json:"queueLog"`
}

// Server exposes the dashboard and its JSON data endpoints.
type Server struct {
	data Dashboard
	mux  *http.ServeMux
}

// NewServer builds the HTTP handler tree.
func NewServer(data Dashboard) (*Server, error) {
	s := &Server{data: data, mux: http.NewServeMux()}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("/api/data", s.handleData)
	s.mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return s, nil
}

func (s *Server) handleData(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.data)
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }
