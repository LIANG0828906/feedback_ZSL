package quantforensics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type SchemaInteropSummary struct {
	Version       string `json:"version"`
	Bytes         int    `json:"bytes"`
	TraceCount    int    `json:"traceCount"`
	LossCount     int    `json:"lossCount"`
	SemanticEqual bool   `json:"semanticEqual"`
	LoadedVersion string `json:"loadedVersion"`
	OK            bool   `json:"ok"`
}

type RunSummary struct {
	Mode            string `json:"mode"`
	Outcomes        int    `json:"outcomes"`
	CommitStages    int    `json:"commitStages"`
	ComputedBlocks  int    `json:"computedBlocks"`
	ReusedBlocks    int    `json:"reusedBlocks"`
	FailedBlocks    int    `json:"failedBlocks"`
	QueueWaitEvents int    `json:"queueWaitEvents"`
	Interrupted     bool   `json:"interrupted"`
	MaxParallelism  int    `json:"maxParallelism"`
	PixelBufferCap  int    `json:"pixelBufferCap"`
	QueueBudget     int    `json:"queueBudget"`
}

func SummarizeRun(r *RunReport) RunSummary {
	if r == nil {
		return RunSummary{}
	}
	return RunSummary{
		Mode: r.Mode, Outcomes: len(r.Outcomes), CommitStages: len(r.CommitSequence),
		ComputedBlocks: len(r.ComputedBlocks), ReusedBlocks: len(r.ReusedBlocks), FailedBlocks: len(r.FailedBlocks),
		QueueWaitEvents: len(r.QueueWaits), Interrupted: r.Interrupted, MaxParallelism: r.MaxParallelism,
		PixelBufferCap: r.PixelBufferCap, QueueBudget: r.QueueBudget,
	}
}

type PathVerificationSummary struct {
	Consistent            bool           `json:"consistent"`
	CheckedBlocks         int            `json:"checkedBlocks"`
	FirstInconsistency    *Inconsistency `json:"firstInconsistency,omitempty"`
	CommitSequencesEqual  bool           `json:"commitSequencesEqual"`
	CommitSequenceLengths []int          `json:"commitSequenceLengths"`
	Realtime              RunSummary     `json:"realtime"`
	RecoveryPartial       RunSummary     `json:"recoveryPartial"`
	Recovery              RunSummary     `json:"recovery"`
	FullRecompute         RunSummary     `json:"fullRecompute"`
}

func summarizePaths(r *ThreePathResult) PathVerificationSummary {
	return PathVerificationSummary{
		Consistent: r.Consistency.Consistent, CheckedBlocks: r.Consistency.CheckedBlocks,
		FirstInconsistency: r.Consistency.FirstInconsistency, CommitSequencesEqual: r.Consistency.CommitSequencesEqual,
		CommitSequenceLengths: r.Consistency.CommitSequenceLengths,
		Realtime:              SummarizeRun(r.Realtime), RecoveryPartial: SummarizeRun(r.RecoveryPartial),
		Recovery: SummarizeRun(r.Recovery), FullRecompute: SummarizeRun(r.FullRecompute),
	}
}

type CategoryConclusion struct {
	Kind           BlockKind `json:"kind"`
	Blocks         int       `json:"blocks"`
	Passed         int       `json:"passed"`
	Failed         int       `json:"failed"`
	Conclusion     string    `json:"conclusion"`
	ExampleBlockID string    `json:"exampleBlockId"`
	MaxDeviation   [3]int    `json:"maxDeviation"`
	WeightedDiff   int64     `json:"weightedDiff"`
}

type FaultSummary struct {
	BlockID           string   `json:"blockId"`
	FirstFailedBlocks []string `json:"firstFailedBlocks"`
	RetryFailedBlocks []string `json:"retryFailedBlocks"`
	Attempts          int      `json:"attempts"`
	OK                bool     `json:"ok"`
}

type SQLiteFileInfo struct {
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	StageRecords int    `json:"stageRecords"`
}

type DiagnosticReport struct {
	OK                               bool                    `json:"ok"`
	GeneratedAtUTC                   string                  `json:"generatedAtUtc"`
	Seed                             int64                   `json:"seed"`
	ProjectID                        string                  `json:"projectId"`
	BlockCount                       int                     `json:"blockCount"`
	ImageCount                       int                     `json:"imageCount"`
	ScreenshotFiles                  []string                `json:"screenshotFiles"`
	SchemaInterop                    []SchemaInteropSummary  `json:"schemaInterop"`
	SchemaErrorFieldPath             string                  `json:"schemaErrorFieldPath"`
	BaselineThreePaths               PathVerificationSummary `json:"baselineThreePaths"`
	EditedThreePaths                 PathVerificationSummary `json:"editedThreePaths"`
	Invalidation                     *InvalidationScenario   `json:"invalidation,omitempty"`
	FaultRetry                       FaultSummary            `json:"faultRetry"`
	Categories                       []CategoryConclusion    `json:"categories"`
	Triage                           ForensicTriage          `json:"triage"`
	TightToleranceMeasurementsStable bool                    `json:"tightToleranceMeasurementsStable"`
	QueueWaitSample                  []QueueWait             `json:"queueWaitSample"`
	SQLiteFiles                      []SQLiteFileInfo        `json:"sqliteFiles"`
	ValidationErrors                 []string                `json:"validationErrors,omitempty"`
}

func WriteDiagnostics(outDir, dbDir string) (*DiagnosticReport, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, err
	}
	p, sample := DefaultProject()
	report := &DiagnosticReport{GeneratedAtUTC: time.Now().UTC().Format(time.RFC3339), Seed: p.Seed, ProjectID: p.ProjectID, BlockCount: len(p.Blocks), ImageCount: len(p.Images)}
	screenshotFiles, err := WriteSamplePNGs(sample, filepath.Join(outDir, "screenshots"))
	if err != nil {
		return nil, err
	}
	report.ScreenshotFiles = screenshotFiles

	if err := writeSchemaDocuments(p, outDir, report); err != nil {
		return nil, err
	}
	if _, err := LoadProject([]byte(`{"schema":"forensics.v9"}`)); err != nil {
		fe := asDiagnosticError(err)
		report.SchemaErrorFieldPath = fe.Field
		if fe.Code != CodeSchema {
			report.ValidationErrors = append(report.ValidationErrors, "unknown schema did not return E_SCHEMA")
		}
	}

	baseline, err := RunThreePathVerification(p, dbDir)
	if err != nil {
		return nil, err
	}
	report.BaselineThreePaths = summarizePaths(baseline)
	if !baseline.Consistency.Consistent {
		report.ValidationErrors = append(report.ValidationErrors, "baseline three-path inconsistency")
	}
	fullJSON, _ := json.MarshalIndent(baseline, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "three-path-baseline.json"), fullJSON, 0o644); err != nil {
		return nil, err
	}

	edited := ApplyForensicEditSequence(p)
	editedPaths, err := RunThreePathVerification(edited, filepath.Join(dbDir, "edited-sequence"))
	if err != nil {
		return nil, err
	}
	report.EditedThreePaths = summarizePaths(editedPaths)
	if !editedPaths.Consistency.Consistent {
		report.ValidationErrors = append(report.ValidationErrors, "edited three-path inconsistency")
	}

	inv, err := RunInvalidationScenario(p, dbDir)
	if err != nil {
		return nil, err
	}
	report.Invalidation = inv
	for _, step := range inv.Steps {
		if len(step.FailedBlocks) != 0 {
			report.ValidationErrors = append(report.ValidationErrors, "invalidation step "+step.Name+" has failed blocks")
		}
	}

	report.FaultRetry = runFaultDiagnostic(p)
	if !report.FaultRetry.OK {
		report.ValidationErrors = append(report.ValidationErrors, "fault retry did not converge")
	}
	report.Categories, report.TightToleranceMeasurementsStable = categoryConclusions(p, baseline.Realtime)
	report.Triage = RunForensicTriage(p, baseline.Realtime)

	for _, w := range baseline.FullRecompute.QueueWaits {
		if len(report.QueueWaitSample) < 20 {
			report.QueueWaitSample = append(report.QueueWaitSample, w)
		}
	}

	report.SQLiteFiles = collectSQLiteInfo(dbDir)
	report.OK = len(report.ValidationErrors) == 0 && report.BaselineThreePaths.Consistent && report.EditedThreePaths.Consistent

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "diagnostic-report.json"), data, 0o644); err != nil {
		return nil, err
	}
	if err := writeDiagnosticHTML(filepath.Join(outDir, "index.html"), report, sample); err != nil {
		return nil, err
	}
	return report, nil
}

func writeSchemaDocuments(p *Project, outDir string, report *DiagnosticReport) error {
	showcase := ApplyForensicEditSequence(p)
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		data, conv, err := ExportProject(showcase, version)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, "project."+version+".json"), data, 0o644); err != nil {
			return err
		}
		loaded, err := LoadProject(data)
		if err != nil {
			return err
		}
		degraded, _, err := DegradeProject(showcase, version)
		if err != nil {
			return err
		}
		equal := SemanticHash(loaded) == SemanticHash(degraded)
		report.SchemaInterop = append(report.SchemaInterop, SchemaInteropSummary{Version: version, Bytes: len(data), TraceCount: len(loaded.Traces), LossCount: len(conv.Losses), SemanticEqual: equal, LoadedVersion: loaded.SourceSchema, OK: equal})
		if !equal {
			report.ValidationErrors = append(report.ValidationErrors, "schema semantic mismatch for "+version)
		}
	}
	return nil
}

func asDiagnosticError(err error) *Error {
	if fe, ok := err.(*Error); ok {
		return fe
	}
	return &Error{Code: "E_UNKNOWN", Message: err.Error()}
}

func runFaultDiagnostic(p *Project) FaultSummary {
	blockID := "shot-main-b00"
	store := NewMemoryStore("fault-diagnostic")
	sm := NewStateMachine()
	faults := NewFaultPolicy(blockID)
	first := NewEngine(p, store, EngineOptions{Mode: "fault-diagnostic", Workers: 1, StateMachine: sm})
	first.Faults = faults
	r1, err := first.Run()
	if err != nil {
		return FaultSummary{BlockID: blockID, OK: false}
	}
	second := NewEngine(p, store, EngineOptions{Mode: "fault-retry", Workers: 1, ReuseCheckpoints: true, StateMachine: sm})
	second.Faults = faults
	r2, err := second.Run()
	if err != nil {
		return FaultSummary{BlockID: blockID, FirstFailedBlocks: r1.FailedBlocks, OK: false}
	}
	return FaultSummary{BlockID: blockID, FirstFailedBlocks: r1.FailedBlocks, RetryFailedBlocks: r2.FailedBlocks, Attempts: faults.Attempts(blockID), OK: len(r1.FailedBlocks) == 1 && len(r2.FailedBlocks) == 0}
}

func categoryConclusions(p *Project, r *RunReport) ([]CategoryConclusion, bool) {
	byID := map[string]BlockOutcome{}
	for _, o := range r.Outcomes {
		byID[o.Block.ID] = o
	}
	groups := map[BlockKind]*CategoryConclusion{}
	stable := true
	for _, b := range p.Blocks {
		o := byID[b.ID]
		c := groups[b.Kind]
		if c == nil {
			c = &CategoryConclusion{Kind: b.Kind}
			groups[b.Kind] = c
		}
		c.Blocks++
		if o.Evaluation.Passed {
			c.Passed++
			c.Conclusion = "some pass"
		} else {
			c.Failed++
		}
		if c.ExampleBlockID == "" || b.Kind == KindGradient || b.Kind == KindStroke {
			c.ExampleBlockID = b.ID
			c.MaxDeviation = o.Evaluation.MaxDeviation
			c.WeightedDiff = o.Evaluation.WeightedColorDiff
		}
		im, ok := func() (Image, bool) {
			for _, im := range p.Images {
				if im.ID == b.ImageID {
					return im, true
				}
			}
			return Image{}, false
		}()
		if ok {
			tight := EvaluateBlock(im, b, o.Reconstruction.Pixels, p.Params.Metric, Tolerance{MaxComponentDeviation: 1, MaxWeightedColorDiff: 1})
			if tight.WeightedColorDiff != o.Evaluation.WeightedColorDiff || tight.MaxDeviation != o.Evaluation.MaxDeviation {
				stable = false
			}
		}
	}
	order := []BlockKind{KindForeground, KindGradient, KindStroke, KindNoise, KindMixed}
	out := []CategoryConclusion{}
	for _, kind := range order {
		if c := groups[kind]; c != nil {
			if c.Failed == 0 {
				c.Conclusion = "all_pass"
			} else if c.Passed == 0 {
				c.Conclusion = "all_fail_under_declared_tolerance"
			} else {
				c.Conclusion = "mixed_under_declared_tolerance"
			}
			out = append(out, *c)
		}
	}
	return out, stable
}

func collectSQLiteInfo(dir string) []SQLiteFileInfo {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sqlite"))
	for _, sub := range []string{"edited-sequence"} {
		more, _ := filepath.Glob(filepath.Join(dir, sub, "*.sqlite"))
		paths = append(paths, more...)
	}
	out := []SQLiteFileInfo{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		item := SQLiteFileInfo{Path: path, Bytes: info.Size()}
		if store, err := OpenSQLiteStore(path); err == nil {
			if records, err := store.Records(""); err == nil {
				item.StageRecords = len(records)
			}
			_ = store.Close()
		}
		out = append(out, item)
	}
	return out
}
