package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/generator"
	"mvccworkbench/internal/schema"
)

type Service struct {
	Suite *generator.Suite
	Docs  map[string]map[string][]byte
}

type Mismatch struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
}

type VerifyReport struct {
	BatchID      string            `json:"batch_id"`
	Schema       string            `json:"schema"`
	Execution    string            `json:"execution"`
	OK           bool              `json:"ok"`
	ReadCount    int               `json:"read_count"`
	WriteCount   int               `json:"write_count"`
	CommitCount  int               `json:"commit_count"`
	GCCount      int               `json:"gc_count"`
	Mismatches   []Mismatch        `json:"mismatches"`
	Metrics      map[string]string `json:"metrics,omitempty"`
	WorkerBudget int               `json:"worker_budget,omitempty"`
	BufferLimit  int               `json:"buffer_limit,omitempty"`
}

func NewService(seed int64) (*Service, error) {
	suite, err := generator.DefaultSuite(seed)
	if err != nil {
		return nil, err
	}
	docs := map[string]map[string][]byte{}
	for _, batchID := range suite.Order {
		project := suite.Batches[batchID].Project
		docs[batchID] = map[string][]byte{}
		for _, version := range []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
			data, err := schema.Encode(version, project)
			if err != nil {
				return nil, err
			}
			docs[batchID][version] = data
		}
	}
	return &Service{Suite: suite, Docs: docs}, nil
}

func (s *Service) BatchIDs() []string { return append([]string{}, s.Suite.Order...) }

func (s *Service) Project(batchID, schemaVersion string) (*domain.Project, string, error) {
	batch, ok := s.Suite.Batches[batchID]
	if !ok {
		return nil, "", domain.NewError(domain.EInputInvalidRef, "unknown batch").With("batch_id", batchID).With("supported", s.Suite.Order)
	}
	if schemaVersion == "" || schemaVersion == "canonical" {
		return batch.Project, "canonical", nil
	}
	data, ok := s.Docs[batchID][schemaVersion]
	if !ok {
		return nil, schemaVersion, domain.NewError(domain.ESchemaUnknown, "schema not generated for batch").
			With("batch_id", batchID).With("schema_version", schemaVersion)
	}
	project, detected, err := schema.LoadDocument(data)
	return project, detected, err
}

func (s *Service) Verify(ctx context.Context, batchID, schemaVersion string, parallel bool, opts engine.Options) (*VerifyReport, error) {
	project, resolvedSchema, err := s.Project(batchID, schemaVersion)
	if err != nil {
		return nil, err
	}
	batch := s.Suite.Batches[batchID]
	execution := "sequential"
	var report *engine.Report
	if parallel {
		execution = "parallel"
		report, err = engine.RunParallel(ctx, project, opts)
	} else {
		report, err = engine.Run(ctx, project, opts)
	}
	if err != nil {
		return nil, err
	}

	result := &VerifyReport{
		BatchID:      batchID,
		Schema:       resolvedSchema,
		Execution:    execution,
		OK:           true,
		ReadCount:    len(report.Reads),
		WriteCount:   len(report.Writes),
		CommitCount:  len(report.Commits),
		GCCount:      len(report.GC),
		Mismatches:   []Mismatch{},
		WorkerBudget: opts.Workers,
		BufferLimit:  opts.ItemBufferLimit,
		Metrics:      map[string]string{},
	}
	for itemID, elapsed := range report.Metrics {
		result.Metrics[itemID] = elapsed.String()
	}

	for _, got := range report.Reads {
		want := batch.Truth.Reads[got.OpID]
		if !reflect.DeepEqual(got, want) {
			result.OK = false
			result.Mismatches = append(result.Mismatches, mismatch("read", got.OpID, want, got))
		}
	}
	for _, got := range report.Writes {
		want := batch.Truth.Writes[got.OpID]
		if !reflect.DeepEqual(got, want) {
			result.OK = false
			result.Mismatches = append(result.Mismatches, mismatch("write", got.OpID, want, got))
		}
	}
	wantCommits := batch.Truth.CommitList()
	for i, got := range report.Commits {
		if i >= len(wantCommits) || !reflect.DeepEqual(got, wantCommits[i]) {
			id := ""
			if i < len(wantCommits) {
				id = fmt.Sprint(wantCommits[i].TxnID)
			}
			result.OK = false
			result.Mismatches = append(result.Mismatches, mismatch("commit", id, safeIndex(wantCommits, i), got))
		}
	}
	for i, got := range report.GC {
		if i >= len(batch.Truth.GC) || !reflect.DeepEqual(got, batch.Truth.GC[i]) {
			result.OK = false
			id := got.StepID
			var want any
			if i < len(batch.Truth.GC) {
				want = batch.Truth.GC[i]
			}
			result.Mismatches = append(result.Mismatches, mismatch("gc", id, want, got))
		}
	}
	return result, nil
}

func mismatch(kind, id string, want, got any) Mismatch {
	expected, _ := json.MarshalIndent(want, "", "  ")
	actual, _ := json.MarshalIndent(got, "", "  ")
	return Mismatch{Kind: kind, ID: id, Expected: string(expected), Got: string(actual)}
}

func safeIndex(list []domain.CommitDecision, i int) any {
	if i < len(list) {
		return list[i]
	}
	return nil
}
