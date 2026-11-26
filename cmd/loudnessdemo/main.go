package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	loudness "soundloudness"
)

const demoSeed int64 = 20261127

func main() {
	out := flag.String("out", "artifacts/demo", "demo output directory")
	flag.Parse()
	if err := run(context.Background(), *out); err != nil {
		fmt.Fprintf(os.Stderr, "demo failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out string) error {
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	cases, err := loudness.GenerateAll(demoSeed)
	if err != nil {
		return err
	}
	projects := make(map[loudness.Version]*loudness.Project, len(cases))
	fmt.Println("== deterministic generation ==")
	for _, generated := range cases {
		projects[generated.Version] = generated.Project
		projectName := fmt.Sprintf("project_%s.json", strings.TrimPrefix(string(generated.Version), "game-audio/"))
		truthName := fmt.Sprintf("truth_%s.json", strings.TrimPrefix(string(generated.Version), "game-audio/"))
		manifestName := fmt.Sprintf("pcm_manifest_%s.json", strings.TrimPrefix(string(generated.Version), "game-audio/"))
		if err := writeFile(filepath.Join(out, projectName), generated.Wire); err != nil {
			return err
		}
		truthJSON, err := loudness.MarshalTruth(generated.Truth)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(out, truthName), truthJSON); err != nil {
			return err
		}
		manifestJSON, err := json.MarshalIndent(generated.PCMManifest, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(out, manifestName), manifestJSON); err != nil {
			return err
		}
		loaded, err := loudness.Load(generated.Wire)
		if err != nil {
			return err
		}
		mismatch, err := loudness.VerifyTruth(loaded, generated.Truth)
		if err != nil {
			return err
		}
		if mismatch != nil {
			return fmt.Errorf("truth mismatch: %w", mismatch)
		}
		fmt.Printf("%s: clips=%d truthFrames=%d traces=%d pcmHashes=%d\n",
			generated.Version, len(loaded.Clips), truthFrameCount(generated.Truth),
			len(loaded.Provenance.Traces), len(generated.PCMManifest))
	}
	if err := compareVersionContent(projects); err != nil {
		return err
	}
	fmt.Println("cross-version core content: compatible (scene/marker loss only)")

	fmt.Println("\n== upgrade / downgrade ==")
	v1 := projects[loudness.SchemaV1]
	upgraded, upgradeReport, err := loudness.Upgrade(v1, loudness.SchemaV3)
	if err != nil {
		return err
	}
	fmt.Printf("v1 -> v3 defaults inserted: %d\n", len(upgradeReport.Defaults))
	for _, trace := range upgradeReport.Defaults {
		if strings.Contains(trace.InternalPath, "calibrationMarker") && strings.Contains(trace.InternalPath, "[1]") {
			break
		}
		if trace.InternalPath == "$.clips[0].scene" || trace.InternalPath == "$.clips[0].calibrationMarker" {
			fmt.Printf("  default %s=%s source=%s\n", trace.InternalPath, trace.NewValue, trace.DefaultSource)
		}
	}
	exported, err := loudness.Export(upgraded, loudness.SchemaV3, loudness.RoundHalfUp)
	if err != nil {
		return err
	}
	if _, err := loudness.Load(exported.Data); err != nil {
		return fmt.Errorf("upgraded export readback: %w", err)
	}
	fmt.Println("v3 export after upgrade: readback ok")

	v3 := projects[loudness.SchemaV3]
	downgraded, lossErr := loudness.Export(v3, loudness.SchemaV1, loudness.RoundHalfUp)
	if !loudness.HasCode(lossErr, loudness.ELossy) {
		return fmt.Errorf("expected E_LOSSY downgrade, got %v", lossErr)
	}
	if err := writeFile(filepath.Join(out, "downgraded_v3_to_v1_lossy.json"), downgraded.Data); err != nil {
		return err
	}
	lossJSON, _ := json.MarshalIndent(downgraded.Losses, "", "  ")
	if err := writeFile(filepath.Join(out, "downgrade_losses.json"), lossJSON); err != nil {
		return err
	}
	readback, err := loudness.Load(downgraded.Data)
	if err != nil {
		return fmt.Errorf("lossy export readback: %w", err)
	}
	if len(readback.Clips) != len(v3.Clips) {
		return fmt.Errorf("lossy readback clip count changed")
	}
	fmt.Printf("v3 -> v1: E_LOSSY with %d entries; readback preserved %d clips\n", len(downgraded.Losses), len(readback.Clips))
	fmt.Printf("  first loss: %s (%s)\n", downgraded.Losses[0].Path, downgraded.Losses[0].Disposition)

	fmt.Println("\n== error-code checks ==")
	if err := demonstrateErrorCodes(projects[loudness.SchemaV3], out); err != nil {
		return err
	}

	fmt.Println("\n== three reconstruction paths ==")
	baselineDir := filepath.Join(out, "checkpoints_baseline")
	baselineEngine, err := newDemoEngine(projects[loudness.SchemaV3], baselineDir)
	if err != nil {
		return err
	}
	baseline, err := baselineEngine.Run(ctx, loudness.RunFull)
	if err != nil {
		return err
	}
	fmt.Printf("baseline commits=%v reusedMeasurements=%d\n", baseline.CommitSequence, len(baseline.MeasurementReused))
	resumeDir := filepath.Join(out, "checkpoints_resume")
	if err := copyDir(baselineDir, resumeDir); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(resumeDir, "clip-000003.json")); err != nil {
		return err
	}

	changes := []loudness.Change{
		{Kind: loudness.ChangeGain, ClipID: 2, GainQ12: projects[loudness.SchemaV3].Clips[2].GainQ12 + 1024},
		{Kind: loudness.ChangeTarget, TargetLoudnessDBQ8: projects[loudness.SchemaV3].TargetLoudnessDBQ8 - 256},
	}
	incrementalEngine, err := newDemoEngine(projects[loudness.SchemaV3], baselineDir)
	if err != nil {
		return err
	}
	if _, err := incrementalEngine.ApplyChanges(changes); err != nil {
		return err
	}
	incremental, err := incrementalEngine.Run(ctx, loudness.RunIncremental)
	if err != nil {
		return err
	}

	fullEngine, err := newDemoEngine(projects[loudness.SchemaV3], filepath.Join(out, "checkpoints_full"))
	if err != nil {
		return err
	}
	if _, err := fullEngine.ApplyChanges(changes); err != nil {
		return err
	}
	full, err := fullEngine.Run(ctx, loudness.RunFull)
	if err != nil {
		return err
	}

	resumeEngine, err := newDemoEngine(projects[loudness.SchemaV3], resumeDir)
	if err != nil {
		return err
	}
	if _, err := resumeEngine.ApplyChanges(changes); err != nil {
		return err
	}
	resumed, err := resumeEngine.Run(ctx, loudness.RunResume)
	if err != nil {
		return err
	}
	for name, result := range map[string]*loudness.BuildResult{"incremental": incremental, "full": full, "resume": resumed} {
		fmt.Printf("%s: commits=%v computed=%v measurementReused=%d calibrationReused=%d waits=%d maxWorkers=%d\n",
			name, result.CommitSequence, result.ComputedClips, len(result.MeasurementReused),
			len(result.CalibrationReused), len(result.WaitEvents), result.MaxObservedWorkers)
	}
	if mismatch := loudness.CompareBuildResults(incremental, full); mismatch != nil {
		return fmt.Errorf("incremental vs full: %w", mismatch)
	}
	if mismatch := loudness.CompareBuildResults(incremental, resumed); mismatch != nil {
		return fmt.Errorf("incremental vs resume: %w", mismatch)
	}
	fmt.Println("three-path comparison: frame-identical (first mismatch: none)")
	fmt.Println("invalidations:")
	for _, invalidation := range incremental.Invalidations {
		fmt.Printf("  clip=%d samples=[%d,%d) phase=%s reason=%s\n",
			invalidation.ClipID, invalidation.StartSample, invalidation.EndSample,
			invalidation.Phase, invalidation.Reason)
	}

	graph, err := loudness.BuildDependencyGraph(projects[loudness.SchemaV3])
	if err != nil {
		return err
	}
	frameRateInvalidations, err := graph.MarkChange(loudness.Change{Kind: loudness.ChangeFrameRate, FrameLength: 192, FrameShift: 96})
	if err != nil {
		return err
	}
	fmt.Printf("frame-rate edit preview: %d merged clip/frame intervals invalidated\n", len(frameRateInvalidations))
	snapshot := graph.Snapshot()
	nodeKinds := make(map[loudness.NodeKind]int)
	for _, node := range snapshot.Nodes {
		nodeKinds[node.Kind]++
	}
	fmt.Printf("dependency graph: source=%d frameEnvelope=%d calibration=%d\n",
		nodeKinds[loudness.NodeSource], nodeKinds[loudness.NodeFrameEnvelope], nodeKinds[loudness.NodeCalibration])

	fmt.Println("\n== frame / gain diagnostics ==")
	printDiagnostics(resumed, 12)
	fmt.Println("\n== checkpoint summaries ==")
	for _, summary := range incremental.Checkpoints {
		fmt.Printf("  clip=%d commit=%d source=%s quant=%s algo=%s digest=%s gainQ12=%d\n",
			summary.ClipID, summary.CommitIndex, summary.SourceVersion, summary.QuantVersion,
			summary.AlgorithmVersion, shortHash(summary.EnvelopeDigest), summary.GainAdjustmentQ12)
	}
	fmt.Printf("\nall outputs written to %s\n", out)
	return nil
}

func newDemoEngine(project *loudness.Project, dir string) (*loudness.Engine, error) {
	return loudness.NewEngine(project, loudness.EngineOptions{
		CheckpointDir: dir,
		Scheduler: loudness.SchedulerConfig{
			MaxWorkers: 3, QueueCapacity: 1, QueueBudgetSamples: 8000,
			BufferBudgetSamples: 6000, MaxSamplesPerWorker: 6000,
		},
		Rounding: loudness.RoundHalfUp,
	})
}

func demonstrateErrorCodes(project *loudness.Project, out string) error {
	unknown := []byte(`{"schema":"game-audio/v9"}`)
	_, err := loudness.Load(unknown)
	if !loudness.HasCode(err, loudness.ESchema) {
		return fmt.Errorf("unknown version: expected E_SCHEMA, got %v", err)
	}
	domainErr, _ := loudness.AsDomainError(err)
	fmt.Printf("E_SCHEMA unknown version: path=%s source=%s\n", domainErr.Path, domainErr.SourceVersion)

	missing := []byte(`{"schema":"game-audio/v1"}`)
	_, err = loudness.Load(missing)
	if !loudness.HasCode(err, loudness.ESchema) {
		return fmt.Errorf("missing field: expected E_SCHEMA, got %v", err)
	}
	domainErr, _ = loudness.AsDomainError(err)
	fmt.Printf("E_SCHEMA missing field: path=%s source=%s\n", domainErr.Path, domainErr.SourceVersion)

	err = loudness.ValidateFrameParameters(0, 1)
	if !loudness.HasCode(err, loudness.EPara) {
		return fmt.Errorf("frame params: expected E_PARA, got %v", err)
	}
	fmt.Printf("E_PARA frame bounds: %v\n", err)

	_, _, err = loudness.MulDiv(math.MaxInt64, 2, 1, loudness.RoundDown)
	if !loudness.HasCode(err, loudness.EOvF) {
		return fmt.Errorf("overflow: expected E_OVF, got %v", err)
	}
	fmt.Printf("E_OVF integer conversion: %v\n", err)

	machine, _ := loudness.NewStateMachine([]int{0})
	err = machine.Transition(0, loudness.StateCommitted)
	if !loudness.HasCode(err, loudness.EState) {
		return fmt.Errorf("state: expected E_STATE, got %v", err)
	}
	snapshot, _ := machine.Snapshot(0)
	committed, total := machine.Progress()
	fmt.Printf("E_STATE illegal transition: state=%s progress=%d/%d\n", snapshot.State, committed, total)

	loudEngine, err := newDemoEngine(project, filepath.Join(out, "checkpoints_e_loud"))
	if err != nil {
		return err
	}
	if _, err := loudEngine.ApplyChanges([]loudness.Change{{Kind: loudness.ChangeTarget, TargetLoudnessDBQ8: 0}}); err != nil {
		return err
	}
	_, err = loudEngine.Run(context.Background(), loudness.RunFull)
	if !loudness.HasCode(err, loudness.ELoud) {
		return fmt.Errorf("loudness: expected E_LOUD, got %v", err)
	}
	loudErr, _ := loudness.AsDomainError(err)
	fmt.Printf("E_LOUD unreachable target: target=%d reachableUpper=%d limitedClips=%v\n",
		loudErr.Target, loudErr.Reachable, loudErr.LimitedClips)
	return nil
}

func compareVersionContent(projects map[loudness.Version]*loudness.Project) error {
	base := projects[loudness.SchemaV1]
	if base == nil {
		return fmt.Errorf("missing v1 project")
	}
	for _, version := range []loudness.Version{loudness.SchemaV2, loudness.SchemaV3} {
		project := projects[version]
		if project == nil {
			return fmt.Errorf("missing %s project", version)
		}
		if project.SampleRate != base.SampleRate || project.FrameLength != base.FrameLength ||
			project.FrameShift != base.FrameShift || len(project.Clips) != len(base.Clips) {
			return fmt.Errorf("%s core header differs from v1", version)
		}
		for i := range base.Clips {
			left, right := base.Clips[i], project.Clips[i]
			if left.ID != right.ID || left.StartSample != right.StartSample || left.EndSample != right.EndSample ||
				left.Category != right.Category || left.AmplitudeQ12 != right.AmplitudeQ12 {
				return fmt.Errorf("%s clip %d core fields differ", version, left.ID)
			}
			if len(left.F0TrackHz) != len(right.F0TrackHz) {
				return fmt.Errorf("%s clip %d F0 length differs", version, left.ID)
			}
			for j := range left.F0TrackHz {
				if left.F0TrackHz[j] != right.F0TrackHz[j] {
					return fmt.Errorf("%s clip %d F0 differs", version, left.ID)
				}
			}
			if abs(left.GainQ12-right.GainQ12) > 64 || abs(left.LoudnessWeightQ8-right.LoudnessWeightQ8) > 2 {
				return fmt.Errorf("%s clip %d fixed-point content differs beyond tolerance: gain %d/%d weight %d/%d",
					version, left.ID, left.GainQ12, right.GainQ12, left.LoudnessWeightQ8, right.LoudnessWeightQ8)
			}
		}
		if abs(project.SessionGainQ12-base.SessionGainQ12) > 16 || abs(project.MaxGainQ12-base.MaxGainQ12) > 32 ||
			abs(project.TargetLoudnessDBQ8-base.TargetLoudnessDBQ8) > 4 {
			return fmt.Errorf("%s session fixed-point parameters differ from v1", version)
		}
	}
	return nil
}

func printDiagnostics(result *loudness.BuildResult, frameLimit int) {
	printed := 0
	for _, clip := range result.Clips {
		fmt.Printf("clip=%d gainAdjustmentQ12=%d desiredQ12=%d limited=%t calibratedSegment=%s\n",
			clip.ClipID, clip.GainAdjustmentQ12, clip.DesiredGainQ12, clip.GainLimited, formatDB(clip.CalibratedSegmentDBQ8))
		for _, frame := range clip.Frames {
			if printed >= frameLimit {
				continue
			}
			fmt.Printf("  clip=%d frame=%02d range=[%d,%d) energy=%d measured=%s calibrated=%s peak=%d cache=%s\n",
				frame.ClipID, frame.FrameIndex, frame.StartSample, frame.EndSample, frame.EnergyQ32,
				formatDB(frame.MeasuredDBQ8), formatDB(frame.CalibratedDBQ8), frame.PeakSample, shortHash(frame.CacheKey))
			printed++
		}
	}
}

func formatDB(q8 int64) string {
	negative := q8 < 0
	value := q8
	if negative {
		value = -value
	}
	sign := ""
	if negative {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%03d dB", sign, value/256, (value%256)*1000/256)
}

func shortHash(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func truthFrameCount(truth *loudness.TruthDocument) int {
	total := 0
	for _, clip := range truth.Clips {
		total += len(clip.Frames)
	}
	return total
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func copyDir(source, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
