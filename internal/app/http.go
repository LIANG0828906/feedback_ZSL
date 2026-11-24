package app

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"cadlab/frontend"
	"cadlab/internal/model"
)

type callRequest struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

func NewHTTPHandler(a *App) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "algorithm": a.RenderPathName()})
	})
	mux.HandleFunc("/api/call", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req callRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
			writeJSON(w, fail(modelSchema(err)))
			return
		}
		payload := "{}"
		if len(req.Payload) > 0 {
			payload = string(req.Payload)
		}
		writeJSON(w, a.Call(req.Action, payload))
	})
	static, _ := fs.Sub(frontend.FS, "dist")
	fileServer := http.FileServer(http.FS(static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		clean := path.Clean(r.URL.Path)
		if clean == "/" {
			r.URL.Path = "/"
		} else {
			r.URL.Path = clean
		}
		if _, err := fs.Stat(static, strings.TrimPrefix(clean, "/")); err != nil && !strings.Contains(path.Base(clean), ".") {
			r.URL.Path = "/"
		}
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func modelSchema(err error) *model.Diagnostic {
	return model.Diag(model.CodeSchema, err.Error())
}
