package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"roomreverb/internal/engine"
	"roomreverb/internal/model"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	Engine *engine.Engine
	Addr   string
}

type actionResponse struct {
	OK       bool            `json:"ok"`
	Error    *model.Error    `json:"error,omitempty"`
	Snapshot engine.Snapshot `json:"snapshot"`
	Compare  *model.Error    `json:"compare,omitempty"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/edit/params", s.handleParams)
	mux.HandleFunc("/api/doors/add", s.handleAddDoor)
	mux.HandleFunc("/api/doors/delete", s.handleDeleteDoor)
	mux.HandleFunc("/api/walls/move", s.handleMoveWall)
	mux.HandleFunc("/api/rooms/retry", s.handleRetry)
	mux.HandleFunc("/api/scope-check", s.handleScopeCheck)
	mux.HandleFunc("/api/compare", s.handleCompare)

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
	return mux
}

func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.Addr, s.Handler())
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Engine.Snapshot())
}

type paramsRequest struct {
	RoomID string `json:"room_id"`
	engine.RoomParamsEdit
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	var req paramsRequest
	if !decode(w, r, &req) {
		return
	}
	plan, merr := s.Engine.UpdateRoomParams(req.RoomID, req.RoomParamsEdit)
	if merr != nil {
		respondModelError(w, s.Engine, merr)
		return
	}
	_, merr = s.Engine.RecomputeWithPlan(plan)
	respondAfterCompute(w, s.Engine, merr)
}

func (s *Server) handleAddDoor(w http.ResponseWriter, r *http.Request) {
	var door model.Door
	if !decode(w, r, &door) {
		return
	}
	plan, merr := s.Engine.AddDoor(door)
	if merr != nil {
		respondModelError(w, s.Engine, merr)
		return
	}
	_, merr = s.Engine.RecomputeWithPlan(plan)
	respondAfterCompute(w, s.Engine, merr)
}

type deleteDoorRequest struct {
	DoorID string `json:"door_id"`
}

func (s *Server) handleDeleteDoor(w http.ResponseWriter, r *http.Request) {
	var req deleteDoorRequest
	if !decode(w, r, &req) {
		return
	}
	plan, merr := s.Engine.DeleteDoor(req.DoorID)
	if merr != nil {
		respondModelError(w, s.Engine, merr)
		return
	}
	_, merr = s.Engine.RecomputeWithPlan(plan)
	respondAfterCompute(w, s.Engine, merr)
}

type moveWallRequest struct {
	WallID     string `json:"wall_id"`
	Coordinate int    `json:"coordinate"`
}

func (s *Server) handleMoveWall(w http.ResponseWriter, r *http.Request) {
	var req moveWallRequest
	if !decode(w, r, &req) {
		return
	}
	plan, merr := s.Engine.MoveWallLine(req.WallID, req.Coordinate)
	if merr != nil {
		respondModelError(w, s.Engine, merr)
		return
	}
	_, merr = s.Engine.RecomputeWithPlan(plan)
	respondAfterCompute(w, s.Engine, merr)
}

type retryRequest struct {
	RoomID string `json:"room_id"`
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	var req retryRequest
	if !decode(w, r, &req) {
		return
	}
	_, merr := s.Engine.RetryRoom(req.RoomID)
	respondAfterCompute(w, s.Engine, merr)
}

type scopeRequest struct {
	Rooms []string `json:"rooms"`
}

func (s *Server) handleScopeCheck(w http.ResponseWriter, r *http.Request) {
	var req scopeRequest
	if !decode(w, r, &req) {
		return
	}
	merr := s.Engine.ScopeCheck(req.Rooms)
	writeJSON(w, http.StatusOK, map[string]any{"error": merr, "snapshot": s.Engine.Snapshot()})
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	snapshot := s.Engine.Snapshot()
	_, derr := engine.CompareIncrementalAndFull(snapshot.Level, snapshot.Config, snapshot.Result)
	writeJSON(w, http.StatusOK, actionResponse{
		OK:       derr == nil,
		Error:    derr,
		Snapshot: snapshot,
		Compare:  derr,
	})
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, model.NewError(model.CodeSchema, "request", err.Error()))
		return false
	}
	return true
}

func respondAfterCompute(w http.ResponseWriter, eng *engine.Engine, merr *model.Error) {
	if merr != nil {
		status := http.StatusInternalServerError
		switch merr.Code {
		case model.CodeState:
			status = http.StatusConflict
		case model.CodeUnreach, model.CodePrec, model.CodeDiff:
			status = http.StatusUnprocessableEntity
		case model.CodeGeom, model.CodeSchema:
			status = http.StatusBadRequest
		}
		writeJSON(w, status, actionResponse{OK: false, Error: merr, Snapshot: eng.Snapshot()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{OK: true, Snapshot: eng.Snapshot()})
}

func respondModelError(w http.ResponseWriter, eng *engine.Engine, merr *model.Error) {
	respondAfterCompute(w, eng, merr)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
