package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"battlekernel/internal/app"
)

//go:embed index.html
var assets embed.FS

type Server struct {
	OutDir string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/verify", s.verify)
	mux.HandleFunc("GET /api/report", s.report)
	if s.OutDir != "" {
		mux.Handle("GET /files/", http.StripPrefix("/files/", http.FileServer(http.Dir(s.OutDir))))
	}
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "battlekernel"})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	opts := app.DefaultOptions()
	if s.OutDir != "" {
		opts.OutDir = s.OutDir
	}
	report, err := app.RunVerification(r.Context(), opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	status := http.StatusOK
	if !report.OK {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, report)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.OutDir, "reports", "verification-report.json")
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "report not found; run verification first"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
