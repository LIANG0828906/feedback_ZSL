package dialogue

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

// Server is the local loopback diagnostics HTTP service.
type Server struct {
	Data *DiagnosticData
	Mux  *http.ServeMux
}

func NewServer(data *DiagnosticData) *Server {
	s := &Server{Data: data, Mux: http.NewServeMux()}
	s.Mux.HandleFunc("/", s.handleIndex)
	s.Mux.HandleFunc("/api/health", s.handleHealth)
	s.Mux.HandleFunc("/api/diagnostic", s.handleDiagnostic)
	return s
}

// NewDiagnosticServer builds deterministic data and routes.
func NewDiagnosticServer() (*Server, error) {
	data, err := BuildDiagnosticData()
	if err != nil {
		return nil, err
	}
	return NewServer(data), nil
}

func (s *Server) Handler() http.Handler { return s.Mux }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "service": "dialogue-acoustic-kernel"})
}

func (s *Server) handleDiagnostic(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ListenAndServe starts the server. Callers should use a 127.0.0.1 address.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Mux)
}
