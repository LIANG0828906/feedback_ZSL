package workbench

import (
	"sync"

	"meshpolicy/internal/engine"
	"meshpolicy/internal/gen"
	"meshpolicy/internal/model"
	"meshpolicy/internal/schema"
	"meshpolicy/internal/store"
	"meshpolicy/internal/xerr"
)

type Service struct {
	mu      sync.Mutex
	seed    int64
	sample  *gen.Sample
	version string
	project *model.Project
	report  *engine.RunReport
	dbPath  string
}

func NewService(seed int64, dbPath string) (*Service, error) {
	if seed == 0 {
		seed = gen.DefaultSeed
	}
	sample := gen.Generate(seed)
	p, err := schema.Load(sample.Documents["v1"])
	if err != nil {
		return nil, err
	}
	report, err := engine.Run(p, engine.RunOptions{Workers: 4, BufferPerNS: 64})
	if err != nil {
		return nil, err
	}
	return &Service{
		seed: seed, sample: sample, version: "v1", project: p, report: report, dbPath: dbPath,
	}, nil
}

func (s *Service) DefaultRunOptions() engine.RunOptions {
	return engine.RunOptions{Workers: 4, BufferPerNS: 64}
}

type LoadResult struct {
	Version          string            `json:"version"`
	ProjectID        string            `json:"project_id"`
	CanonicalVersion string            `json:"canonical_version"`
	WorkloadCount    int               `json:"workload_count"`
	PolicyCount      int               `json:"policy_count"`
	EventCount       int               `json:"event_count"`
	AttemptCount     int               `json:"attempt_count"`
	Report           *engine.RunReport `json:"report"`
	Truth            gen.Truth         `json:"truth"`
	Overlaps         []engine.Overlap  `json:"overlaps"`
}

func (s *Service) LoadVersion(version string, opts engine.RunOptions) (*LoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.sample.Documents[version]
	if !ok {
		return nil, unknownVersion(version)
	}
	p, err := schema.Load(doc)
	if err != nil {
		return nil, err
	}
	opts = withDefaults(opts)
	report, err := engine.Run(p, opts)
	if err != nil {
		return nil, err
	}
	s.version = version
	s.project = p
	s.report = report
	return s.currentLoadResultLocked(report), nil
}

func (s *Service) Current() *LoadResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentLoadResultLocked(s.report)
}

func (s *Service) currentLoadResultLocked(report *engine.RunReport) *LoadResult {
	attempts := s.project.Attempts()
	return &LoadResult{
		Version: s.version, ProjectID: s.project.ID, CanonicalVersion: store.CanonicalVersion(s.project),
		WorkloadCount: len(s.project.Workloads), PolicyCount: len(s.project.Policies),
		EventCount: len(s.project.Timeline), AttemptCount: len(attempts), Report: report,
		Truth: s.sample.Truth, Overlaps: engine.AnalyzeOverlaps(s.project),
	}
}

func (s *Service) Run(opts engine.RunOptions) (*engine.RunReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts = withDefaults(opts)
	report, err := engine.Run(s.project, opts)
	if err != nil {
		return nil, err
	}
	s.report = report
	return report, nil
}

func (s *Service) Edit(req engine.EditRequest) (*engine.IncrementalReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts := s.DefaultRunOptions()
	report, edited, err := engine.IncrementalRecompute(s.project, req, opts)
	if err != nil {
		return nil, err
	}
	full, err := engine.Run(edited, opts)
	if err != nil {
		return nil, err
	}
	s.project = edited
	s.report = full
	return report, nil
}

func (s *Service) Replace(req engine.ReplaceRequest) (*engine.ReplacementResult, *engine.RunReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := model.CloneProject(s.project)
	result, err := engine.ReplacePolicy(candidate, req)
	if err != nil {
		return nil, nil, err
	}
	report, err := engine.Run(candidate, s.DefaultRunOptions())
	if err != nil {
		return nil, nil, err
	}
	s.project = candidate
	s.report = report
	return result, report, nil
}

func withDefaults(opts engine.RunOptions) engine.RunOptions {
	if opts.Workers == 0 {
		opts.Workers = 4
	}
	if opts.BufferPerNS == 0 {
		opts.BufferPerNS = 64
	}
	return opts
}

func unknownVersion(version string) error {
	return xerr.New(xerr.ERequestInvalid, "unknown schema version", map[string]any{"version": version, "allowed": []string{"v1", "v2", "v3"}})
}
