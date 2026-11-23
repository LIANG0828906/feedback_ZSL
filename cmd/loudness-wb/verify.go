package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"loudnessworkbench/apperr"
	"loudnessworkbench/checkpoint"
	"loudnessworkbench/engine"
	"loudnessworkbench/project"
)

type runSummary struct {
	Name           string              `json:"name"`
	SourceVersion  string              `json:"source_version"`
	Mode           engine.Mode         `json:"mode"`
	MaxParallel    int                 `json:"max_parallel"`
	MixHash        string              `json:"mix_hash"`
	MixFrames      int                 `json:"mix_frames"`
	GlobalGainDB   float64             `json:"global_gain_db"`
	CompressedLUFS float64             `json:"compressed_lufs"`
	FinalLUFS      float64             `json:"final_lufs"`
	TargetLUFS     float64             `json:"target_lufs"`
	Iterations     int                 `json:"normalization_iterations"`
	Converged      bool                `json:"normalization_converged"`
	Committed      bool                `json:"committed"`
	Recovery       engine.RecoveryInfo `json:"recovery"`
	GainDecisions  []float64           `json:"gain_decisions"`
}

type comparisonSummary struct {
	Left      string             `json:"left"`
	Right     string             `json:"right"`
	Identical bool               `json:"identical"`
	GainEqual bool               `json:"gain_decisions_equal"`
	FirstDiff *engine.Divergence `json:"first_divergence,omitempty"`
	Heatmap   []engine.DiffSpan  `json:"segment_heatmap"`
}

type interopSummary struct {
	Left          string   `json:"left"`
	Right         string   `json:"right"`
	Equivalent    bool     `json:"equivalent"`
	FirstMismatch any      `json:"first_mismatch,omitempty"`
	Trace         []string `json:"degradation_trace"`
}

type checkpointCheck struct {
	Name            string             `json:"name"`
	Invalidated     bool               `json:"invalidated"`
	CheckpointUsed  bool               `json:"checkpoint_used"`
	Reason          string             `json:"reason"`
	Committed       bool               `json:"committed"`
	MixHash         string             `json:"mix_hash"`
	MatchesBaseline bool               `json:"matches_baseline"`
	FirstDiff       *engine.Divergence `json:"first_divergence,omitempty"`
}

type verificationReport struct {
	Manifest         string                  `json:"manifest"`
	Algorithm        string                  `json:"algorithm_version"`
	Versions         map[string]any          `json:"versions"`
	Interoperability []interopSummary        `json:"interoperability"`
	Runs             []runSummary            `json:"runs"`
	Comparisons      []comparisonSummary     `json:"comparisons"`
	Truth            *engine.TruthReport     `json:"truth_verification"`
	ToleranceSweep   []engine.TolerancePoint `json:"tolerance_sweep"`
	CheckpointChecks []checkpointCheck       `json:"checkpoint_checks"`
}

func commandVerify(args []string) error {
	flags := newFlagSet("verify")
	dataDir := flags.String("data", "demo", "generated data directory")
	outPath := flags.String("out", "artifacts/verification.json", "verification report path")
	parallel := flags.Int("parallel", 4, "maximum parallel workers")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifestPath := filepath.Join(*dataDir, "manifest.json")
	manifest, err := project.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	versionPaths := map[string]string{
		project.SourceV1: filepath.Join(*dataDir, "projects", "program.v1.json"),
		project.SourceV2: filepath.Join(*dataDir, "projects", "program.v2.json"),
		project.SourceV3: filepath.Join(*dataDir, "projects", "program.v3.json"),
	}
	programs := map[string]*project.Program{}
	versions := map[string]any{}
	for _, version := range []string{project.SourceV1, project.SourceV2, project.SourceV3} {
		program, err := project.LoadWithManifest(versionPaths[version], manifestPath)
		if err != nil {
			return err
		}
		programs[version] = program
		versions[version] = map[string]any{
			"path": versionPaths[version], "project_id": program.ProjectID,
			"parameters_hash": program.DecisionParamsHash(),
			"segments":        len(program.Segments),
		}
	}
	report := &verificationReport{
		Manifest: manifestPath, Algorithm: checkpoint.AlgorithmVersion,
		Versions: versions,
	}
	pairs := [][2]string{
		{project.SourceV1, project.SourceV2},
		{project.SourceV1, project.SourceV3},
		{project.SourceV2, project.SourceV3},
	}
	for _, pair := range pairs {
		equivalence, compareErr := project.ComparePrograms(programs[pair[0]], programs[pair[1]])
		entry := interopSummary{Left: pair[0], Right: pair[1], Trace: equivalence.Trace}
		entry.Equivalent = compareErr == nil
		if compareErr != nil {
			if e, ok := apperr.As(compareErr); ok {
				entry.FirstMismatch = map[string]any{
					"code": e.Code, "path": e.Path, "left": e.Left, "right": e.Right,
				}
			}
			report.Interoperability = append(report.Interoperability, entry)
			return compareErr
		}
		report.Interoperability = append(report.Interoperability, entry)
	}

	checkpointRoot := filepath.Join(*dataDir, "checkpoints", "verification")
	if err := os.RemoveAll(checkpointRoot); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot clear verification checkpoints", err)
	}
	baselineStore := checkpoint.NewStore(filepath.Join(checkpointRoot, "full-v1"), programs[project.SourceV1].ProjectID)
	baseline, err := runSession(programs[project.SourceV1], baselineStore, engine.ModeFull, *parallel)
	if err != nil {
		return err
	}
	report.Runs = append(report.Runs, summarizeRun("full-v1", programs[project.SourceV1], baseline))
	truthReport, err := engine.VerifyTruth(baseline, manifest)
	if err != nil {
		report.Truth = truthReport
		return err
	}
	report.Truth = truthReport
	report.ToleranceSweep = engine.ToleranceSweep(baseline, manifest,
		[]float64{0.2, 0.1, 0.05, 0.01, 0.001})
	for i := 1; i < len(report.ToleranceSweep); i++ {
		if report.ToleranceSweep[i].SegmentsWithin > report.ToleranceSweep[i-1].SegmentsWithin {
			return apperr.New(apperr.CodeInternal, "tolerance sweep monotonicity violated")
		}
	}

	realtimeStore := checkpoint.NewStore(filepath.Join(checkpointRoot, "realtime-v1"), programs[project.SourceV1].ProjectID)
	realtime, err := runSession(programs[project.SourceV1], realtimeStore, engine.ModeRealtime, *parallel)
	if err != nil {
		return err
	}
	report.Runs = append(report.Runs, summarizeRun("realtime-v1", programs[project.SourceV1], realtime))

	resumeStore := checkpoint.NewStore(filepath.Join(checkpointRoot, "resume-v1"), programs[project.SourceV1].ProjectID)
	fullSnapshot, err := baselineStore.Load()
	if err != nil {
		return err
	}
	if err := resumeStore.Save(*fullSnapshot); err != nil {
		return err
	}
	resumed, err := runSession(programs[project.SourceV1], resumeStore, engine.ModeResume, *parallel)
	if err != nil {
		return err
	}
	report.Runs = append(report.Runs, summarizeRun("resume-v1", programs[project.SourceV1], resumed))

	partialSnapshot := *fullSnapshot
	partialSnapshot.Records = append([]checkpoint.Record(nil), fullSnapshot.Records[:3]...)
	partialStore := checkpoint.NewStore(filepath.Join(checkpointRoot, "partial-v1"), programs[project.SourceV1].ProjectID)
	if err := partialStore.Save(partialSnapshot); err != nil {
		return err
	}
	partial, err := runSession(programs[project.SourceV1], partialStore, engine.ModeResume, *parallel)
	if err != nil {
		return err
	}
	report.Runs = append(report.Runs, summarizeRun("partial-resume-v1", programs[project.SourceV1], partial))

	v2Store := checkpoint.NewStore(filepath.Join(checkpointRoot, "full-v2"), programs[project.SourceV2].ProjectID)
	v2Result, err := runSession(programs[project.SourceV2], v2Store, engine.ModeFull, *parallel)
	if err != nil {
		return err
	}
	report.Runs = append(report.Runs, summarizeRun("full-v2", programs[project.SourceV2], v2Result))

	v3Store := checkpoint.NewStore(filepath.Join(checkpointRoot, "full-v3"), programs[project.SourceV3].ProjectID)
	v3Result, err := runSession(programs[project.SourceV3], v3Store, engine.ModeFull, *parallel)
	if err != nil {
		return err
	}
	report.Runs = append(report.Runs, summarizeRun("full-v3", programs[project.SourceV3], v3Result))

	comparisons := []struct {
		leftName, rightName string
		left, right         *engine.Result
	}{
		{"full-v1", "realtime-v1", baseline, realtime},
		{"full-v1", "resume-v1", baseline, resumed},
		{"full-v1", "partial-resume-v1", baseline, partial},
		{"full-v1", "full-v2", baseline, v2Result},
		{"full-v1", "full-v3", baseline, v3Result},
	}
	for _, pair := range comparisons {
		diff, compareErr := engine.CompareResults(pair.left, pair.right)
		entry := comparisonSummary{
			Left: pair.leftName, Right: pair.rightName,
			Identical: diff.Identical, GainEqual: diff.GainDecisionsEqual,
			FirstDiff: diff.FirstDivergence, Heatmap: diff.Segments,
		}
		report.Comparisons = append(report.Comparisons, entry)
		if compareErr != nil {
			if err := writeJSONFile(*outPath, report); err != nil {
				return err
			}
			return compareErr
		}
	}
	for _, run := range report.Runs {
		if !run.Converged || math.Abs(run.FinalLUFS-run.TargetLUFS) > 0.02 {
			return apperr.New(apperr.CodeDivergence, "normalized program did not converge to target loudness").
				WithField("final_program_loudness_lufs").WithActual(run)
		}
	}

	versionBadStore := checkpoint.NewStore(filepath.Join(checkpointRoot, "bad-version"), programs[project.SourceV1].ProjectID)
	badVersionSnapshot := *fullSnapshot
	badVersionSnapshot.AlgorithmVersion = "lw-algo/0.0.0-stale"
	if err := versionBadStore.Save(badVersionSnapshot); err != nil {
		return err
	}
	badVersionResult, err := runSession(programs[project.SourceV1], versionBadStore, engine.ModeResume, *parallel)
	if err != nil {
		return err
	}
	report.CheckpointChecks = append(report.CheckpointChecks,
		evaluateCheckpointCheck("version-mismatch", baseline, badVersionResult, true))

	hashBadStore := checkpoint.NewStore(filepath.Join(checkpointRoot, "bad-output-hash"), programs[project.SourceV1].ProjectID)
	badHashSnapshot := *fullSnapshot
	badHashSnapshot.Records = append([]checkpoint.Record(nil), fullSnapshot.Records...)
	badHashSnapshot.Records[0].OutputHash = strings.Repeat("0", 64)
	if err := hashBadStore.Save(badHashSnapshot); err != nil {
		return err
	}
	badHashResult, err := runSession(programs[project.SourceV1], hashBadStore, engine.ModeResume, *parallel)
	if err != nil {
		return err
	}
	report.CheckpointChecks = append(report.CheckpointChecks,
		evaluateCheckpointCheck("output-hash-mismatch", baseline, badHashResult, true))

	for _, check := range report.CheckpointChecks {
		if !check.Invalidated || !check.Committed || !check.MatchesBaseline {
			return apperr.New(apperr.CodeDivergence, "checkpoint invalidation check failed").
				WithField("checkpoint_checks").WithActual(check)
		}
	}
	if resumed.Recovery.ReusedSegments != len(programs[project.SourceV1].Segments) {
		return apperr.New(apperr.CodeDivergence, "full resume did not reuse every committed segment").
			WithExpected(len(programs[project.SourceV1].Segments)).WithActual(resumed.Recovery.ReusedSegments)
	}
	if partial.Recovery.ReusedSegments != 3 {
		return apperr.New(apperr.CodeDivergence, "partial resume did not reuse the persisted prefix").
			WithExpected(3).WithActual(partial.Recovery.ReusedSegments)
	}
	if err := writeJSONFile(*outPath, report); err != nil {
		return err
	}
	fmt.Printf("verified %d runs, %d interoperability pairs, %d stream comparisons, %d checkpoint cases\n",
		len(report.Runs), len(report.Interoperability), len(report.Comparisons), len(report.CheckpointChecks))
	fmt.Printf("mix sha256: %s\n", baseline.MixHash)
	fmt.Printf("max loudness error: %.9f LU, max true-peak error: %.9f dB\n",
		truthReport.MaxLoudnessErrorLU, truthReport.MaxTruePeakErrorDB)
	fmt.Printf("report: %s\n", *outPath)
	return nil
}

func runSession(program *project.Program, store *checkpoint.Store, mode engine.Mode, parallel int) (*engine.Result, error) {
	session, err := engine.NewSession(program, store)
	if err != nil {
		return nil, err
	}
	return session.Run(context.Background(), engine.RunOptions{
		Mode: mode, MaxParallel: parallel, PerWorkerBufferBytes: 16 << 20,
	})
}

func summarizeRun(name string, program *project.Program, result *engine.Result) runSummary {
	gains := make([]float64, len(result.Segments))
	for i, segment := range result.Segments {
		gains[i] = segment.NormalizationGainDB
	}
	return runSummary{
		Name: name, SourceVersion: program.SourceVersion, Mode: result.Mode,
		MaxParallel: result.MaxParallel, MixHash: result.MixHash, MixFrames: result.MixFrames,
		GlobalGainDB: result.GlobalGainDB, CompressedLUFS: result.CompressedIntegratedLUFS,
		FinalLUFS: result.FinalProgramLoudnessLUFS, TargetLUFS: result.TargetLUFS,
		Iterations: result.NormalizationIterations, Converged: result.NormalizationConverged,
		Committed: result.AllCommitted(), Recovery: result.Recovery, GainDecisions: gains,
	}
}

func evaluateCheckpointCheck(name string, baseline, result *engine.Result, expectInvalidated bool) checkpointCheck {
	diff, compareErr := engine.CompareResults(baseline, result)
	return checkpointCheck{
		Name: name, Invalidated: result.Recovery.Invalidated,
		CheckpointUsed: result.Recovery.CheckpointUsed,
		Reason:         result.Recovery.Reason, Committed: result.AllCommitted(),
		MixHash: result.MixHash, MatchesBaseline: compareErr == nil,
		FirstDiff: diff.FirstDivergence,
	}
}
