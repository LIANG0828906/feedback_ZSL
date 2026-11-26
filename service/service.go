// Package service is the application facade used by the diagnostic workbench
// and command line. It keeps generated source documents immutable and routes
// all edits through validated copies.
package service

import (
	"context"
	"path/filepath"
	"sync"

	"dnslab/checkpoint"
	"dnslab/codes"
	"dnslab/editor"
	"dnslab/engine"
	"dnslab/generator"
	"dnslab/loader"
	"dnslab/spec"
)

type Service struct {
	mu          sync.Mutex
	seed        int64
	bundle      *spec.Bundle
	schema      string
	loaded      *loader.LoadResult
	project     *spec.Project
	engine      *engine.Engine
	report      *engine.Report
	lastInvalid *engine.InvalidationReport
	store       *checkpoint.Store
	dbPath      string
}

func New(seed int64, dbPath string) (*Service, error) {
	bundle, err := generator.Generate(seed)
	if err != nil {
		return nil, err
	}
	s := &Service{seed: seed, bundle: bundle, dbPath: dbPath}
	if err := s.LoadSchema(spec.SchemaV3); err != nil {
		return nil, err
	}
	if dbPath != "" {
		store, err := checkpoint.Open(dbPath)
		if err != nil {
			return nil, err
		}
		s.store = store
	}
	return s, nil
}

func (s *Service) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Service) Schemas() []string {
	return []string{spec.SchemaV1, spec.SchemaV2, spec.SchemaV3}
}

func (s *Service) SourceDocument(schema string) ([]byte, error) {
	data, ok := s.bundle.Docs[schema]
	if !ok {
		return nil, codes.New(codes.ESchemaUnsupported, "unknown generated schema").WithContext("schema", schema)
	}
	return data, nil
}

func (s *Service) State() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"seed": s.seed, "schema": s.schema, "project": s.project,
		"queries": s.bundle.Queries, "truth": s.bundle.Truth,
		"has_report": s.report != nil, "last_invalidation": s.lastInvalid,
		"checkpoint_path": s.dbPath, "schemas": s.Schemas(),
	}
}

// LoadSchema reloads an immutable generated schema document and canonicalizes it.
func (s *Service) LoadSchema(schema string) error {
	data, ok := s.bundle.Docs[schema]
	if !ok {
		return codes.New(codes.ESchemaUnsupported, "schema document was not generated").
			WithContext("schema", schema)
	}
	loaded, err := loader.Load(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schema = schema
	s.loaded = loaded
	s.project = loaded.Project.Clone()
	s.engine = engine.NewEngine(loaded.Project)
	s.report = nil
	s.lastInvalid = nil
	return nil
}

type Mismatch struct {
	Sequence int    `json:"sequence"`
	QueryID  string `json:"query_id"`
	Reason   string `json:"reason"`
	GotCode  string `json:"got_code"`
	WantCode string `json:"want_code"`
}

type RunResponse struct {
	Schema       string         `json:"schema"`
	Report       *engine.Report `json:"report"`
	Truth        []spec.Truth   `json:"truth"`
	AllMatch     bool           `json:"all_match"`
	Matches      int            `json:"matches"`
	Mismatches   []Mismatch     `json:"mismatches"`
	SegmentCount int            `json:"segment_count"`
}

// RunAll resets to declared seed state and processes every sample once.
func (s *Service) RunAll(ctx context.Context, workers int) (*RunResponse, error) {
	s.mu.Lock()
	project := s.project
	s.mu.Unlock()

	if workers <= 0 {
		workers = project.WorkerBudget
	}
	fresh := engine.NewEngine(project)
	report, err := engine.RunSamples(ctx, fresh, s.bundle.Queries, workers)
	if err != nil {
		return nil, err
	}
	matches, mismatches := compareTruth(report.Results, s.bundle.Truth)

	s.mu.Lock()
	s.engine = fresh
	s.report = report
	s.mu.Unlock()

	return &RunResponse{
		Schema: s.schema, Report: report, Truth: s.bundle.Truth,
		AllMatch: len(mismatches) == 0, Matches: matches, Mismatches: mismatches,
		SegmentCount: len(fresh.SegmentSnapshots()),
	}, nil
}

func compareTruth(results []engine.Result, truth []spec.Truth) (int, []Mismatch) {
	var mismatches []Mismatch
	matches := 0
	for i, want := range truth {
		if i >= len(results) {
			mismatches = append(mismatches, Mismatch{Sequence: want.Sequence, QueryID: want.QueryID, Reason: "missing result", WantCode: want.Code})
			continue
		}
		got := results[i]
		if engine.CompareSemantic(got, want) {
			matches++
			continue
		}
		reason := "semantic fields differ"
		if got.Code != want.Code {
			reason = "code differs"
		} else if got.RuleNumber != want.RuleNumber {
			reason = "rule differs"
		} else if got.Source != want.Source {
			reason = "source differs"
		}
		mismatches = append(mismatches, Mismatch{
			Sequence: want.Sequence, QueryID: want.QueryID, Reason: reason,
			GotCode: got.Code, WantCode: want.Code,
		})
	}
	return matches, mismatches
}

// QuerySample runs one declared sample against the current mutable engine.
func (s *Service) QuerySample(sequence int) (engine.Result, spec.Truth, error) {
	if sequence < 1 || sequence > len(s.bundle.Queries) {
		return engine.Result{}, spec.Truth{}, codes.New(codes.EFieldInvalid, "query sequence out of range").
			WithContext("sequence", sequence).WithContext("max", len(s.bundle.Queries))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.bundle.Queries[sequence-1]
	return s.engine.ProcessQuery(q), s.bundle.Truth[sequence-1], nil
}

type EditResponse struct {
	Edit           editor.Edit                `json:"edit"`
	Report         *engine.InvalidationReport `json:"invalidation"`
	IncrementalRun *engine.Report             `json:"incremental_run"`
	FullRun        *engine.Report             `json:"full_run"`
	AllMatch       bool                       `json:"all_match"`
	Mismatches     []Mismatch                 `json:"mismatches"`
	Project        *spec.Project              `json:"project"`
}

// ApplyEdit performs validated copy-on-write editing and compares incremental
// execution with a one-shot full rebuild.
func (s *Service) ApplyEdit(edit editor.Edit) (*EditResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next, err := editor.Apply(s.project, edit)
	if err != nil {
		return nil, err
	}

	// Stateful reconciliation reports reuse of the live runtime segments.
	stateful, report := engine.Reconcile(s.engine, next, s.schema, s.schema, nil)
	report.AnnotateQueries(stateful, s.bundle.Queries)

	// Separately prove incremental seed rebuild equals one-shot full rebuild,
	// avoiding source changes caused by intentionally retained runtime cache.
	proofSeed := engine.NewEngine(s.project)
	proofEngine, _ := engine.Reconcile(proofSeed, next, s.schema, s.schema, nil)
	incReport, err := engine.RunSamples(context.Background(), proofEngine, s.bundle.Queries, 1)
	if err != nil {
		return nil, err
	}
	fullEngine := engine.NewEngine(next)
	fullReport, err := engine.RunSamples(context.Background(), fullEngine, s.bundle.Queries, 1)
	if err != nil {
		return nil, err
	}
	_, mismatches := compareTruth(incReport.Results, truthFromResults(fullReport.Results, s.bundle.Truth))
	allMatch := resultsSemanticEqual(incReport.Results, fullReport.Results)
	if !allMatch {
		mismatches = append(mismatches, Mismatch{Reason: "incremental and full recompute differ"})
	}

	s.project = next
	s.engine = stateful
	s.lastInvalid = report
	s.report = incReport

	return &EditResponse{
		Edit: edit, Report: report, IncrementalRun: incReport, FullRun: fullReport,
		AllMatch: allMatch && len(mismatches) == 0, Mismatches: mismatches, Project: next,
	}, nil
}

func truthFromResults(results []engine.Result, fallback []spec.Truth) []spec.Truth {
	out := make([]spec.Truth, len(results))
	for i, r := range results {
		t := spec.Truth{
			Sequence: r.Sequence, QueryID: r.QueryID, Code: r.Code, Matched: r.Matched,
			RuleNumber: r.RuleNumber, RuleOrdinal: r.RuleOrdinal, DecisionBasis: r.DecisionBasis,
			CandidateRules: r.CandidateRules, UpstreamIndex: r.UpstreamIndex, HopCount: r.HopCount,
			Source: r.Source, Negative: r.Negative, Records: r.Records,
			RemainingTTLMS: r.RemainingTTLMS, EntryRemainingMS: r.EntryRemainingMS,
			VirtualElapsedMS: r.VirtualElapsedMS,
		}
		if i < len(fallback) {
			t.Sequence = fallback[i].Sequence
		}
		out[i] = t
	}
	return out
}

func resultsSemanticEqual(a, b []engine.Result) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ta := spec.Truth{
			Code: a[i].Code, Matched: a[i].Matched, RuleNumber: a[i].RuleNumber,
			RuleOrdinal: a[i].RuleOrdinal, DecisionBasis: a[i].DecisionBasis,
			CandidateRules: a[i].CandidateRules, UpstreamIndex: a[i].UpstreamIndex,
			HopCount: a[i].HopCount, Source: a[i].Source, Negative: a[i].Negative,
			Records: a[i].Records, RemainingTTLMS: a[i].RemainingTTLMS,
			EntryRemainingMS: a[i].EntryRemainingMS, VirtualElapsedMS: a[i].VirtualElapsedMS,
		}
		if !engine.CompareSemantic(b[i], ta) {
			return false
		}
	}
	return true
}

func (s *Service) Inject(req editor.InjectionRequest) error {
	s.mu.Lock()
	p := s.project
	dbPath := s.dbPath
	s.mu.Unlock()
	if req.Kind == "truncated_checkpoint" {
		return injectTruncatedCheckpoint(dbPath)
	}
	if req.Kind == "version_mismatch_checkpoint" {
		return injectMismatchCheckpoint(dbPath, p.Version)
	}
	return editor.Inject(p, req)
}

func injectTruncatedCheckpoint(mainPath string) error {
	if mainPath == "" {
		return codes.New(codes.ECheckpointNotConfigured, "checkpoint database path is not configured")
	}
	path := filepath.Join(filepath.Dir(mainPath), "inject-truncated.sqlite")
	store, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	if err := store.Save(context.Background(), checkpoint.Checkpoint{
		ProjectVersion: "injection-fixture", CommittedSeq: 1,
		Segments: map[string]engine.Segment{},
	}); err != nil {
		_ = store.Close()
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}
	if err := checkpoint.TruncateForInjection(path, 48); err != nil {
		return err
	}
	_, err = checkpoint.Open(path)
	return err
}

func injectMismatchCheckpoint(mainPath, expectedVersion string) error {
	if mainPath == "" {
		return codes.New(codes.ECheckpointNotConfigured, "checkpoint database path is not configured")
	}
	path := filepath.Join(filepath.Dir(mainPath), "inject-mismatch.sqlite")
	store, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Save(context.Background(), checkpoint.Checkpoint{
		ProjectVersion: "snapshot-version-does-not-match",
		SchemaVersion:  spec.SchemaV1,
		CommittedSeq:   0,
		Segments:       map[string]engine.Segment{},
	}); err != nil {
		return err
	}
	_, err = store.LoadRestored(context.Background(), expectedVersion)
	return err
}
