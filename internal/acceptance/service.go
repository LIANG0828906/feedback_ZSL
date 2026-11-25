package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"channelpipe"
	"channelpipe/internal/checkpoint"
	"channelpipe/internal/independent"
	"channelpipe/internal/runner"
	"channelpipe/internal/sample"
)

type Config struct {
	OutputDir  string
	Tiny       bool
	Workers    int
	TileWidth  int
	TileHeight int
	KeepDB     bool
}

type LoaderCheck struct {
	Version      int    `json:"version"`
	File         string `json:"file"`
	VersionField string `json:"version_field"`
	TraceCount   int    `json:"trace_count"`
	SemanticOK   bool   `json:"semantic_ok"`
	SemanticDiff string `json:"semantic_diff,omitempty"`
}

type NegativeCheck struct {
	Name         string `json:"name"`
	ExpectedCode string `json:"expected_code"`
	ActualCode   string `json:"actual_code"`
	Field        string `json:"field,omitempty"`
	OK           bool   `json:"ok"`
}

type Report struct {
	OK                bool                               `json:"ok"`
	StartedAt         time.Time                          `json:"started_at"`
	FinishedAt        time.Time                          `json:"finished_at"`
	OutputDir         string                             `json:"output_dir"`
	SampleDir         string                             `json:"sample_dir"`
	MainDB            string                             `json:"main_db"`
	LoaderChecks      []LoaderCheck                      `json:"loader_checks"`
	NegativeChecks    []NegativeCheck                    `json:"negative_checks"`
	UpgradeTraces     []channelpipe.FieldTrace           `json:"upgrade_traces,omitempty"`
	LossReports       map[string]*channelpipe.LossReport `json:"loss_reports,omitempty"`
	MainFailure       *FailureReport                     `json:"main_failure,omitempty"`
	ThreePaths        *ThreePathReport                   `json:"three_paths,omitempty"`
	IndependentVerify *independent.Report                `json:"independent_verify,omitempty"`
	ResourceProbe     *ResourceProbe                     `json:"resource_probe,omitempty"`
	Errors            []string                           `json:"errors,omitempty"`
}

type FailureReport struct {
	InjectedChunk       int                  `json:"injected_chunk"`
	FirstRunFailed      []runner.FailedChunk `json:"first_run_failed"`
	FirstRunCommitted   []int                `json:"first_run_committed"`
	RetryCommitted      []int                `json:"retry_committed"`
	FinalCommitSequence []int                `json:"final_commit_sequence"`
	Isolated            bool                 `json:"isolated"`
	Recovered           bool                 `json:"recovered"`
}

type ResourceProbe struct {
	BlockedWaitMS int64 `json:"blocked_wait_ms"`
	Recorded      bool  `json:"recorded"`
}

func Run(cfg Config) (*Report, error) {
	report := &Report{StartedAt: time.Now(), OutputDir: cfg.OutputDir}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.TileWidth <= 0 {
		cfg.TileWidth = 128
	}
	if cfg.TileHeight <= 0 {
		cfg.TileHeight = 128
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return report, channelpipe.WrapError(channelpipe.CodeIO, err, cfg.OutputDir, "cannot create output directory")
	}
	sampleDir := filepath.Join(cfg.OutputDir, "sample")
	report.SampleDir = sampleDir

	sampleSet, err := sample.Generate(sample.Options{Tiny: cfg.Tiny})
	if err != nil {
		return report, err
	}
	written, err := sample.WriteDirectory(sampleDir, sampleSet, cfg.TileWidth, cfg.TileHeight)
	if err != nil {
		return report, err
	}

	loaderChecks, traces, lossReports, err := checkLoaders(sampleDir, sampleSet)
	report.LoaderChecks = loaderChecks
	report.UpgradeTraces = traces
	report.LossReports = lossReports
	report.NegativeChecks = checkNegativeLoads()
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
	}

	mainDB := filepath.Join(cfg.OutputDir, "checkpoint-main.sqlite")
	report.MainDB = mainDB
	if err := freshSQLite(mainDB); err != nil {
		return report, err
	}
	identity := checkpoint.Identity{
		PlanFingerprint:  written.Manifest.PlanFingerprint,
		ParamVersion:     channelpipe.ParamVersion,
		AlgorithmVersion: channelpipe.AlgorithmVersion,
	}
	store, openInfo, err := checkpoint.Open(mainDB, identity)
	if err != nil {
		return report, err
	}
	_ = openInfo
	mainFailure, err := runMainPipeline(context.Background(), store, sampleSet, cfg.Workers, cfg.TileWidth, cfg.TileHeight)
	if err != nil {
		report.Errors = append(report.Errors, "main pipeline: "+err.Error())
	}
	report.MainFailure = mainFailure
	queueEvents, _ := store.QueueEvents()
	if err := writeJSON(filepath.Join(cfg.OutputDir, "queue-events.json"), queueEvents); err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	cacheKeys, cacheErr := store.LoadChunkOutputs(sampleSet.Set.Plans[0].ID, 0)
	if cacheErr == nil {
		if err := writeJSON(filepath.Join(cfg.OutputDir, "cache-keys-chunk0.json"), cacheKeys); err != nil {
			report.Errors = append(report.Errors, err.Error())
		}
	}
	if err := store.Close(); err != nil {
		report.Errors = append(report.Errors, err.Error())
	}

	verifyReport, err := independent.Verify(sampleDir, mainDB)
	if err != nil {
		report.Errors = append(report.Errors, "independent verify: "+err.Error())
	} else {
		report.IndependentVerify = verifyReport
	}

	threePaths, err := RunThreePaths(cfg.OutputDir, sampleSet, cfg.Workers, cfg.TileWidth, cfg.TileHeight)
	report.ThreePaths = threePaths
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	if threePaths != nil {
		_ = writeJSON(filepath.Join(cfg.OutputDir, "three-path-report.json"), threePaths)
		if len(threePaths.Invalidations) > 0 {
			_ = writeJSON(filepath.Join(cfg.OutputDir, "invalidation-report.json"), threePaths.Invalidations)
		}
	}
	report.ResourceProbe = runResourceProbe()

	report.OK = len(report.Errors) == 0 &&
		allLoadersOK(report.LoaderChecks) &&
		allNegativeOK(report.NegativeChecks) &&
		report.MainFailure != nil && report.MainFailure.Isolated && report.MainFailure.Recovered &&
		report.IndependentVerify != nil && report.IndependentVerify.OK &&
		report.ThreePaths != nil && report.ThreePaths.OK &&
		report.ResourceProbe != nil && report.ResourceProbe.Recorded
	report.FinishedAt = time.Now()
	if err := writeJSON(filepath.Join(cfg.OutputDir, "demo-report.json"), report); err != nil {
		return report, err
	}
	if err := writeMarkdown(filepath.Join(cfg.OutputDir, "validation-report.md"), report); err != nil {
		return report, err
	}
	return report, nil
}

func checkLoaders(sampleDir string, set *sample.SampleSet) ([]LoaderCheck, []channelpipe.FieldTrace, map[string]*channelpipe.LossReport, error) {
	checks := []LoaderCheck{}
	var allTraces []channelpipe.FieldTrace
	lossReports := map[string]*channelpipe.LossReport{}
	var firstErr error
	for _, version := range []int{1, 2, 3} {
		name := filepath.Join(sampleDir, "plans", planName(version))
		data, err := os.ReadFile(name)
		if err != nil {
			return checks, allTraces, lossReports, err
		}
		loaded, err := channelpipe.LoadPlanSet(data)
		if err != nil {
			return checks, allTraces, lossReports, err
		}
		check := LoaderCheck{Version: version, File: name, VersionField: loaded.VersionField, TraceCount: len(loaded.UpgradeTraces), SemanticOK: true}
		allTraces = append(allTraces, loaded.UpgradeTraces...)
		for i := range set.Set.Plans {
			ok, diff, err := channelpipe.SemanticEquivalentPlan(set.Set.Plans[i], loaded.Set.Plans[i])
			if err != nil {
				return checks, allTraces, lossReports, err
			}
			if !ok {
				check.SemanticOK = false
				check.SemanticDiff = diff
				if firstErr == nil {
					firstErr = channelpipe.FieldError(channelpipe.CodeDiff, diff, "v%d semantic mismatch", version)
				}
			}
		}
		checks = append(checks, check)
		lossName := name + ".lossy.json"
		if data, err := os.ReadFile(lossName); err == nil {
			var loss channelpipe.LossReport
			if json.Unmarshal(data, &loss) == nil {
				lossReports[planName(version)] = &loss
			}
		}
	}
	return checks, allTraces, lossReports, firstErr
}

func checkNegativeLoads() []NegativeCheck {
	cases := []struct {
		name  string
		json  string
		code  string
		field string
	}{
		{name: "unknown-version", json: `{"plan_ver":9,"jobs":[]}`, code: channelpipe.CodeVersion, field: "plan_ver"},
		{name: "missing-version", json: `{"plans":[]}`, code: channelpipe.CodeSchema, field: "version"},
		{name: "missing-required-field-path", json: `{"version":3,"plans":[{"id":"broken"}]}`, code: channelpipe.CodeSchema, field: "plans[0].width"},
	}
	result := make([]NegativeCheck, 0, len(cases))
	for _, tc := range cases {
		_, err := channelpipe.LoadPlanSet([]byte(tc.json))
		check := NegativeCheck{Name: tc.name, ExpectedCode: tc.code}
		if domainErr, ok := channelpipe.AsError(err); ok {
			check.ActualCode = domainErr.Code
			check.Field = domainErr.Field
			check.OK = domainErr.Code == tc.code && (tc.field == "" || domainErr.Field == tc.field)
		}
		result = append(result, check)
	}
	return result
}

func runResourceProbe() *ResourceProbe {
	gate := runner.NewBufferGate(1, 10)
	firstWait, _ := gate.Acquire(context.Background(), 6, "probe")
	_ = firstWait
	done := make(chan int64, 1)
	go func() {
		waitMS, _ := gate.Acquire(context.Background(), 6, "probe-blocked")
		done <- waitMS
	}()
	time.Sleep(20 * time.Millisecond)
	gate.Release(6)
	select {
	case waitMS := <-done:
		return &ResourceProbe{BlockedWaitMS: waitMS, Recorded: waitMS >= 10}
	case <-time.After(time.Second):
		return &ResourceProbe{Recorded: false}
	}
}

func allLoadersOK(checks []LoaderCheck) bool {
	for _, check := range checks {
		if !check.SemanticOK {
			return false
		}
	}
	return len(checks) == 3
}

func allNegativeOK(checks []NegativeCheck) bool {
	for _, check := range checks {
		if !check.OK {
			return false
		}
	}
	return true
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func planName(version int) string {
	switch version {
	case 1:
		return "plan-v1.json"
	case 2:
		return "plan-v2.json"
	default:
		return "plan-v3.json"
	}
}
