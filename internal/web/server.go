package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"

	"roomworkbench/internal/wb"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	Engine *wb.Engine
	Logger *log.Logger
}

type actionResponse struct {
	OK     bool                   `json:"ok"`
	Error  *wb.APIError           `json:"error,omitempty"`
	Report *wb.InvalidationReport `json:"report,omitempty"`
	Diff   *wb.DiffReport         `json:"diff,omitempty"`
	State  wb.StateView           `json:"state"`
}

type injectRequest struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

type idRequest struct {
	RoomID string `json:"roomId"`
	DoorID string `json:"doorId"`
	Status string `json:"status"`
	Reset  bool   `json:"reset"`
}

func NewServer(engine *wb.Engine, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{Engine: engine, Logger: logger}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/boundary", s.handleBoundary)
	mux.HandleFunc("POST /api/rooms/params", s.handleParams)
	mux.HandleFunc("POST /api/precision", s.handlePrecision)
	mux.HandleFunc("POST /api/doors/add", s.handleAddDoor)
	mux.HandleFunc("POST /api/doors/delete", s.handleDeleteDoor)
	mux.HandleFunc("POST /api/state/transition", s.handleTransition)
	mux.HandleFunc("POST /api/recompute/full", s.handleFull)
	mux.HandleFunc("POST /api/recompute/resume", s.handleResume)
	mux.HandleFunc("POST /api/compare", s.handleCompare)
	mux.HandleFunc("POST /api/truth/verify", s.handleTruthVerify)
	mux.HandleFunc("POST /api/checkpoint/recover", s.handleRecover)
	mux.HandleFunc("POST /api/inject", s.handleInject)

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(staticSub)))
	return mux
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Engine.View())
}

func (s *Server) handleBoundary(w http.ResponseWriter, r *http.Request) {
	var req wb.BoundaryRequest
	if apiErr := decodeBody(r, &req); apiErr != nil {
		s.respondAction(w, nil, apiErr)
		return
	}
	report, apiErr := s.Engine.DragBoundary(r.Context(), req)
	s.respondAction(w, report, apiErr)
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	var req wb.ParamUpdateRequest
	if apiErr := decodeBody(r, &req); apiErr != nil {
		s.respondAction(w, nil, apiErr)
		return
	}
	report, apiErr := s.Engine.UpdateRoomParams(r.Context(), req)
	s.respondAction(w, report, apiErr)
}

func (s *Server) handlePrecision(w http.ResponseWriter, r *http.Request) {
	var precision wb.PrecisionSpec
	if apiErr := decodeBody(r, &precision); apiErr != nil {
		s.respondAction(w, nil, apiErr)
		return
	}
	report, apiErr := s.Engine.UpdatePrecision(r.Context(), precision)
	s.respondAction(w, report, apiErr)
}

func (s *Server) handleAddDoor(w http.ResponseWriter, r *http.Request) {
	var req wb.AddDoorRequest
	if apiErr := decodeBody(r, &req); apiErr != nil {
		s.respondAction(w, nil, apiErr)
		return
	}
	report, apiErr := s.Engine.AddDoor(r.Context(), req)
	s.respondAction(w, report, apiErr)
}

func (s *Server) handleDeleteDoor(w http.ResponseWriter, r *http.Request) {
	var req idRequest
	if apiErr := decodeBody(r, &req); apiErr != nil {
		s.respondAction(w, nil, apiErr)
		return
	}
	report, apiErr := s.Engine.DeleteDoor(r.Context(), req.DoorID)
	s.respondAction(w, report, apiErr)
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request) {
	var req idRequest
	if apiErr := decodeBody(r, &req); apiErr != nil {
		s.respondAction(w, nil, apiErr)
		return
	}
	apiErr := s.Engine.TransitionRoom(req.RoomID, wb.RoomStatus(req.Status))
	s.respondAction(w, nil, apiErr)
}

func (s *Server) handleFull(w http.ResponseWriter, r *http.Request) {
	var req idRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	report, apiErr := s.Engine.RunFull(r.Context(), req.Reset)
	s.respondAction(w, report, apiErr)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	report, apiErr := s.Engine.Resume(r.Context())
	s.respondAction(w, report, apiErr)
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	diff, apiErr := s.Engine.CompareWithFull(r.Context())
	resp := actionResponse{OK: apiErr == nil, Diff: diff, Error: apiErr, State: s.Engine.View()}
	if apiErr != nil {
		s.logAPIError(apiErr)
		writeJSON(w, statusForError(apiErr), resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTruthVerify(w http.ResponseWriter, r *http.Request) {
	apiErr := s.Engine.VerifyInitialTruth()
	s.respondAction(w, nil, apiErr)
}

func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	apiErr := s.Engine.Recover(r.Context())
	if apiErr == nil {
		_, apiErr = s.Engine.Resume(r.Context())
	}
	s.respondAction(w, nil, apiErr)
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var req injectRequest
	if apiErr := decodeBody(r, &req); apiErr != nil {
		s.respondAction(w, nil, apiErr)
		return
	}
	apiErr := s.runInjection(r.Context(), req)
	s.respondAction(w, nil, apiErr)
}

func (s *Server) runInjection(ctx context.Context, req injectRequest) *wb.APIError {
	switch req.Type {
	case "source_out_of_bounds", "listener_out_of_bounds", "door_nonadjacent", "room_overlap":
		view := s.Engine.View()
		sample := wb.CloneSample(view.Sample)
		return injectStaticSample(sample, req)
	case "room_failure":
		roomID := req.Target
		if roomID == "" {
			roomID = "R2"
		}
		_, apiErr := s.Engine.InjectRoomFailure(ctx, roomID)
		return apiErr
	default:
		return wb.NewAPIError(wb.CodeSchema, req.Type, "type", "unknown injection type")
	}
}

func injectStaticSample(sample *wb.Sample, req injectRequest) *wb.APIError {
	switch req.Type {
	case "source_out_of_bounds":
		if len(sample.Sources) == 0 {
			return wb.NewAPIError(wb.CodeSchema, "source", "", "no source to inject")
		}
		idx := 0
		if req.Target != "" {
			for i, src := range sample.Sources {
				if src.ID == req.Target {
					idx = i
				}
			}
		}
		sample.Sources[idx].X = sample.Bounds.X2 + 1
		return wb.ValidateSample(sample)
	case "listener_out_of_bounds":
		if len(sample.Listeners) == 0 {
			return wb.NewAPIError(wb.CodeSchema, "listener", "", "no listener to inject")
		}
		idx := 0
		if req.Target != "" {
			for i, l := range sample.Listeners {
				if l.ID == req.Target {
					idx = i
				}
			}
		}
		sample.Listeners[idx].Y = sample.Bounds.Y2 + 1
		return wb.ValidateSample(sample)
	case "door_nonadjacent":
		doorID := req.Target
		if doorID == "" {
			doorID = "D1"
		}
		for i := range sample.Openings {
			if sample.Openings[i].ID == doorID {
				sample.Openings[i].WallID = "W_B1"
				return wb.ValidateSample(sample)
			}
		}
		return wb.NewAPIError(wb.CodeSchema, doorID, "target", "injection target door not found")
	case "room_overlap":
		roomID := req.Target
		if roomID == "" {
			roomID = "R2"
		}
		for i := range sample.Rooms {
			if sample.Rooms[i].ID == roomID {
				sample.Rooms[i].Rect.X1 = 10
				return wb.ValidateSample(sample)
			}
		}
		return wb.NewAPIError(wb.CodeSchema, roomID, "target", "injection target room not found")
	default:
		return wb.NewAPIError(wb.CodeSchema, req.Type, "type", "unknown injection")
	}
}

func (s *Server) respondAction(w http.ResponseWriter, report *wb.InvalidationReport, apiErr *wb.APIError) {
	resp := actionResponse{
		OK:     apiErr == nil,
		Report: report,
		Error:  apiErr,
		State:  s.Engine.View(),
	}
	if apiErr != nil {
		s.logAPIError(apiErr)
		writeJSON(w, statusForError(apiErr), resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) logAPIError(apiErr *wb.APIError) {
	s.Logger.Printf("ERROR code=%s object=%s field=%s message=%q",
		apiErr.Code, apiErr.Object, apiErr.Field, apiErr.Message)
}

func statusForError(apiErr *wb.APIError) int {
	switch apiErr.Code {
	case wb.CodeSchema, wb.CodePara:
		return http.StatusBadRequest
	case wb.CodeState:
		return http.StatusConflict
	case wb.CodeDiff:
		return http.StatusConflict
	case wb.CodeConn:
		return http.StatusUnprocessableEntity
	case wb.CodeGeom:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func decodeBody(r *http.Request, target any) *wb.APIError {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return wb.NewAPIError(wb.CodePara, "request", typeErr.Field, "invalid parameter type: %v", err)
		}
		return wb.NewAPIError(wb.CodeSchema, "request", "", "invalid request JSON: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		log.Printf("write json: %v", err)
	}
}

var _ = fmt.Sprintf
