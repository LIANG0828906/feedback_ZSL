// Package server serves the local deterministic diagnostic page and JSON API.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"roomacoustics/pkg/acoustics"
	"roomacoustics/pkg/checkpoint"
	"roomacoustics/pkg/engine"
	"roomacoustics/pkg/generator"
	"roomacoustics/pkg/pipeline"
)

type Service struct {
	mu               sync.Mutex
	root             string
	level            *acoustics.Level
	truth            *acoustics.TruthFile
	report           *acoustics.Report
	store            *checkpoint.Store
	pool             *acoustics.Pool
	dbPath           string
	lastInvalidation *acoustics.InvalidationReport
}

func NewService(root string) (*Service, error) {
	if err := os.MkdirAll(filepath.Join(root, "samples"), 0o755); err != nil {
		return nil, err
	}
	sampleDir := filepath.Join(root, "samples")
	if _, err := generator.GenerateFiles(sampleDir, generator.DefaultSeed); err != nil {
		return nil, err
	}
	level, truth, err := acoustics.LoadProjectFiles(filepath.Join(sampleDir, "level.json"), filepath.Join(sampleDir, "truth.json"))
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(root, "samples", "local-server.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	pool := acoustics.NewPool(2, 1)
	s := &Service{root: root, level: level, truth: truth, store: store, pool: pool, dbPath: dbPath}
	eng, err := engine.New(level, engine.Config{Store: store, Pool: pool})
	if err != nil {
		return nil, err
	}
	report, err := eng.Run(context.Background())
	if err != nil {
		return nil, err
	}
	if vr, verr := acoustics.VerifyTruth(level, report, truth); verr == nil {
		report.Verification = vr
	} else {
		report.Verification = vr
	}
	s.report = report
	return s, nil
}

func (s *Service) Close() error {
	s.pool.Close()
	return s.store.Close()
}

type stateResponse struct {
	OK           bool                          `json:"ok"`
	Level        *acoustics.Level              `json:"level"`
	Truth        *acoustics.TruthFile          `json:"truth"`
	Report       *acoustics.Report             `json:"report"`
	Invalidation *acoustics.InvalidationReport `json:"invalidation,omitempty"`
	DBPath       string                        `json:"db_path"`
}

func (s *Service) State() stateResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return stateResponse{OK: true, Level: s.level, Truth: s.truth, Report: s.report, Invalidation: s.lastInvalidation, DBPath: s.dbPath}
}

func (s *Service) ApplyEdit(edit acoustics.Edit) (stateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := acoustics.ApplyEdit(s.level, edit)
	if err != nil {
		return stateResponse{}, err
	}
	ctx := context.Background()
	persisted, err := pipeline.LoadReportFromStore(ctx, s.store)
	if err != nil {
		return stateResponse{}, err
	}
	plan := acoustics.PlanInvalidation(s.level, next, persisted)
	eng, err := engine.New(next, engine.Config{Store: s.store, Pool: s.pool, ReuseReport: persisted, Invalidation: plan})
	if err != nil {
		return stateResponse{}, err
	}
	report, err := eng.Run(ctx)
	if err != nil {
		return stateResponse{}, err
	}
	s.level = next
	s.report = report
	s.lastInvalidation = plan
	return stateResponse{OK: true, Level: s.level, Truth: s.truth, Report: s.report, Invalidation: plan, DBPath: s.dbPath}, nil
}

func (s *Service) Reset() (stateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.store.Close()
	_ = os.Remove(s.dbPath)
	_ = os.Remove(s.dbPath + "-wal")
	_ = os.Remove(s.dbPath + "-shm")
	spec, err := generator.Generate(s.level.Seed)
	if err != nil {
		return stateResponse{}, err
	}
	store, err := checkpoint.Open(s.dbPath)
	if err != nil {
		return stateResponse{}, err
	}
	s.store = store
	s.level = spec.Level
	s.truth = spec.Truth
	s.lastInvalidation = nil
	eng, err := engine.New(s.level, engine.Config{Store: store, Pool: s.pool})
	if err != nil {
		return stateResponse{}, err
	}
	report, err := eng.Run(context.Background())
	if err != nil {
		return stateResponse{}, err
	}
	if vr, verr := acoustics.VerifyTruth(s.level, report, s.truth); verr == nil {
		report.Verification = vr
	} else {
		report.Verification = vr
	}
	s.report = report
	return stateResponse{OK: true, Level: s.level, Truth: s.truth, Report: s.report, DBPath: s.dbPath}, nil
}

func (s *Service) Acceptance(root string) (*pipeline.AcceptanceReport, error) {
	return pipeline.RunAcceptance(context.Background(), root)
}

func ListenAndServe(ctx context.Context, addr, root string) error {
	svc, err := NewService(root)
	if err != nil {
		return err
	}
	defer svc.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, svc.State())
	})
	mux.HandleFunc("/api/edit", func(w http.ResponseWriter, r *http.Request) {
		var edit acoustics.Edit
		if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
			writeError(w, acoustics.Errorf(acoustics.CodeSchema, "request", "json", err.Error()))
			return
		}
		resp, err := svc.ApplyEdit(edit)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		resp, err := svc.Reset()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/api/acceptance", func(w http.ResponseWriter, r *http.Request) {
		report, err := svc.Acceptance(root)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, report)
	})
	mux.Handle("/", http.FileServer(http.FS(webFS())))

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	de, ok := acoustics.AsDomainError(err)
	if !ok {
		de = &acoustics.DomainError{Code: "E_INTERNAL", Message: err.Error()}
	}
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": de})
}
