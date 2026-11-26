package budget

import (
	"path/filepath"
)

type FormatLoadSummary struct {
	Version      SourceVersion `json:"version"`
	File         string        `json:"file"`
	Hash         string        `json:"hash"`
	TraceCount   int           `json:"trace_count"`
	TickCount    int           `json:"tick_count"`
	IntentCount  int           `json:"intent_count"`
	MatchesTruth bool          `json:"matches_truth"`
}

type ScaleRoundTripProbe struct {
	OriginalMyriad int64 `json:"original_myriad"`
	LowPermille    int64 `json:"low_permille"`
	Residual       int64 `json:"residual"`
	RestoredMyriad int64 `json:"restored_myriad"`
	Exact          bool  `json:"exact"`
}

type DemoResult struct {
	Seed            int64               `json:"seed"`
	OutDir          string              `json:"out_dir"`
	Documents       map[string]string   `json:"documents"`
	Loads           []FormatLoadSummary `json:"loads"`
	ScaleRoundTrip  ScaleRoundTripProbe `json:"scale_round_trip"`
	ThreePaths      *ThreePathResult    `json:"three_paths"`
	Incremental     *IncrementalReport  `json:"incremental"`
	Faults          []FaultProbe        `json:"faults"`
	Budget          BudgetSummary       `json:"budget"`
	DiagnosticsPath string              `json:"diagnostics_path"`
	ResultPath      string              `json:"result_path"`
	AllFaultsPass   bool                `json:"all_faults_pass"`
}

func RunDemo(seed int64, outDir string) (*DemoResult, error) {
	if outDir == "" {
		outDir = "demo-out"
	}
	fixture, err := GenerateFixture(seed)
	if err != nil {
		return nil, err
	}
	params := DefaultParams()
	result := &DemoResult{
		Seed: seed, OutDir: outDir,
		Documents: map[string]string{},
	}

	versions := []struct {
		version SourceVersion
		file    string
	}{
		{SourceV1, "rules-v1.json"},
		{SourceV2, "rules-v2.json"},
		{SourceV3, "rules-v3.json"},
	}
	loadedByVersion := map[SourceVersion]*Project{}
	docsByVersion := map[SourceVersion][]byte{}
	for _, item := range versions {
		doc, err := RenderDocument(fixture.Project, item.version)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(outDir, item.file)
		if err := WriteAtomicBytes(path, doc); err != nil {
			return nil, err
		}
		loaded, err := LoadDocument(doc)
		if err != nil {
			return nil, err
		}
		docsByVersion[item.version] = doc
		loadedByVersion[item.version] = loaded
		report, err := ReferenceSchedule(loaded, params)
		if err != nil {
			return nil, err
		}
		if err := CompareReports(fixture.Truth, report); err != nil {
			return nil, err
		}
		result.Documents[string(item.version)] = path
		result.Loads = append(result.Loads, FormatLoadSummary{
			Version: item.version, File: path, Hash: loaded.ContentHash,
			TraceCount: len(loaded.Traces), TickCount: len(report.Ticks),
			IntentCount: len(loaded.Intents), MatchesTruth: true,
		})
	}

	scaleProbe, err := demoScaleRoundTrip()
	if err != nil {
		return nil, err
	}
	result.ScaleRoundTrip = scaleProbe

	v3 := loadedByVersion[SourceV3]
	threePaths, err := RunThreePathCheck(v3, params, filepath.Join(outDir, "checkpoints"), 1)
	if err != nil {
		return nil, err
	}
	for _, report := range []*RunReport{threePaths.Realtime, threePaths.Resumed, threePaths.Full} {
		if err := CompareTickReports(fixture.Truth, report); err != nil {
			return nil, err
		}
	}
	result.ThreePaths = threePaths

	cachePath := filepath.Join(outDir, "cache", "result-cache.json")
	if _, _, err := BuildBaselineCache(v3, threePaths.Realtime, params, cachePath); err != nil {
		return nil, err
	}
	edited, changed, err := EditSkillCooldown(v3, "fireball", 6)
	if err != nil {
		return nil, err
	}
	incremental, err := RerunWithPersistentCache(v3, threePaths.Realtime, edited, params, cachePath, changed)
	if err != nil {
		return nil, err
	}
	expectedEdited, err := ReferenceSchedule(edited, params)
	if err != nil {
		return nil, err
	}
	if err := CompareReports(expectedEdited, incremental.Report); err != nil {
		return nil, err
	}
	result.Incremental = incremental

	result.Budget = BuildBudget(v3, threePaths.Realtime)
	diagPath := filepath.Join(outDir, "diagnostics.html")
	diagData := DiagnosticsData{
		Project: v3, Report: threePaths.Realtime, Plan: incremental.Plan,
		Budget: result.Budget, GeneratedNote: "deterministic local demo",
	}
	if err := WriteDiagnostics(diagPath, diagData); err != nil {
		return nil, err
	}
	if err := WriteDiagnostics(filepath.Join(outDir, "index.html"), diagData); err != nil {
		return nil, err
	}
	result.DiagnosticsPath = diagPath

	result.Faults = RunFaultProbes(fixture.Project, docsByVersion[SourceV1], docsByVersion[SourceV3])
	result.AllFaultsPass = true
	for _, probe := range result.Faults {
		if !probe.OK {
			result.AllFaultsPass = false
		}
	}

	resultPath := filepath.Join(outDir, "demo-result.json")
	result.ResultPath = resultPath
	if err := WriteAtomicJSON(resultPath, result); err != nil {
		return nil, err
	}
	return result, nil
}

func demoScaleRoundTrip() (ScaleRoundTripProbe, error) {
	const original = int64(4234)
	low, err := ConvertScaled(original, ScalePermyriad, ScalePermille, RoundFloor, "demo.cost")
	if err != nil {
		return ScaleRoundTripProbe{}, err
	}
	restored, err := low.RestoreHighResolution(ScalePermyriad, "demo.cost")
	if err != nil {
		return ScaleRoundTripProbe{}, err
	}
	return ScaleRoundTripProbe{
		OriginalMyriad: original, LowPermille: low.Value, Residual: low.Residual,
		RestoredMyriad: restored.Value, Exact: restored.Value == original && low.Residual == 4,
	}, nil
}
