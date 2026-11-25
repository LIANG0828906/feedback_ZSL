package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	lipsync "lipsync"
)

type Server struct {
	mu         sync.Mutex
	service    *lipsync.Service
	mux        *http.ServeMux
	queueDemo  []lipsync.QueueEvent
	acceptance *lipsync.AcceptanceReport
}

func NewServer(dbPath string) (*Server, error) {
	project := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	pool := lipsync.PoolOptions{MaxParallelism: 3, PerWorkerBuffer: 32, QueueBudget: 4}
	service, err := lipsync.NewService(project, lipsync.ServiceOptions{
		DBPath: dbPath, RunID: "web-local-run", Pool: pool,
	})
	if err != nil {
		return nil, err
	}
	if _, _, _, err := service.ProcessAndCommitAll(context.Background(), 0, nil); err != nil {
		_ = service.Close()
		return nil, err
	}
	s := &Server{service: service, mux: http.NewServeMux(), queueDemo: []lipsync.QueueEvent{}}
	s.routes()
	return s, nil
}

func (s *Server) Close() error { return s.service.Close() }

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) ListenAndServe(addr string) error {
	server := &http.Server{Addr: addr, Handler: s.mux, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServe()
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/edit", s.handleEdit)
	s.mux.HandleFunc("/api/inject", s.handleInject)
	s.mux.HandleFunc("/api/queue-demo", s.handleQueueDemo)
	s.mux.HandleFunc("/api/acceptance", s.handleAcceptance)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	commits, err := s.service.Store.ListCommitRecords(s.service.Project.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project":     s.service.Project,
		"result":      s.service.LastResult,
		"manifest":    s.service.LastManifest,
		"graph":       s.service.Engine.Snapshot.Graph,
		"commits":     commits,
		"queueEvents": s.service.QueueEvents,
		"queueDemo":   s.queueDemo,
		"poolOptions": s.service.Options.Pool,
		"cacheStats":  s.service.Cache.Stats(),
		"acceptance":  s.acceptance,
	})
}

type editRequest struct {
	lipsync.Edit
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var edit lipsync.Edit
	if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": lipsync.SchemaError("body", "invalid JSON: %v", err)})
		return
	}
	result, manifest, diff, err := s.service.ApplyEdit(edit)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	response := map[string]any{"ok": diff == nil, "manifest": manifest, "diff": diff, "result": result}
	if diff != nil {
		status = http.StatusConflict
	}
	writeJSON(w, status, response)
}

type injectRequest struct {
	Kind string `json:"kind"`
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req injectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Kind = r.URL.Query().Get("kind")
	}
	switch lipsync.InjectKind(req.Kind) {
	case lipsync.InjectOrder, lipsync.InjectLabel, lipsync.InjectVis:
		project := lipsync.GenerateProject(lipsync.GenerateOptions{Inject: lipsync.InjectKind(req.Kind)})
		_, err := lipsync.BuildFullResult(project)
		writeError(w, err)
	case lipsync.InjectStage:
		fail := 3
		run, err := lipsync.RunShardPool(r.Context(),
			lipsync.PoolOptions{MaxParallelism: 2, PerWorkerBuffer: 8, QueueBudget: 4},
			lipsync.ShardWorksForProject(lipsync.GenerateProject(lipsync.DefaultGenerateOptions()), &fail),
		)
		if err != nil {
			writeError(w, err)
			return
		}
		writeError(w, lipsync.FirstOutcomeError(run.Outcomes))
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": lipsync.SchemaError("kind", "unknown injection %q", req.Kind)})
	}
}

func (s *Server) handleQueueDemo(w http.ResponseWriter, r *http.Request) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	opts := lipsync.PoolOptions{MaxParallelism: 1, PerWorkerBuffer: 2, QueueBudget: 1}
	works := []lipsync.ShardWork{
		{ShardIndex: 0, DialogueID: "queue-demo-blocking", Task: func(ctx context.Context, buffer *lipsync.StateBuffer) (any, error) {
			_ = buffer.Push("running")
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, nil
		}},
	}
	for i := 1; i < 4; i++ {
		idx := i
		works = append(works, lipsync.ShardWork{
			ShardIndex: idx, DialogueID: "queue-demo",
			Task: func(ctx context.Context, buffer *lipsync.StateBuffer) (any, error) {
				_ = buffer.Push("queued-state")
				return idx, nil
			},
		})
	}
	runDone := make(chan *lipsync.PoolRun, 1)
	go func() {
		run, _ := lipsync.RunShardPool(context.Background(), opts, works)
		runDone <- run
	}()
	<-entered
	time.Sleep(20 * time.Millisecond)
	close(release)
	run := <-runDone
	s.mu.Lock()
	s.queueDemo = run.Events
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"events": run.Events, "options": opts})
}

func (s *Server) handleAcceptance(w http.ResponseWriter, r *http.Request) {
	report, err := lipsync.RunAcceptance(r.Context(), ".")
	s.mu.Lock()
	s.acceptance = report
	s.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	if domainErr, ok := lipsync.AsError(err); ok {
		status := http.StatusBadRequest
		if domainErr.Code == lipsync.ECStage || domainErr.Code == lipsync.ECDiff {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": domainErr})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": map[string]string{"code": "E_STAGE", "message": err.Error()}})
}
