package diagnostic

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"terrainbake/model"
)

//go:embed web/*
var webFS embed.FS

// NewHTTPHandler serves the diagnostic UI and JSON API.
func NewHTTPHandler(c *Controller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, model.NewError(model.CodeValue, "method must be GET").WithField("http.method"))
			return
		}
		timeValue := 0
		if raw := r.URL.Query().Get("time"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				timeValue = parsed
			}
		}
		writeJSON(w, http.StatusOK, c.State(timeValue))
	})
	mux.HandleFunc("/api/raise", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, model.NewError(model.CodeValue, "method must be POST").WithField("http.method"))
			return
		}
		var req struct {
			ChunkID string  `json:"chunk_id"`
			X       int     `json:"x"`
			Z       int     `json:"z"`
			Delta   float64 `json:"delta"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, model.NewError(model.CodeDecode, "decode raise request: %v", err).WithField("raise"))
			return
		}
		state, err := c.Raise(req.ChunkID, req.X, req.Z, req.Delta)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, model.NewError(model.CodeValue, "method must be POST").WithField("http.method"))
			return
		}
		state, err := c.Reset()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		sub, err := fs.Sub(webFS, "web")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data, err := fs.ReadFile(sub, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(path, ".html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasSuffix(path, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(path, ".js"):
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		}
		_, _ = w.Write(data)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var coded *model.CodedError
	if errors.As(err, &coded) {
		switch coded.Code {
		case model.CodeSnap:
			status = http.StatusConflict
		case model.CodeDiff:
			status = http.StatusUnprocessableEntity
		case model.CodeIO:
			status = http.StatusInternalServerError
		}
	}
	writeJSON(w, status, map[string]any{"error": err.Error(), "detail": err})
}
