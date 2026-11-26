package service

import (
	"context"

	"dnslab/editor"
	"dnslab/engine"
	"dnslab/loader"
)

// ReplaceSnapshot loads a complete replacement document and reconciles it with
// the current canonical project. Schema detection happens in loader.Load before
// invalidation scope is calculated.
func (s *Service) ReplaceSnapshot(data []byte) (*EditResponse, error) {
	loaded, err := loader.Load(data)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stateful, report := engine.Reconcile(s.engine, loaded.Project, s.schema, loaded.SchemaVersion, nil)
	report.AnnotateQueries(stateful, s.bundle.Queries)

	proofSeed := engine.NewEngine(s.project)
	proofEngine, _ := engine.Reconcile(proofSeed, loaded.Project, s.schema, loaded.SchemaVersion, nil)
	incReport, err := engine.RunSamples(context.Background(), proofEngine, s.bundle.Queries, 1)
	if err != nil {
		return nil, err
	}
	fullEngine := engine.NewEngine(loaded.Project)
	fullReport, err := engine.RunSamples(context.Background(), fullEngine, s.bundle.Queries, 1)
	if err != nil {
		return nil, err
	}
	allMatch := resultsSemanticEqual(incReport.Results, fullReport.Results)

	s.schema = loaded.SchemaVersion
	s.loaded = loaded
	s.project = loaded.Project.Clone()
	s.engine = stateful
	s.lastInvalid = report
	s.report = incReport

	return &EditResponse{
		Edit:           editor.Edit{Kind: "snapshot_replace"},
		Report:         report,
		IncrementalRun: incReport,
		FullRun:        fullReport,
		AllMatch:       allMatch,
		Project:        loaded.Project,
	}, nil
}

// ReplaceGeneratedSchema replaces the current snapshot with a generated schema.
func (s *Service) ReplaceGeneratedSchema(schema string) (*EditResponse, error) {
	data, err := s.SourceDocument(schema)
	if err != nil {
		return nil, err
	}
	return s.ReplaceSnapshot(data)
}
