package webui

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"texlab/internal/apperr"
	"texlab/internal/workbench"
)

type Server struct {
	Service  *workbench.Service
	AssetDir string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/samples", s.handleSamples)
	mux.HandleFunc("/api/load", s.handleLoad)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/reorder", s.handleReorder)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/compare", s.handleCompare)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.Handle("/", s.static())
	return logRequests(mux)
}

func (s *Server) static() http.Handler {
	fs := http.FileServer(http.Dir(s.AssetDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, filepath.Join(s.AssetDir, "index.html"))
			return
		}
		if strings.Contains(r.URL.Path, "..") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if _, err := os.Stat(filepath.Join(s.AssetDir, filepath.Clean(strings.TrimPrefix(r.URL.Path, "/")))); err != nil {
			http.ServeFile(w, r, filepath.Join(s.AssetDir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	out, err := s.Service.ListSamples()
	write(w, out, err)
}

func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := s.Service.LoadSample(req.Name)
	write(w, out, err)
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	var req workbench.EditRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.Service.ApplyEdit(req)
	write(w, out, err)
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var req workbench.ReorderRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.Service.Reorder(req)
	write(w, out, err)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	var req workbench.RetryRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.Service.RetrySegment(req)
	write(w, out, err)
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	out, err := s.Service.ComparePaths()
	write(w, out, err)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	out, err := s.Service.Export()
	write(w, out, err)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return false
	}
	defer r.Body.Close()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(v); err != nil {
		write(w, nil, &apperr.Error{Code: apperr.EPara, Message: "decode JSON request", Details: err.Error()})
		return false
	}
	return true
}

func write(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		ae := apperr.As(err)
		status := http.StatusBadRequest
		if ae.Code == apperr.EIO || ae.Code == apperr.ECheckpoint {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": ae})
		return
	}
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
		return
	}
	_, _ = w.Write([]byte("{}"))
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
