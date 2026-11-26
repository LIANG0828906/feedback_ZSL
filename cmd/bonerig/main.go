package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	bonerig "bonerig"
)

type cliReport struct {
	Seed             int64                         `json:"seed"`
	SampleSchemas    []string                      `json:"sample_schemas"`
	CrossFormat      map[string]crossFormatSummary `json:"cross_format"`
	ThreePaths       *bonerig.ThreePathReport      `json:"three_paths"`
	AcceptanceChecks map[string]bool               `json:"acceptance_checks"`
}

type crossFormatSummary struct {
	Frames         int   `json:"frames"`
	JointsPerFrame int   `json:"joints_per_frame"`
	MaxMeasuredL1  int64 `json:"max_measured_l1"`
	MaxUpperBound  int64 `json:"max_upper_bound"`
	Traces         int   `json:"transform_traces"`
	NormTraces     int   `json:"norm_traces"`
	TruthTolerance int64 `json:"truth_tolerance"`
	WithinTruth    bool  `json:"within_truth"`
}

func main() {
	out := flag.String("out", "artifacts/run", "output directory for samples, checkpoints and report")
	workers := flag.Int("workers", 4, "maximum frame worker parallelism")
	jointBuffer := flag.Int("joint-buffer", 6, "per-worker joint state buffer")
	pendingFrames := flag.Int("pending-frames", 2, "pending frame queue budget")
	flag.Parse()

	if err := run(*out, bonerig.Budget{MaxWorkers: *workers, MaxJointBufferPerWorker: *jointBuffer, MaxPendingFrames: *pendingFrames}); err != nil {
		b, _ := json.MarshalIndent(err, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
		os.Exit(1)
	}
}

func run(outDir string, budget bonerig.Budget) *bonerig.Error {
	if err := os.MkdirAll(filepath.Join(outDir, "samples"), 0o755); err != nil {
		e := ioErr(err)
		return e
	}
	sample, se := bonerig.NewDeterministicSample(bonerig.DefaultSeed)
	if se != nil {
		return se
	}
	schemas := []string{bonerig.V1Schema, bonerig.V2Schema, bonerig.V3Schema}
	cross := map[string]crossFormatSummary{}
	checks := map[string]bool{}
	for _, schema := range schemas {
		name := map[string]string{bonerig.V1Schema: "rig-v1.json", bonerig.V2Schema: "rig-v2.json", bonerig.V3Schema: "rig-v3.json"}[schema]
		if err := os.WriteFile(filepath.Join(outDir, "samples", name), sample.Docs[schema], 0o644); err != nil {
			return ioErr(err)
		}
		loaded, le := bonerig.LoadDocument(sample.Docs[schema])
		if le != nil {
			return le
		}
		normBytes, merr := json.MarshalIndent(loaded.NormTraces, "", "  ")
		if merr != nil {
			return ioErr(merr)
		}
		traceName := "norm-traces-" + map[string]string{bonerig.V1Schema: "v1", bonerig.V2Schema: "v2", bonerig.V3Schema: "v3"}[schema] + ".json"
		if err := os.WriteFile(filepath.Join(outDir, traceName), append(normBytes, '\n'), 0o644); err != nil {
			return ioErr(err)
		}
		loaded.Asset.Truth = sample.Truth
		frames, re := bonerig.RetargetAll(loaded.Asset)
		if re != nil {
			return re
		}
		sum := crossFormatSummary{Frames: len(frames), JointsPerFrame: len(loaded.Asset.TargetSkeleton.Joints), Traces: len(loaded.Asset.Traces), NormTraces: len(loaded.NormTraces), TruthTolerance: sample.Truth.Tolerance, WithinTruth: true}
		for _, f := range frames {
			for _, r := range f.Precision {
				if r.MeasuredDeviation > sum.MaxMeasuredL1 {
					sum.MaxMeasuredL1 = r.MeasuredDeviation
				}
				if r.TotalUpper > sum.MaxUpperBound {
					sum.MaxUpperBound = r.TotalUpper
				}
				if r.MeasuredDeviation > sample.Truth.Tolerance || !r.WithinBound {
					sum.WithinTruth = false
				}
			}
		}
		cross[schema] = sum
		checks["load_and_retarget_"+schema] = sum.WithinTruth
	}
	truthBytes, _ := json.MarshalIndent(sample.Truth, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "samples", "truth.json"), append(truthBytes, '\n'), 0o644); err != nil {
		return ioErr(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "dependency-graph.dot"), []byte(bonerig.BuildAssetGraph(sample.Asset).RenderDOT()), 0o644); err != nil {
		return ioErr(err)
	}

	paths, pe := bonerig.RunThreePaths(filepath.Join(outDir, "checkpoints"), budget)
	if pe != nil {
		return pe
	}
	checks["three_path_frame_identical"] = paths.FrameDiff == nil
	checks["three_path_sequence_identical"] = paths.SequenceDiff == nil
	checks["recovery_write_sequence_identical"] = paths.RecoveryWriteSequenceMatches

	report := cliReport{Seed: sample.Seed, SampleSchemas: schemas, CrossFormat: cross, ThreePaths: paths, AcceptanceChecks: checks}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return ioErr(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "verification-report.json"), append(b, '\n'), 0o644); err != nil {
		return ioErr(err)
	}
	fmt.Printf("bonerig verification complete: %s\n", filepath.Join(outDir, "verification-report.json"))
	for schema, sum := range cross {
		fmt.Printf("  %s frames=%d maxDev=%d maxUB=%d truthOK=%v\n", schema, sum.Frames, sum.MaxMeasuredL1, sum.MaxUpperBound, sum.WithinTruth)
	}
	for _, p := range paths.Paths {
		fmt.Printf("  path=%s sequence=%v writes=%v maxWorkers=%d waits=%d\n", p.Name, p.CanonicalSequence, p.WriteSequence, p.MaxWorkers, p.WaitCount)
	}
	return nil
}

func ioErr(err error) *bonerig.Error {
	return &bonerig.Error{Code: bonerig.CodeSchema, Message: err.Error(), Field: "io"}
}
