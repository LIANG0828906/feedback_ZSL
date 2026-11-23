package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"inputkernel/internal/kernel"
	"inputkernel/internal/model"
	"inputkernel/internal/sampleio"
)

type Options struct {
	SamplesDir string
	StoreDir   string
	ReportPath string
	Workers    int
	Buffer     int
}

type Report struct {
	Seed       int64            `json:"seed"`
	SamplesDir string           `json:"samples_dir"`
	Status     string           `json:"status"`
	Scenarios  []ScenarioReport `json:"scenarios"`
}

type ScenarioReport struct {
	ID          string                        `json:"id"`
	Name        string                        `json:"name"`
	Schema      string                        `json:"schema"`
	Truth       kernel.TruthResult            `json:"truth"`
	FullReplay  ReplaySummary                 `json:"full_replay"`
	Snapshot    SnapshotReport                `json:"snapshot"`
	Incremental IncrementalReport             `json:"incremental"`
	Errors      kernel.ErrorPropagationReport `json:"error_propagation"`
	Unmapped    []model.TruthUnmapped         `json:"unmapped"`
	Workers     kernel.PoolStats              `json:"worker_pool"`
}

type ReplaySummary struct {
	Actions      int `json:"actions"`
	AxisSamples  int `json:"axis_samples"`
	TimingChecks int `json:"timing_checks"`
	Unmapped     int `json:"unmapped"`
}

type SnapshotReport struct {
	Path          string `json:"path"`
	Saved         bool   `json:"saved"`
	Restored      bool   `json:"restored"`
	RestoreEqual  bool   `json:"restore_equal"`
	StaleCode     string `json:"stale_code,omitempty"`
	StaleDetail   string `json:"stale_detail,omitempty"`
	FullRecovery  bool   `json:"full_recovery"`
	RecoveryEqual bool   `json:"recovery_equal"`
}

type IncrementalReport struct {
	Edit         string                    `json:"edit"`
	Invalidation kernel.InvalidationReport `json:"invalidation"`
	Comparison   kernel.CompareResult      `json:"comparison"`
	ErrorCode    string                    `json:"error_code,omitempty"`
}

func Execute(ctx context.Context, options Options) (*Report, error) {
	if options.Workers <= 0 {
		options.Workers = 4
	}
	if options.Buffer <= 0 {
		options.Buffer = 8
	}
	set, err := sampleio.Load(options.SamplesDir)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Seed: set.Manifest.Seed, SamplesDir: options.SamplesDir, Status: "ok",
	}
	store := kernel.NewSnapshotStore(options.StoreDir)
	for _, scenario := range set.Scenarios {
		scenarioReport, err := runScenario(ctx, scenario, store, options)
		if err != nil {
			scenarioReport.ID = scenario.ID
			scenarioReport.Name = scenario.Name
			report.Status = "error"
			report.Scenarios = append(report.Scenarios, scenarioReport)
			writeReportIfRequested(options.ReportPath, report)
			return report, err
		}
		report.Scenarios = append(report.Scenarios, scenarioReport)
		if !scenarioReport.Truth.Pass || !scenarioReport.Incremental.Comparison.Equal ||
			!scenarioReport.Snapshot.RestoreEqual || !scenarioReport.Snapshot.RecoveryEqual {
			report.Status = "error"
			writeReportIfRequested(options.ReportPath, report)
			return report, errors.New("acceptance comparison failed for scenario " + scenario.ID)
		}
	}
	if err := writeReportIfRequested(options.ReportPath, report); err != nil {
		return report, err
	}
	return report, nil
}

func runScenario(ctx context.Context, scenario *model.Scenario, store *kernel.SnapshotStore, options Options) (ScenarioReport, error) {
	result := ScenarioReport{ID: scenario.ID, Name: scenario.Name, Schema: "valid"}
	full, err := kernel.NewRuntime(ctx, scenario, kernel.Options{MaxWorkers: options.Workers, PerWorkerBuffer: options.Buffer})
	if err != nil {
		return result, err
	}
	defer full.Close()
	if _, err := full.RecomputeAll(); err != nil {
		return result, err
	}
	result.Truth = full.CompareTruth(kernel.DefaultTruthTolerance())
	if !result.Truth.Pass {
		return result, fmt.Errorf("truth mismatch: event %d field %s", result.Truth.FirstMismatch.EventSeq, result.Truth.FirstMismatch.Field)
	}
	result.FullReplay = ReplaySummary{
		Actions: len(full.ActionOutputs()), AxisSamples: len(full.AxisSamples()),
		TimingChecks: len(full.TimingChecks()), Unmapped: len(full.UnmappedInputs()),
	}
	result.Errors = full.ErrorReport()
	result.Unmapped = full.UnmappedInputs()
	result.Workers = full.PoolStats()

	snapshot, err := full.MakeSnapshot()
	if err != nil {
		return result, err
	}
	path, err := store.Save(snapshot)
	if err != nil {
		return result, err
	}
	result.Snapshot.Path = path
	result.Snapshot.Saved = true
	loaded, err := store.Load(scenario.ID)
	if err != nil {
		return result, err
	}
	restored, err := kernel.NewRuntime(ctx, scenario, kernel.Options{MaxWorkers: options.Workers, PerWorkerBuffer: options.Buffer})
	if err != nil {
		return result, err
	}
	defer restored.Close()
	if err := restored.RestoreSnapshot(loaded); err != nil {
		return result, err
	}
	result.Snapshot.Restored = true
	restoreComparison, err := kernel.CompareRuntimesChecked(full, restored)
	if err != nil {
		return result, err
	}
	result.Snapshot.RestoreEqual = restoreComparison.Equal

	stale := *loaded
	stale.BindingVersion = "stale-binding-version"
	recovery, err := kernel.NewRuntime(ctx, scenario, kernel.Options{MaxWorkers: options.Workers, PerWorkerBuffer: options.Buffer})
	if err != nil {
		return result, err
	}
	defer recovery.Close()
	if err := recovery.ValidateSnapshot(&stale); err != nil {
		var kernelErr *model.Error
		if errors.As(err, &kernelErr) && kernelErr.Code == model.CodeVer {
			result.Snapshot.StaleCode = kernelErr.Code
			result.Snapshot.StaleDetail = kernelErr.Detail
		} else {
			return result, err
		}
	} else {
		return result, errors.New("expected stale snapshot to return E_VER")
	}
	if _, err := recovery.RecomputeAll(); err != nil {
		return result, err
	}
	result.Snapshot.FullRecovery = true
	recoveryComparison, err := kernel.CompareRuntimesChecked(full, recovery)
	if err != nil {
		return result, err
	}
	result.Snapshot.RecoveryEqual = recoveryComparison.Equal

	incremental, err := kernel.NewRuntime(ctx, scenario, kernel.Options{MaxWorkers: options.Workers, PerWorkerBuffer: options.Buffer})
	if err != nil {
		return result, err
	}
	defer incremental.Close()
	if err := incremental.RestoreSnapshot(loaded); err != nil {
		return result, err
	}
	invalidation, err := incremental.EditAndRecompute(func(s *model.Scenario) error {
		s.Curves[0].CurvaturePPM++
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Incremental.Edit = "curves[0].curvature_ppm += 1"
	result.Incremental.Invalidation = invalidation
	editedFull, err := kernel.NewRuntime(ctx, incremental.Scenario(), kernel.Options{MaxWorkers: options.Workers, PerWorkerBuffer: options.Buffer})
	if err != nil {
		return result, err
	}
	defer editedFull.Close()
	if _, err := editedFull.RecomputeAll(); err != nil {
		return result, err
	}
	comparison, cmpErr := kernel.CompareRuntimesChecked(incremental, editedFull)
	result.Incremental.Comparison = comparison
	if cmpErr != nil {
		result.Incremental.ErrorCode = model.CodeDiff
		return result, cmpErr
	}
	return result, nil
}

func writeReportIfRequested(path string, report *Report) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, report)
}

func writeJSON(path string, value interface{}) error {
	data, err := jsonMarshalIndent(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
