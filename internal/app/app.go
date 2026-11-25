package app

import (
	"os"
	"path/filepath"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/checkpoint"
	"tlscheck/internal/engine"
	"tlscheck/internal/incremental"
	"tlscheck/internal/model"
	"tlscheck/internal/projectio"
	"tlscheck/internal/runtime"
	"tlscheck/internal/sample"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type Summary struct {
	OK                   bool              `json:"ok"`
	ProjectID            string            `json:"project_id"`
	SchemaHashes         map[string]string `json:"schema_hashes"`
	EndpointCount        int               `json:"endpoint_count"`
	ChainCount           int               `json:"chain_count"`
	EventCount           int               `json:"event_count"`
	ResultCount          int               `json:"result_count"`
	StatusCounts         map[string]int    `json:"status_counts"`
	QueueCount           int               `json:"queue_count"`
	BackpressureCount    int               `json:"backpressure_count"`
	PeakWorkers          int               `json:"peak_workers"`
	CheckpointPath       string            `json:"checkpoint_path,omitempty"`
	CheckpointSequence   int               `json:"checkpoint_sequence,omitempty"`
	RecoveryOutcomeEqual bool              `json:"recovery_outcome_equal"`
	IncrementalEqual     bool              `json:"incremental_equal"`
	SamplesDir           string            `json:"samples_dir,omitempty"`
	Checks               []Check           `json:"checks,omitempty"`
}

func GenerateSamples(dir string) (*sample.Bundle, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, bizerr.New(bizerr.EInvalidArgument, err.Error()).WithContext("dir", dir)
	}
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"project.v1.json": b.SchemaV1,
		"project.v2.json": b.SchemaV2,
		"project.v3.json": b.SchemaV3,
		"truth.json":      b.TruthJSON,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return nil, bizerr.New(bizerr.EInvalidArgument, err.Error()).WithContext("file", name)
		}
	}
	return b, nil
}

func RunDemo(samplesDir, dbPath string) (*Summary, error) {
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		return nil, err
	}
	summary := &Summary{
		ProjectID: b.Project.ProjectID, SchemaHashes: map[string]string{},
		EndpointCount: len(b.Project.Endpoints), ChainCount: len(b.Project.Chains),
		EventCount: len(b.Project.Events), StatusCounts: map[string]int{},
	}
	if samplesDir != "" {
		if _, err := GenerateSamples(samplesDir); err != nil {
			return nil, err
		}
		summary.SamplesDir = samplesDir
	}
	for schema, data := range map[string][]byte{
		model.SchemaV1: b.SchemaV1, model.SchemaV2: b.SchemaV2, model.SchemaV3: b.SchemaV3,
	} {
		p, err := projectio.Load(data)
		if err != nil {
			return nil, err
		}
		h, err := p.CanonicalHash()
		if err != nil {
			return nil, err
		}
		summary.SchemaHashes[schema] = h[:16]
	}

	order := make([]string, 0, len(b.Project.Endpoints))
	for _, ep := range b.Project.Endpoints {
		order = append(order, ep.ID)
	}
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	report, err := runtime.RunParallel(b.Project, runtime.Config{
		Workers: b.Project.ThreadBudget, EndpointOrder: order,
	})
	if err != nil {
		return nil, err
	}
	fillReportSummary(summary, report)

	if dbPath != "" {
		if dir := filepath.Dir(dbPath); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, bizerr.New(bizerr.EInvalidArgument, err.Error()).WithContext("dir", dir)
			}
		}
		store, err := checkpoint.Open(dbPath)
		if err != nil {
			return nil, err
		}
		cp := checkpoint.FromReport(b.Project, report, 1)
		if err := store.Save(b.Project, cp, checkpoint.SaveOptions{}); err != nil {
			_ = store.Close()
			return nil, err
		}
		summary.CheckpointPath = dbPath
		summary.CheckpointSequence = cp.GlobalSequence
		if err := store.Close(); err != nil {
			return nil, err
		}

		reopen, err := checkpoint.Open(dbPath)
		if err != nil {
			return nil, err
		}
		loaded, err := reopen.LoadForProject(b.Project)
		if err != nil {
			_ = reopen.Close()
			return nil, err
		}
		initialResults := map[string][]*engine.RequestResult{}
		for _, rr := range loaded.Results {
			initialResults[rr.EndpointID] = append(initialResults[rr.EndpointID], rr)
		}
		recovered, err := engine.Run(b.Project, engine.Options{
			InitialStates: loaded.States, InitialResults: initialResults,
		})
		_ = reopen.Close()
		if err != nil {
			return nil, err
		}
		equal, err := incremental.OutcomesEqual(recovered, b.Truth)
		if err != nil {
			return nil, err
		}
		summary.RecoveryOutcomeEqual = equal
	}

	mgr, err := incremental.New(b.Project)
	if err != nil {
		return nil, err
	}
	change, err := mgr.AppendRequest(3000, model.Request{
		ID: "CLI-INCREMENTAL-PROBE", EndpointID: "alpha-ok", OfferedProtocol: "TLS1.3",
		OfferedCiphers: []string{"TLS_AES_256_GCM_SHA384"}, DurationMs: 1000,
	})
	if err != nil {
		return nil, err
	}
	inc, err := mgr.Refresh(change)
	if err != nil {
		return nil, err
	}
	full, err := engine.Run(mgr.Project())
	if err != nil {
		return nil, err
	}
	summary.IncrementalEqual, err = incremental.JudgmentsEqual(inc, full)
	if err != nil {
		return nil, err
	}
	summary.OK = true
	return summary, nil
}

func Verify() (*Summary, error) {
	tmp, err := os.MkdirTemp("", "tlscheck-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	summary, err := RunDemo(filepath.Join(tmp, "samples"), filepath.Join(tmp, "verify.db"))
	if err != nil {
		return nil, err
	}
	checks := []Check{
		{Name: "three_schema_hashes", OK: summary.SchemaHashes[model.SchemaV1] == summary.SchemaHashes[model.SchemaV2] &&
			summary.SchemaHashes[model.SchemaV2] == summary.SchemaHashes[model.SchemaV3]},
		{Name: "checkpoint_recovery", OK: summary.RecoveryOutcomeEqual},
		{Name: "incremental_recompute", OK: summary.IncrementalEqual},
		{Name: "buffer_backpressure", OK: summary.BackpressureCount > 0},
	}
	if _, err := projectio.Load([]byte(`{"schema":"unknown"}`)); bizerr.CodeOf(err) != bizerr.ESchemaUnsupported {
		checks = append(checks, Check{Name: "business_error_codes", OK: false, Detail: "schema code mismatch"})
	} else if _, err := projectio.ScaledToBP(1, 7); bizerr.CodeOf(err) != bizerr.EBudgetScaleUnsupported {
		checks = append(checks, Check{Name: "business_error_codes", OK: false, Detail: "budget code mismatch"})
	} else {
		checks = append(checks, Check{Name: "business_error_codes", OK: true})
	}
	for _, c := range checks {
		if !c.OK {
			summary.OK = false
		}
		summary.Checks = append(summary.Checks, c)
	}
	return summary, nil
}

func fillReportSummary(s *Summary, r *engine.Report) {
	s.ResultCount = len(r.Results)
	s.QueueCount = len(r.QueueRecords)
	s.BackpressureCount = r.Diagnostics.BackpressureCount
	s.PeakWorkers = r.Diagnostics.PeakWorkers
	for _, rr := range r.Results {
		s.StatusCounts[rr.Status]++
	}
}
