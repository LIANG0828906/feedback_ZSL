package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/session"
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	svc *session.Service
	mux *http.ServeMux
}

func New(svc *session.Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		errs.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "algorithm_version": "stitch-v1.0.0"})
	})
	s.mux.HandleFunc("/api/sets", s.handleSets)
	s.mux.HandleFunc("/api/sets/", s.handleSet)
	s.mux.Handle("/", s.staticHandler())
}

func (s *Server) handleSets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
		return
	}
	sets, err := s.svc.List()
	if err != nil {
		writeErr(w, err)
		return
	}
	errs.WriteJSON(w, http.StatusOK, map[string]any{"sets": sets})
}

func (s *Server) handleSet(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/sets/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "set id missing"))
		return
	}
	id := parts[0]
	sess, err := s.svc.Open(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		errs.WriteJSON(w, http.StatusOK, sess.Info())
		return
	}
	switch parts[1] {
	case "params":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeError(w, http.StatusBadRequest, errs.Wrap(errs.EPara, "invalid parameter JSON", err))
			return
		}
		snap, err := sess.UpdateParams(raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		errs.WriteJSON(w, http.StatusOK, snap)
	case "preview":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		var body struct {
			Version int64 `json:"version"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		res, err := sess.Preview(body.Version)
		if err != nil {
			writeErr(w, err)
			return
		}
		errs.WriteJSON(w, http.StatusOK, res)
	case "exact":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		var body struct {
			Version    int64 `json:"version"`
			AutoCommit *bool `json:"auto_commit"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		auto := false
		if body.AutoCommit != nil {
			auto = *body.AutoCommit
		}
		task, err := sess.StartExact(body.Version, auto)
		if err != nil {
			writeErr(w, err)
			return
		}
		errs.WriteJSON(w, http.StatusAccepted, task)
	case "tasks":
		if len(parts) < 3 {
			writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "task id missing"))
			return
		}
		taskID := parts[2]
		if len(parts) >= 4 && parts[3] == "pause" {
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
				return
			}
			task, err := sess.PauseTask(taskID)
			if err != nil {
				writeErr(w, err)
				return
			}
			errs.WriteJSON(w, http.StatusOK, task)
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		task, ok := sess.Task(taskID)
		if !ok {
			writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "task not found"))
			return
		}
		errs.WriteJSON(w, http.StatusOK, task)
	case "commit":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		var body struct {
			Version   int64 `json:"version"`
			PairIndex int   `json:"pair_index"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, errs.Wrap(errs.EPara, "invalid commit body", err))
			return
		}
		st, err := sess.CommitPair(body.Version, body.PairIndex)
		if err != nil {
			writeErr(w, err)
			return
		}
		errs.WriteJSON(w, http.StatusOK, st)
	case "retry":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		latest := sess.Latest()
		task, err := sess.StartExact(latest.Version, false)
		if err != nil {
			writeErr(w, err)
			return
		}
		errs.WriteJSON(w, http.StatusAccepted, task)
	case "reset-commits":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EInternal, "method not allowed"))
			return
		}
		if err := sess.ResetCommits(); err != nil {
			writeErr(w, err)
			return
		}
		errs.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "artifact":
		if len(parts) < 3 {
			writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "artifact key missing"))
			return
		}
		key := strings.Join(parts[2:], "/")
		data, ok := sess.Artifact(key)
		if !ok {
			writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "artifact not found"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		switch path.Ext(key) {
		case ".png":
			w.Header().Set("Content-Type", "image/png")
		case ".json":
			w.Header().Set("Content-Type", "application/json")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		_, _ = w.Write(data)
	default:
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "unknown API route"))
	}
}

func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			b, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "index unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func writeErr(w http.ResponseWriter, err error) {
	e := errs.As(err)
	writeError(w, errs.HTTPStatus(e.Code), e)
}

func writeError(w http.ResponseWriter, status int, e *errs.Error) {
	errs.WriteJSON(w, status, e)
}
