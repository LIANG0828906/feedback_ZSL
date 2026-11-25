package diag

import (
	"encoding/json"
	"net/http"
)

func NewHandler(state *State) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(indexHTML))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("/api/invalidation", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		report, ok := state.Invalidations[kind]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown invalidation kind"})
			return
		}
		writeJSON(w, http.StatusOK, report)
	})
	mux.HandleFunc("/api/fault", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		fault, ok := state.Faults[kind]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown fault kind"})
			return
		}
		writeJSON(w, http.StatusOK, fault)
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
