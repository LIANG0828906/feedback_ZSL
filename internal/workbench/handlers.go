package workbench

import (
	"embed"
	"encoding/json"
	"net/http"
	"strconv"

	"enginelab.local/internal/loader"
	"enginelab.local/internal/model"
	"enginelab.local/internal/recovery"
)

//go:embed web/index.html
var webFS embed.FS

type APIError struct {
	OK    bool         `json:"ok"`
	Error *model.Error `json:"error"`
}

func NewHandler(svc *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", svc.handleIndex)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "enginelab-workbench"})
	})
	mux.HandleFunc("GET /api/sample", svc.handleSample)
	mux.HandleFunc("GET /api/versions", svc.handleVersions)
	mux.HandleFunc("GET /api/timeline", svc.handleTimeline)
	mux.HandleFunc("GET /api/timeline/{index}", svc.handleMoment)
	mux.HandleFunc("POST /api/recover", svc.handleRecover)
	mux.HandleFunc("GET /api/inject", svc.handleInjectGet)
	mux.HandleFunc("POST /api/inject", svc.handleInjectPost)
	mux.HandleFunc("POST /api/checkpoint/save/{index}", svc.handleCheckpointSave)
	mux.HandleFunc("GET /api/checkpoint/restore", svc.handleCheckpointRestore)
	mux.HandleFunc("POST /api/checkpoint/resume", svc.handleCheckpointResume)
	mux.HandleFunc("GET /api/checkpoint/resume", svc.handleCheckpointResume)
	return withJSONContentType(mux)
}

func withJSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeDomainError(w http.ResponseWriter, err error) {
	be := model.AsError(err)
	status := http.StatusUnprocessableEntity
	if be.Code == model.EBudgetThreads || be.Code == model.EBudgetMemory || be.Code == model.EFieldRequired {
		status = http.StatusBadRequest
	}
	if be.Code == model.EBackpressure {
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, APIError{OK: false, Error: be})
}

func (s *Service) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Service) handleSample(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"db_path": s.dbPath,
		"budget":  s.cfg.Budget,
		"bundle":  s.bundle,
		"edits":   []recovery.Edit{},
	})
}

type versionsResponse struct {
	OK         bool                                   `json:"ok"`
	Loaded     map[model.SchemaVersion]*model.Project `json:"loaded"`
	Comparison *model.Error                           `json:"comparison_error,omitempty"`
	Equal      bool                                   `json:"equal"`
}

func (s *Service) handleVersions(w http.ResponseWriter, r *http.Request) {
	loaded := map[model.SchemaVersion]*model.Project{}
	for version, doc := range s.bundle.Docs {
		p, err := loader.LoadDocument(doc)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		loaded[version] = p
	}
	err := recovery.CompareVersionProjects(loaded)
	resp := versionsResponse{OK: err == nil, Loaded: loaded, Equal: err == nil}
	if err != nil {
		resp.Comparison = model.AsError(err)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) handleTimeline(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moments": s.timeline})
}

func (s *Service) handleMoment(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeDomainError(w, model.NewError(model.EFieldRequired, "$.index", "invalid moment index"))
		return
	}
	moment, err := s.Moment(index)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moment": moment})
}

type recoverRequest struct {
	Workers     int   `json:"workers"`
	QueueDepth  int   `json:"queue_depth"`
	MemoryBytes int64 `json:"memory_bytes"`
	Reverse     bool  `json:"reverse"`
	BatchSize   int   `json:"batch_size"`
	FullStart   bool  `json:"full_start"`
}

func (s *Service) handleRecover(w http.ResponseWriter, r *http.Request) {
	var req recoverRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	baseline := s.bundle.Truth.CheckpointItems
	if req.FullStart {
		baseline = map[string]model.DataItem{}
		p, err := recovery.CloneProject(s.bundle.Project)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		p.Checkpoints = nil
		p.LatestCheckpoint = nil
		result, err := recovery.RecoverOneShot(p, recovery.Options{
			Budget:          model.Budget{Workers: req.Workers, QueueDepth: req.QueueDepth, MaxSegmentMemoryBytes: req.MemoryBytes},
			BaselineItems:   baseline,
			ReverseSchedule: req.Reverse,
			CommitBatchSize: req.BatchSize,
		})
		writeRecoverResult(w, result, err)
		return
	}
	result, err := recovery.RecoverOneShot(s.bundle.Project, recovery.Options{
		Budget:          model.Budget{Workers: req.Workers, QueueDepth: req.QueueDepth, MaxSegmentMemoryBytes: req.MemoryBytes},
		BaselineItems:   baseline,
		ReverseSchedule: req.Reverse,
		CommitBatchSize: req.BatchSize,
	})
	writeRecoverResult(w, result, err)
}

func writeRecoverResult(w http.ResponseWriter, result *model.RecoveryResult, err error) {
	if err != nil {
		be := model.AsError(err)
		status := http.StatusUnprocessableEntity
		if be.Code == model.EBudgetThreads || be.Code == model.EBudgetMemory {
			status = http.StatusBadRequest
		}
		if be.Code == model.EBackpressure {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": be, "result": result})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func (s *Service) handleInjectGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := InjectRequest{
		Kind:        q.Get("kind"),
		SegmentID:   q.Get("segment_id"),
		Workers:     atoiDefault(q.Get("workers"), 0),
		QueueDepth:  atoiDefault(q.Get("queue_depth"), 0),
		MemoryBytes: int64(atoiDefault(q.Get("memory_bytes"), 0)),
		CutRecords:  atoiDefault(q.Get("cut_records"), 0),
		Incomplete:  q.Get("incomplete") == "1" || q.Get("incomplete") == "true",
	}
	resp := s.Inject(r.Context(), req)
	writeInjectResponse(w, resp)
}

func (s *Service) handleInjectPost(w http.ResponseWriter, r *http.Request) {
	var req InjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDomainError(w, model.WrapError(model.EFieldRequired, "$.body", err))
		return
	}
	resp := s.Inject(r.Context(), req)
	writeInjectResponse(w, resp)
}

func writeInjectResponse(w http.ResponseWriter, resp *InjectResponse) {
	if resp.OK {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	status := http.StatusUnprocessableEntity
	if resp.Code == model.EBudgetThreads || resp.Code == model.EBudgetMemory {
		status = http.StatusBadRequest
	}
	if resp.Code == model.EBackpressure {
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, resp)
}

func (s *Service) handleCheckpointSave(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeDomainError(w, model.NewError(model.EFieldRequired, "$.index", "invalid moment index"))
		return
	}
	id, err := s.SaveCheckpoint(r.Context(), index)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "checkpoint_id": id, "db_path": s.dbPath})
}

func (s *Service) handleCheckpointRestore(w http.ResponseWriter, r *http.Request) {
	report, err := s.RestoreCheckpoint(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restore": report})
}

func (s *Service) handleCheckpointResume(w http.ResponseWriter, r *http.Request) {
	report, err := s.Resume(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "resume": report})
}

func atoiDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
