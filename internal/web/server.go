package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ocrworkbench/internal/app"
	"ocrworkbench/internal/core"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	app *app.App
}

func New(application *app.App) *Server {
	return &Server{app: application}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/run", s.run)
	mux.HandleFunc("POST /api/pause", s.pause)
	mux.HandleFunc("POST /api/resume", s.resume)
	mux.HandleFunc("POST /api/cancel", s.cancel)
	mux.HandleFunc("POST /api/retry", s.retry)
	mux.HandleFunc("POST /api/fault", s.fault)
	mux.HandleFunc("POST /api/fault/clear", s.clearFault)
	mux.HandleFunc("GET /api/verify", s.verify)
	mux.HandleFunc("GET /api/export", s.export)
	mux.HandleFunc("GET /image/{id}", s.image)
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	return localOnly(noStore(mux))
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.Status())
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Start(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	s.app.Pause()
	writeJSON(w, http.StatusOK, map[string]bool{"paused": true})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	s.app.Resume()
	writeJSON(w, http.StatusOK, map[string]bool{"paused": false})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.app.Cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"canceled": true})
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Retry(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"retried": true})
}

func (s *Server) fault(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil {
		writeError(w, core.FieldError(core.CodePara, "index", "[0,2147483647]", "fault index must be an integer"))
		return
	}
	if err := s.app.InjectFault(r.URL.Query().Get("kind"), r.URL.Query().Get("image"), index); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"injected": true, "kind": r.URL.Query().Get("kind"), "index": index})
}

func (s *Server) clearFault(w http.ResponseWriter, r *http.Request) {
	s.app.ClearFaults()
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	status := s.app.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         status.VerificationOK,
		"comparison": status.Comparison,
	})
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "realtime"
	}
	run, ok := s.app.Export(kind)
	if !ok {
		http.Error(w, "E_STATE: no exported run of that kind", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", kind+".json"))
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(run)
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	manifest := s.app.Manifest()
	for _, image := range manifest.Images {
		if image.ID != id {
			continue
		}
		path := filepath.Join(s.app.DataDir(), filepath.Base(image.File))
		http.ServeFile(w, r, path)
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if core.IsCode(err, core.CodeState) {
		status = http.StatusConflict
	}
	if core.CodeOf(err) != "" {
		writeJSON(w, status, err)
		return
	}
	writeJSON(w, status, core.NewError("E_INTERNAL", err.Error()))
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "local access only", http.StatusForbidden)
			return
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, "local access only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type HTTPServer struct {
	*http.Server
}

func NewHTTPServer(addr string, handler http.Handler) *HTTPServer {
	return &HTTPServer{Server: &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}}
}
