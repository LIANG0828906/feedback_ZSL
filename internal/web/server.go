package web

import (
	"encoding/json"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"strata/internal/formats"
	"strata/internal/model"
	"strata/internal/service"
)

type Server struct {
	Service *service.Service
	Assets  fs.FS
	Root    string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.health)
	mux.HandleFunc("/api/dispatch", s.dispatch)
	mux.HandleFunc("/api/upload", s.upload)
	mux.HandleFunc("/api/frame/", s.frame)
	mux.HandleFunc("/api/download", s.download)
	mux.HandleFunc("/", s.static)
	return mux
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, service.Envelope{OK: false})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusOK, service.Envelope{OK: false, Error: mapError("E_DECODE", "multipart upload is invalid", "upload", 0)})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusOK, service.Envelope{OK: false, Error: mapError("E_DECODE", "file field is required", "upload.file", 0)})
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".ply" && ext != ".pcbin" {
		writeJSON(w, http.StatusOK, service.Envelope{OK: false, Error: mapError("E_VER", "only .ply and .pcbin inputs are supported", "upload.extension", 0)})
		return
	}
	name := "local-" + time.Now().UTC().Format("20060102-150405.000000000") + ext
	path := filepath.Join(s.Root, "data", "samples", name)
	out, err := os.Create(path)
	if err != nil {
		writeJSON(w, http.StatusOK, service.Envelope{OK: false, Error: mapError("E_DECODE", err.Error(), "upload.path", 0)})
		return
	}
	_, copyErr := io.Copy(out, file)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		writeJSON(w, http.StatusOK, service.Envelope{OK: false, Error: mapError("E_DECODE", "upload could not be persisted", "upload", 0)})
		return
	}
	versionID, err := formats.InferVersion(path)
	if err != nil {
		_ = os.Remove(path)
		writeJSON(w, http.StatusOK, service.Envelope{OK: false, Error: model.AsAppError(err)})
		return
	}
	env := s.Service.LoadPath(path, versionID)
	writeJSON(w, http.StatusOK, env)
}

func mapError(code, message, path string, offset int64) *model.AppError {
	return model.NewError(code, message, path, offset, nil)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "service": "strata", "version": "1.0.0", "desktopReady": true,
	})
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": map[string]string{"code": "E_STATE", "message": "POST required"}})
		return
	}
	action := r.URL.Query().Get("action")
	var payload json.RawMessage
	if action != "" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, service.Envelope{OK: false})
			return
		}
		payload = body
	} else {
		var req struct {
			Action  string          `json:"action"`
			Payload json.RawMessage `json:"payload"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil || json.Unmarshal(body, &req) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": map[string]string{"code": "E_PARA", "message": "invalid dispatch envelope"}})
			return
		}
		action = req.Action
		payload = req.Payload
	}
	env := s.Service.Dispatch(action, payload)
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) frame(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/frame/")
	name = strings.TrimSuffix(name, ".png")
	data, ok := s.Service.FramePNG(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(abs, filepath.Join(s.Root, "artifacts")) {
		http.Error(w, "path is outside the export directory", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(abs)+`"`)
	http.ServeFile(w, r, abs)
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if s.Assets == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if _, err := fs.Stat(s.Assets, name); err != nil {
		name = "index.html"
	}
	data, err := fs.ReadFile(s.Assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if typ := mime.TypeByExtension(filepath.Ext(name)); typ != "" {
		w.Header().Set("Content-Type", typ)
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
