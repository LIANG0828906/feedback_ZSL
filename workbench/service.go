package workbench

import (
	"context"
	"os"
	"path/filepath"
	"reflect"

	"chaosdiag/checkpoint"
	"chaosdiag/engine"
	"chaosdiag/gen"
	"chaosdiag/incremental"
	"chaosdiag/loader"
	"chaosdiag/model"
)

type Service struct {
	bundle        *gen.Bundle
	source        map[string]*model.Project
	engines       map[string]*incremental.Engine
	checkpointDir string
}

type StateResponse struct {
	Version  string           `json:"version"`
	Versions []VersionSummary `json:"versions"`
	Project  *model.Project   `json:"project"`
	Report   *model.Report    `json:"report"`
	Truth    *gen.Truth       `json:"truth"`
}

type VersionSummary struct {
	Version     string `json:"version"`
	Short       string `json:"short"`
	Description string `json:"description"`
}

type AnomalyRequest struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

type AnomalyResponse struct {
	OK        bool           `json:"ok"`
	Code      string         `json:"code,omitempty"`
	Field     string         `json:"field,omitempty"`
	Message   string         `json:"message,omitempty"`
	Context   map[string]any `json:"context,omitempty"`
	Available bool           `json:"workbench_still_available"`
}

type CheckpointDemo struct {
	Path             string                 `json:"path"`
	StopAfterTick    int                    `json:"stop_after_tick"`
	Partial          *checkpoint.Checkpoint `json:"partial"`
	ResumedReport    *model.Report          `json:"resumed_report"`
	FullResultsEqual bool                   `json:"full_results_equal"`
	RecoveryNote     string                 `json:"recovery_note"`
}

func NewService(checkpointDir string) (*Service, error) {
	bundle, err := gen.Generate()
	if err != nil {
		return nil, err
	}
	source := map[string]*model.Project{}
	engines := map[string]*incremental.Engine{}
	for version, doc := range bundle.Docs {
		p, err := loader.Load(doc)
		if err != nil {
			return nil, err
		}
		eng, err := incremental.NewEngine(p)
		if err != nil {
			return nil, err
		}
		source[version] = p
		engines[version] = eng
	}
	if checkpointDir == "" {
		checkpointDir = filepath.Join("artifacts", "checkpoints")
	}
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		return nil, err
	}
	return &Service{bundle: bundle, source: source, engines: engines, checkpointDir: checkpointDir}, nil
}

func NormalizeVersion(v string) string {
	switch v {
	case "", "v1", "1":
		return model.SchemaV1
	case "v2", "2":
		return model.SchemaV2
	case "v3", "3":
		return model.SchemaV3
	default:
		return v
	}
}

func (s *Service) Versions() []VersionSummary {
	return []VersionSummary{
		{Version: model.SchemaV1, Short: "v1", Description: "snake_case flat selectors; absolute ms/tick window endpoints"},
		{Version: model.SchemaV2, Short: "v2", Description: "camelCase selector table; tick amplitudes; relative duration windows"},
		{Version: model.SchemaV3, Short: "v3", Description: "nested service match tree; quantile amplitude manifests"},
	}
}

func (s *Service) State(version string) (*StateResponse, error) {
	v := NormalizeVersion(version)
	eng, ok := s.engines[v]
	if !ok {
		return nil, model.NewError(model.CodeSchemaUnsupported, "version", "version %q is not loaded", version)
	}
	return &StateResponse{
		Version: v, Versions: s.Versions(), Project: eng.Project(), Report: eng.Report(), Truth: s.bundle.Truth,
	}, nil
}

func (s *Service) Document(version string) ([]byte, error) {
	v := NormalizeVersion(version)
	doc, ok := s.bundle.Docs[v]
	if !ok {
		return nil, model.NewError(model.CodeSchemaUnsupported, "version", "no document for %q", version)
	}
	return doc, nil
}

func (s *Service) Edit(version string, mut incremental.Mutation) (*incremental.Result, error) {
	v := NormalizeVersion(version)
	eng, ok := s.engines[v]
	if !ok {
		return nil, model.NewError(model.CodeSchemaUnsupported, "version", "version %q is not loaded", version)
	}
	return eng.Apply(mut)
}

func (s *Service) Reset(version string) error {
	v := NormalizeVersion(version)
	p, ok := s.source[v]
	if !ok {
		return model.NewError(model.CodeSchemaUnsupported, "version", "version %q is not loaded", version)
	}
	eng, err := incremental.NewEngine(p)
	if err != nil {
		return err
	}
	s.engines[v] = eng
	return nil
}

func (s *Service) CheckpointDemo(version string) (*CheckpointDemo, error) {
	v := NormalizeVersion(version)
	p, ok := s.source[v]
	if !ok {
		return nil, model.NewError(model.CodeSchemaUnsupported, "version", "version %q is not loaded", version)
	}
	path := filepath.Join(s.checkpointDir, "recovery-"+filepath.Base(v)+".sqlite")
	_ = os.Remove(path)
	store, err := checkpoint.OpenStore(path)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if _, err := checkpoint.RunFresh(ctx, p, store, 10); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.Close(); err != nil {
		return nil, err
	}

	recovered, err := checkpoint.LoadExpected(path, p.ProjectVersion)
	if err != nil {
		return nil, err
	}
	store2, err := checkpoint.OpenStore(path)
	if err != nil {
		return nil, err
	}
	defer store2.Close()
	outcome, err := checkpoint.Resume(ctx, p, store2, *recovered, 0)
	if err != nil {
		return nil, err
	}
	full := engine.Run(p)
	equal := reflect.DeepEqual(outcome.Report.Results, full.Results)
	return &CheckpointDemo{
		Path: path, StopAfterTick: 10, Partial: recovered, ResumedReport: outcome.Report,
		FullResultsEqual: equal,
		RecoveryNote:     "checkpoint accepted matching project version and resumed after last_tick; mismatches trigger full replay",
	}, nil
}

func (s *Service) Anomaly(req AnomalyRequest) AnomalyResponse {
	ce := s.injectAnomaly(req)
	if ce == nil {
		return AnomalyResponse{OK: true, Available: true, Message: "anomaly accepted"}
	}
	return AnomalyResponse{
		OK: false, Code: ce.Code, Field: ce.Field, Message: ce.Message, Context: ce.Context, Available: true,
	}
}

func (s *Service) injectAnomaly(req AnomalyRequest) *model.Error {
	p, err := model.CloneProject(s.bundle.Canonical)
	if err != nil {
		return model.AsErrorOrInternal(err)
	}
	switch req.Type {
	case "cycle_dependency":
		p.Edges = append(p.Edges, model.Edge{ID: "edge-anomaly-cycle", From: "inventory", To: "order", LatencyMS: 1})
		return validateToError(p)
	case "missing_service_target":
		p.Instructions = append(p.Instructions, model.Instruction{
			ID: "I-ANOMALY-MISSING", Sequence: len(p.Instructions), SubmittedTick: 1, Kind: model.KindDelay,
			StartTick: 1, EndTick: 1, DelayMS: 5,
			Selector: model.Selector{Kind: model.TargetService, Service: "ghost-service"},
		})
		return validateToError(p)
	case "negative_delay":
		p.Instructions = append(p.Instructions, model.Instruction{
			ID: "I-ANOMALY-NEGATIVE", Sequence: len(p.Instructions), SubmittedTick: 1, Kind: model.KindDelay,
			StartTick: 1, EndTick: 1, DelayMS: -25,
			Selector: model.Selector{Kind: model.TargetService, Service: "auth"},
		})
		return validateToError(p)
	case "zero_timeout_budget":
		p.RequestTimeoutBudgetMS = 0
		return validateToError(p)
	case "concurrency_limit_out_of_range":
		p.Limits.GlobalConcurrent = model.MaxConcurrencyLimit + 1
		return validateToError(p)
	case "truncated_checkpoint":
		return s.truncatedCheckpointAnomaly()
	case "version_mismatch_snapshot":
		return s.versionMismatchAnomaly()
	default:
		return model.NewError(model.CodeFieldInvalid, "type", "unknown anomaly type %q", req.Type)
	}
}

func validateToError(p *model.Project) *model.Error {
	err := model.ValidateProject(p)
	if err == nil {
		return model.NewError(model.CodeInternal, "", "anomaly did not produce expected validation error")
	}
	ce, ok := model.AsError(err)
	if !ok {
		return model.NewError(model.CodeInternal, "", "%v", err)
	}
	return ce
}

func (s *Service) truncatedCheckpointAnomaly() *model.Error {
	path := filepath.Join(s.checkpointDir, "anomaly-truncated.sqlite")
	_ = os.Remove(path)
	store, err := checkpoint.OpenStore(path)
	if err != nil {
		ce, _ := model.AsError(err)
		return ce
	}
	if err := store.Save(checkpoint.Checkpoint{ProjectVersion: s.bundle.Canonical.ProjectVersion, LastTick: 7}); err != nil {
		_ = store.Close()
		ce, _ := model.AsError(err)
		return ce
	}
	_ = store.Close()
	if err := os.Truncate(path, 64); err != nil {
		return model.NewError(model.CodeInternal, "checkpoint.file", "truncate fixture: %v", err)
	}
	_, err = checkpoint.Load(path)
	ce, ok := model.AsError(err)
	if !ok {
		return model.NewError(model.CodeInternal, "checkpoint.file", "expected corrupt error, got %v", err)
	}
	return ce
}

func (s *Service) versionMismatchAnomaly() *model.Error {
	path := filepath.Join(s.checkpointDir, "anomaly-version-mismatch.sqlite")
	_ = os.Remove(path)
	store, err := checkpoint.OpenStore(path)
	if err != nil {
		ce, _ := model.AsError(err)
		return ce
	}
	if err := store.Save(checkpoint.Checkpoint{ProjectVersion: "snapshot-older-revision", LastTick: 4}); err != nil {
		_ = store.Close()
		ce, _ := model.AsError(err)
		return ce
	}
	_ = store.Close()
	_, err = checkpoint.LoadExpected(path, s.bundle.Canonical.ProjectVersion)
	ce, ok := model.AsError(err)
	if !ok {
		return model.NewError(model.CodeInternal, "checkpoint.project_version", "expected mismatch error, got %v", err)
	}
	return ce
}
