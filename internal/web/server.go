package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"mocapworkbench/internal/checkpoint"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/pipeline"
	"mocapworkbench/internal/retarget"
	"mocapworkbench/internal/sample"
	"mocapworkbench/internal/verify"
)

//go:embed static/index.html
var staticFS embed.FS

type SkeletonNode struct {
	Name        string     `json:"name"`
	Parent      string     `json:"parent"`
	Offset      model.Vec3 `json:"offset"`
	MaxAngleDeg float64    `json:"max_angle_deg"`
	EndEffector bool       `json:"end_effector"`
}

type FrameView struct {
	Frame           int              `json:"frame"`
	RootTranslation model.Vec3       `json:"root_translation"`
	Rotations       []model.Quat     `json:"rotations"`
	World           []model.Vec3     `json:"world"`
	Contacts        []string         `json:"contacts"`
	Stats           model.FrameStats `json:"stats"`
	Decisions       []model.Decision `json:"decisions"`
	State           string           `json:"state"`
}

type Dashboard struct {
	ClipID           string                             `json:"clip_id"`
	TargetID         string                             `json:"target_id"`
	AlgorithmVersion string                             `json:"algorithm_version"`
	ParameterVersion string                             `json:"parameter_version"`
	Mode             string                             `json:"mode"`
	Adaptive         bool                               `json:"adaptive"`
	Strategy         string                             `json:"strategy"`
	Plan             model.Plan                         `json:"plan"`
	Skeleton         []SkeletonNode                     `json:"skeleton"`
	JointNames       []string                           `json:"joint_names"`
	Frames           []FrameView                        `json:"frames"`
	ContactSpans     map[string][]model.ContactInterval `json:"contact_spans"`
	RootStart        model.Vec3                         `json:"root_start"`
	RootEnd          model.Vec3                         `json:"root_end"`
	Summary          model.Summary                      `json:"summary"`
	Mapping          model.MappingTrace                 `json:"mapping"`
	Report           *verify.Report                     `json:"report,omitempty"`
	Truth            model.Truth                        `json:"truth"`
	FPS              float64                            `json:"fps"`
	CommittedCount   int                                `json:"committed_count"`
	ResumeAction     string                             `json:"resume_action"`
	ScaleAudit       *verify.ScaleAudit                 `json:"scale_audit,omitempty"`
}

type retryRequest struct {
	Frame int `json:"frame"`
}

type retryResponse struct {
	OK        bool                 `json:"ok"`
	Frame     int                  `json:"frame"`
	Action    string               `json:"action"`
	Equal     bool                 `json:"equal"`
	FirstDiff *pipeline.Difference `json:"first_diff,omitempty"`
	Message   string               `json:"message"`
}

type Server struct {
	mu        sync.Mutex
	clip      model.Clip
	target    model.TargetSkeleton
	animation *model.Animation
	exact     *model.Animation
	report    *verify.Report
	cfg       model.Config
	dbPath    string
	dashboard *Dashboard
	mux       *http.ServeMux
}

func New(clip model.Clip, target model.TargetSkeleton, animation, exact *model.Animation, report *verify.Report, dbPath string, cfg model.Config) *Server {
	server := &Server{
		clip: clip, target: target, animation: animation, exact: exact,
		report: report, cfg: cfg, dbPath: dbPath,
	}
	server.dashboard = server.buildDashboard()
	server.mux = http.NewServeMux()
	server.routes()
	return server
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.serveIndex)
	s.mux.HandleFunc("/api/state", s.serveState)
	s.mux.HandleFunc("/api/frame/", s.serveFrame)
	s.mux.HandleFunc("/api/export", s.serveExport)
	s.mux.HandleFunc("/api/retry", s.serveRetry)
	s.mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "algorithm": model.AlgorithmVersion})
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) serveState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.dashboard)
}

func (s *Server) serveFrame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	value := strings.TrimPrefix(r.URL.Path, "/api/frame/")
	frame, err := strconv.Atoi(value)
	if err != nil || frame < 0 {
		http.Error(w, "invalid frame", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame >= len(s.dashboard.Frames) {
		http.Error(w, "frame out of range", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.dashboard.Frames[frame])
}

func (s *Server) serveExport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-retarget-%s.json"`, s.clip.ID, s.target.ID))
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(s.animation)
}

// serveRetry re-executes the deterministic solver and verifies the requested
// frame. The state machine itself is exercised during checkpoint commits; the
// endpoint is intentionally side-effect-free so a failed frame can be retried
// without mixing a new prefix into a committed snapshot.
func (s *Server) serveRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request retryRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Frame < 0 || request.Frame >= len(s.animation.Frames) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "frame out of range"})
		return
	}
	recomputed, err := retarget.Run(s.clip, s.target, s.cfg)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	diff := pipeline.FirstDiff(
		[]model.FrameResult{s.animation.Frames[request.Frame]},
		[]model.FrameResult{recomputed.Frames[request.Frame]},
		s.target,
	)
	response := retryResponse{
		OK: true, Frame: request.Frame, Action: "deterministic_frame_retry",
		Equal: diff == nil, FirstDiff: diff,
		Message: "frame re-solved and compared; committed sequence was not mutated",
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) buildDashboard() *Dashboard {
	animation := s.animation
	nodes := make([]SkeletonNode, len(s.target.Joints))
	names := make([]string, len(s.target.Joints))
	for i, joint := range s.target.Joints {
		nodes[i] = SkeletonNode{
			Name: joint.Name, Parent: joint.Parent, Offset: joint.Offset,
			MaxAngleDeg: joint.MaxAngleDeg, EndEffector: joint.EndEffector,
		}
		names[i] = joint.Name
	}
	frames := make([]FrameView, len(animation.Frames))
	for i, frame := range animation.Frames {
		positions, _ := sample.ForwardKinematics(s.target.Skeleton(), model.Frame{
			RootTranslation: frame.RootTranslation, Rotations: frame.Rotations,
		})
		frames[i] = FrameView{
			Frame: frame.Frame, RootTranslation: frame.RootTranslation,
			Rotations: frame.Rotations, World: positions, Contacts: frame.Contacts,
			Stats: frame.Stats, Decisions: frame.Decisions, State: frame.State,
		}
	}
	committed := len(animation.Frames)
	if count, err := checkpoint.SnapshotTableInfo(s.dbPath); err == nil {
		committed = count
	}
	return &Dashboard{
		ClipID: animation.ClipID, TargetID: animation.TargetID,
		AlgorithmVersion: animation.AlgorithmVersion, ParameterVersion: animation.ParameterVersion,
		Mode: animation.Plan.Mode, Adaptive: animation.Plan.RequestedMode == "auto",
		Strategy: animation.Plan.Strategy, Plan: animation.Plan,
		Skeleton: nodes, JointNames: names, Frames: frames,
		ContactSpans: animation.ContactSpans, RootStart: animation.RootStart,
		RootEnd: animation.RootEnd, Summary: animation.Summary, Mapping: animation.Mapping,
		Report: s.report, Truth: s.clip.Truth, FPS: s.clip.FPS,
		CommittedCount: committed, ResumeAction: animation.SnapshotAction,
		ScaleAudit: func() *verify.ScaleAudit {
			if s.report == nil {
				return nil
			}
			return s.report.ScaleAudit
		}(),
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	_ = encoder.Encode(value)
}
