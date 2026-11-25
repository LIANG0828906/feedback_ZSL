package web

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"time"

	"terrainpatch/internal/engine"
	"terrainpatch/internal/model"
)

//go:embed index.html
var assets embed.FS

type Server struct {
	Engine       *engine.Engine
	BaseProject  *model.Project
	VerifyDBPath string
}

type stateResponse struct {
	Project    *model.Project `json:"project"`
	Graph      any            `json:"graph"`
	Report     *engine.Report `json:"report"`
	Truth      model.Truth    `json:"truth"`
	ServerTime string         `json:"serverTime"`
}

type editRequest struct {
	TileID string `json:"tileId"`
	Field  string `json:"field"`
	Value  int64  `json:"value"`
	GridX  int    `json:"gridX"`
	GridY  int    `json:"gridY"`
}

type tileRequest struct {
	TileID string `json:"tileId"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/edit", s.edit)
	mux.HandleFunc("POST /api/retry", s.retry)
	mux.HandleFunc("POST /api/failure", s.failure)
	mux.HandleFunc("POST /api/rebuild", s.rebuild)
	mux.HandleFunc("POST /api/verify", s.verify)
	mux.HandleFunc("GET /api/export", s.export)
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, stateResponse{
		Project: s.Engine.Project(), Graph: s.Engine.Graph(), Report: s.Engine.LastReport(),
		Truth: s.Engine.Project().Truth, ServerTime: time.Now().Format(time.RFC3339),
	})
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	var req editRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppError(w, model.NewError(model.CodeSchema, "invalid JSON: "+err.Error()))
		return
	}
	rep, ae := s.Engine.ApplyEdit(r.Context(), engine.EditSpec{
		TileID: req.TileID, Field: req.Field, IntValue: req.Value, GridX: req.GridX, GridY: req.GridY,
	})
	if ae != nil {
		writeAppError(w, ae)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	var req tileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppError(w, model.NewError(model.CodeSchema, "invalid JSON: "+err.Error()))
		return
	}
	rep, ae := s.Engine.RetryTile(r.Context(), req.TileID)
	if ae != nil {
		writeAppError(w, ae)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}

func (s *Server) failure(w http.ResponseWriter, r *http.Request) {
	var req tileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppError(w, model.NewError(model.CodeSchema, "invalid JSON: "+err.Error()))
		return
	}
	s.Engine.InjectFailure(req.TileID, model.CodeInternal)
	rep, ae := s.Engine.RecomputeOnly(r.Context(), req.TileID, "injected-failure")
	if ae != nil && ae.Code != model.CodeInternal {
		writeAppError(w, ae)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep, "expectedError": ae})
}

func (s *Server) rebuild(w http.ResponseWriter, r *http.Request) {
	rep, ae := s.Engine.RebuildAll(r.Context(), "manual-full")
	if ae != nil {
		writeAppError(w, ae)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	base := s.BaseProject
	if base == nil {
		base = s.Engine.Project()
	}
	dbPath := s.VerifyDBPath
	if dbPath == "" {
		dbPath = ":memory:"
	}
	report, ae := engine.VerifyThreePaths(context.Background(), base, engine.VerificationEdits(base), dbPath, engine.Options{
		Workers: 2,
	})
	if ae != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": ae, "report": report})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": report})
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	proj := s.Engine.Project()
	history, _ := s.Engine.Store().History(r.Context())
	payload := map[string]any{
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"project":    proj,
		"report":     s.Engine.LastReport(),
		"history":    history,
	}
	w.Header().Set("Content-Disposition", `attachment; filename="terrainpatch-export.json"`)
	writeJSON(w, http.StatusOK, payload)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeAppError(w http.ResponseWriter, e *model.AppError) {
	status := http.StatusBadRequest
	switch e.Code {
	case model.CodeSchema:
		status = http.StatusUnprocessableEntity
	case model.CodeMeta, model.CodeBudget, model.CodeDiff, model.CodeAttr:
		status = http.StatusUnprocessableEntity
	case model.CodeInternal:
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": e})
}
