package diagnostic

import (
	"encoding/json"
	"net/http"
)

type Server struct {
	State *State
	Mux   *http.ServeMux
}

func NewServer(dbPath string) (*Server, error) {
	st, err := BuildState(dbPath)
	if err != nil {
		return nil, err
	}
	s := &Server{State: st, Mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.Mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageHTML))
	})
	s.Mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	s.Mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s.State)
	})
	s.Mux.HandleFunc("/api/sample/", func(w http.ResponseWriter, r *http.Request) {
		version := r.URL.Path[len("/api/sample/"):]
		var data []byte
		switch version {
		case "v1", "1.0.0":
			data = s.State.GeneratedSample.V1
		case "v2", "2.0.0":
			data = s.State.GeneratedSample.V2
		case "v3", "3.0.0":
			data = s.State.GeneratedSample.V3
		case "truth":
			data = s.State.GeneratedSample.Truth
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(data)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Mux.ServeHTTP(w, r)
}
