package workbench

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"economycore/internal/core"
	"economycore/internal/samples"
	"economycore/internal/workspace"
)

//go:embed static/*
var staticFiles embed.FS

type Server struct {
	manager *workspace.Manager
	mux     *http.ServeMux
}

func New(samplesDir string) (*Server, error) {
	s := &Server{
		manager: workspace.NewManager(samplesDir),
		mux:     http.NewServeMux(),
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/samples", s.handleSamples)
	s.mux.HandleFunc("/api/sessions", s.handleSessions)
	s.mux.HandleFunc("/api/sessions/", s.handleSession)
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return securityHeaders(s.mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "workbench UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"algorithm": core.AlgorithmVersion,
		"builder":   samples.GeneratorVersion,
	})
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	manifest, err := s.manager.Manifest()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		SampleID string `json:"sample_id"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.SampleID == "" {
		writeErr(w, core.NewError(core.CodeParam, "sample_id is required").WithField("sample_id"))
		return
	}
	session, err := s.manager.Open(req.SampleID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, session.View())
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	session, ok := s.manager.Get(parts[0])
	if !ok {
		writeErr(w, core.NewError(core.CodeParam, "session does not exist").WithField("session_id"))
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, session.View())
		return
	}
	switch parts[1] {
	case "commit":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var batch workspace.Batch
		if err := decodeBody(w, r, &batch); err != nil {
			writeErr(w, err)
			return
		}
		if batch.BatchID == "" {
			batch.BatchID = "batch-unspecified"
		}
		view, err := session.Commit(r.Context(), batch)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, workspace.CommitResult{Session: &view, BatchID: batch.BatchID})
	case "draft":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var batch workspace.Batch
		if err := decodeBody(w, r, &batch); err != nil {
			writeErr(w, err)
			return
		}
		if batch.BatchID == "" {
			batch.BatchID = "draft-unspecified"
		}
		view, err := session.ApplyDraft(batch)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	case "discard":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, session.Discard())
	case "preview":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var batch workspace.Batch
		if err := decodeBody(w, r, &batch); err != nil {
			writeErr(w, err)
			return
		}
		preview, err := session.PreviewDraft(r.Context(), batch)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	default:
		http.NotFound(w, r)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return core.NewError(core.CodeDecode, "request body is not valid JSON").Wrap(err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return core.NewError(core.CodeDecode, "request body contains trailing JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func writeErr(w http.ResponseWriter, err error) {
	e := core.AsError(err)
	status := http.StatusBadRequest
	switch e.Code {
	case core.CodeTxn, core.CodeDiverge:
		status = http.StatusConflict
	case core.CodeState:
		status = http.StatusUnprocessableEntity
	case core.CodeIO, core.CodeCheckpt:
		status = http.StatusInternalServerError
	case core.CodeCancel:
		status = http.StatusRequestTimeout
	}
	writeJSON(w, status, e)
}

func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, core.NewError(core.CodeParam, "method not allowed"))
}
