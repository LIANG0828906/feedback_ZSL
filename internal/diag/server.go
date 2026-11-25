package diag

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"prosodyalign/internal/checkpoint"
	"prosodyalign/internal/generator"
	"prosodyalign/internal/model"
	"prosodyalign/internal/pipeline"
	"prosodyalign/internal/sched"
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	mu           sync.Mutex
	workDir      string
	suite        *generator.Suite
	store        *checkpoint.Store
	engine       *pipeline.Engine
	epoch        int
	lastEdit     pipeline.Edit
	lastOutput   *pipeline.EpochOutput
	verifyReport *pipeline.VerificationReport
	scheduler    *sched.Scheduler
}

type editRequest struct {
	TargetDurationSamples    *int64 `json:"target_duration_samples"`
	DefaultMaxStretchSamples *int64 `json:"default_max_stretch_samples"`
	AccentToleranceSamples   *int64 `json:"accent_tolerance_samples"`
	Description              string `json:"description"`
}

func NewServer(workDir string) (*Server, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	suite, err := generator.GenerateSuite(generator.FixedSeed, 4)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(workDir, "diagnostic-session.db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	projects, err := pipeline.CloneProjects(projectsFromSuite(suite))
	if err != nil {
		return nil, err
	}
	sch := sched.New(4, 2048, 4096)
	engine := pipeline.NewEngine("interactive_realtime", projects, store, sch, false)
	initial := pipeline.Edit{Description: "initial deterministic project"}
	out, err := engine.RunEpoch(0, initial)
	if err != nil {
		return nil, err
	}
	report, err := pipeline.RunThreeWay(filepath.Join(workDir, "three-way"), nil)
	if err != nil {
		return nil, err
	}
	return &Server{
		workDir: workDir, suite: suite, store: store, engine: engine,
		epoch: 0, lastEdit: initial, lastOutput: out, verifyReport: report, scheduler: sch,
	}, nil
}

func projectsFromSuite(suite *generator.Suite) []*model.Project {
	out := make([]*model.Project, 0, len(suite.Dialogs))
	for _, d := range suite.Dialogs {
		out = append(out, d.Project)
	}
	return out
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/verify", s.handleVerify)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return jsonContentType(mux)
}

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "service": "prosody-diag"})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{
		"suite": s.suite, "epoch": s.epoch, "params": s.engine.Params,
		"last_output": s.lastOutput, "results": s.engine.Results,
		"scheduler_metrics": s.scheduler.Metrics(),
		"verify_summary":    verifySummary(s.verifyReport),
	})
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req editRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	edit := pipeline.Edit{
		Description: req.Description, TargetDurationSamples: req.TargetDurationSamples,
		DefaultMaxStretch: req.DefaultMaxStretchSamples, AccentTolerance: req.AccentToleranceSamples,
	}
	if edit.Description == "" {
		edit.Description = "interactive parameter edit"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	out, err := s.engine.RunEpoch(s.epoch, edit)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	s.lastEdit = edit
	s.lastOutput = out
	writeJSON(w, map[string]any{
		"epoch": s.epoch, "output": out, "results": s.engine.Results,
		"scheduler_metrics": s.scheduler.Metrics(),
	})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.engine.RunEpoch(s.epoch, s.lastEdit)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	s.lastOutput = out
	writeJSON(w, map[string]any{"epoch": s.epoch, "output": out, "results": s.engine.Results})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	report := s.verifyReport
	refresh := r.URL.Query().Get("refresh") == "1"
	s.mu.Unlock()
	if refresh || report == nil {
		fresh, err := pipeline.RunThreeWay(filepath.Join(s.workDir, "three-way"), nil)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		s.mu.Lock()
		s.verifyReport = fresh
		report = fresh
		s.mu.Unlock()
	}
	writeJSON(w, report)
}

func verifySummary(r *pipeline.VerificationReport) map[string]any {
	if r == nil {
		return nil
	}
	return map[string]any{
		"all_equal":              r.Comparison.AllEqual,
		"commit_sequences_equal": r.Comparison.CommitSequencesEqual,
		"first_mismatch":         r.Comparison.FirstMismatch,
		"truth":                  r.Truth,
		"recovery_boundary":      r.RecoveryBoundary,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
