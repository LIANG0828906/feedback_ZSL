package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/checkpoint"
	"loudnessworkbench/engine"
	"loudnessworkbench/project"
	"loudnessworkbench/samplegen"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = commandGenerate(os.Args[2:])
	case "inspect":
		err = commandInspect(os.Args[2:])
	case "run":
		err = commandRun(os.Args[2:])
	case "verify":
		err = commandVerify(os.Args[2:])
	case "compare":
		err = commandCompare(os.Args[2:])
	case "serve":
		err = commandServe(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, apperr.JSON(err))
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `loudness-wb - broadcast loudness and dynamics workbench

Commands:
  generate  -out DIR [-seed N]
  inspect   -project FILE [-manifest FILE]
  run       -project FILE -manifest FILE -out DIR [-checkpoint DIR] [-mode full|realtime|resume] [-parallel N] [-buffer-bytes N]
  verify    -data DIR [-out FILE] [-parallel N]
  compare   -left FILE.wav -right FILE.wav -manifest FILE
  serve     -data DIR [-addr 127.0.0.1:8080] [-version v1|v2|v3]`)
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

func commandGenerate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	outDir := flags.String("out", "demo", "output directory")
	seed := flags.Int64("seed", samplegen.DefaultSeed, "deterministic generator seed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	result, err := samplegen.Generate(*outDir, *seed)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"manifest":       result.ManifestPath,
		"projects":       result.ProjectPaths,
		"program_id":     result.Manifest.ProgramID,
		"sample_rate_hz": result.Manifest.SampleRateHz,
		"segments":       len(result.Manifest.Segments),
		"seed":           result.Manifest.Seed,
	})
}

func commandInspect(args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	projectPath := flags.String("project", "", "project file")
	manifestPath := flags.String("manifest", "", "optional manifest file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *projectPath == "" {
		return apperr.New(apperr.CodeParameter, "-project is required")
	}
	var program *project.Program
	var err error
	if *manifestPath != "" {
		program, err = project.LoadWithManifest(*projectPath, *manifestPath)
	} else {
		program, err = project.Load(*projectPath)
	}
	if err != nil {
		return err
	}
	segments := make([]map[string]any, len(program.Segments))
	for i, segment := range program.Segments {
		segments[i] = map[string]any{
			"id": segment.ID, "channels": segment.Channels, "frames": segment.Frames(),
			"source_hash": segment.ContentSHA256,
		}
	}
	return printJSON(map[string]any{
		"project_id":      program.ProjectID,
		"source_version":  program.SourceVersion,
		"sample_rate_hz":  program.SampleRateHz,
		"target_lufs":     program.TargetLUFS,
		"frame_ms":        program.FrameMs,
		"hop_ms":          program.HopMs,
		"gate":            program.Gate,
		"compressor":      program.Compressor,
		"limiter":         program.Limiter,
		"stages":          program.StageOrder,
		"parameters_hash": program.DecisionParamsHash(),
		"segments":        segments,
	})
}

func commandRun(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	projectPath := flags.String("project", "", "project file")
	manifestPath := flags.String("manifest", "", "manifest file")
	outDir := flags.String("out", "out", "output directory")
	checkpointDir := flags.String("checkpoint", "", "checkpoint directory")
	mode := flags.String("mode", "full", "full, realtime or resume")
	parallel := flags.Int("parallel", 4, "maximum parallel segment workers")
	bufferBytes := flags.Int64("buffer-bytes", 16<<20, "per-worker buffer cap in bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *projectPath == "" || *manifestPath == "" || *outDir == "" {
		return apperr.New(apperr.CodeParameter, "-project, -manifest and -out are required")
	}
	manifest, err := project.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	program, err := project.LoadWithManifest(*projectPath, *manifestPath)
	if err != nil {
		return err
	}
	if *checkpointDir == "" {
		*checkpointDir = filepath.Join(*outDir, "checkpoints")
	}
	store := checkpoint.NewStore(*checkpointDir, program.ProjectID)
	session, err := engine.NewSession(program, store)
	if err != nil {
		return err
	}
	options := engine.RunOptions{
		Mode:                 engine.Mode(*mode),
		MaxParallel:          *parallel,
		PerWorkerBufferBytes: *bufferBytes,
	}
	result, runErr := session.Run(context.Background(), options)
	var truthReport *engine.TruthReport
	var truthErr error
	if result != nil {
		truthReport, truthErr = engine.VerifyTruth(result, manifest)
	}
	if result != nil {
		sweep := engine.ToleranceSweep(result, manifest, []float64{0.2, 0.1, 0.05, 0.01, 0.001})
		if err := writeRunArtifacts(*outDir, manifest, result, truthReport, sweep); err != nil {
			return err
		}
	}
	if runErr != nil {
		return runErr
	}
	summary := map[string]any{
		"mode": result.Mode, "mix_hash": result.MixHash, "frames": result.MixFrames,
		"program_loudness_lufs":       result.CompressedIntegratedLUFS,
		"final_program_loudness_lufs": result.FinalProgramLoudnessLUFS,
		"normalization_iterations":    result.NormalizationIterations,
		"normalization_converged":     result.NormalizationConverged,
		"global_gain_db":              result.GlobalGainDB, "segments": len(result.Segments),
		"truth_verification": truthReport,
	}
	if truthErr != nil {
		return truthErr
	}
	return printJSON(summary)
}

func commandCompare(args []string) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	leftPath := flags.String("left", "", "left WAV")
	rightPath := flags.String("right", "", "right WAV")
	manifestPath := flags.String("manifest", "", "manifest for segment boundaries")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *leftPath == "" || *rightPath == "" || *manifestPath == "" {
		return apperr.New(apperr.CodeParameter, "-left, -right and -manifest are required")
	}
	manifest, err := project.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	left, err := audio.ReadWAV(*leftPath)
	if err != nil {
		return err
	}
	right, err := audio.ReadWAV(*rightPath)
	if err != nil {
		return err
	}
	spans := manifestSpans(manifest)
	report, compareErr := engine.CompareSignals(left, right, spans)
	if err := printJSON(report); err != nil {
		return err
	}
	return compareErr
}

func writeRunArtifacts(outDir string, manifest *project.Manifest, result *engine.Result, truth *engine.TruthReport, sweep []engine.TolerancePoint) error {
	if err := os.MkdirAll(filepath.Join(outDir, "segments"), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot create output directory", err)
	}
	for _, segment := range result.Segments {
		if segment.Output.Frames() == 0 {
			continue
		}
		path := filepath.Join(outDir, "segments", segment.ID+".wav")
		if err := audio.WriteWAV(path, segment.Output); err != nil {
			return err
		}
	}
	if result.MixFrames > 0 {
		if err := audio.WriteWAV(filepath.Join(outDir, "mix.wav"), result.Mix); err != nil {
			return err
		}
	}
	report := map[string]any{
		"result": result, "truth_verification": truth, "tolerance_sweep": sweep,
		"mix": map[string]any{
			"path": "mix.wav", "hash": result.MixHash, "frames": result.MixFrames,
			"channels": result.MixChannels, "sample_rate_hz": result.Mix.SampleRateHz,
		},
		"manifest": manifest,
	}
	return writeJSONFile(filepath.Join(outDir, "report.json"), report)
}

func manifestSpans(manifest *project.Manifest) []engine.MixSpan {
	spans := make([]engine.MixSpan, len(manifest.Segments))
	cursor := int64(0)
	for i, segment := range manifest.Segments {
		spans[i] = engine.MixSpan{
			Index: i, ID: segment.ID, StartFrame: cursor,
			FrameCount: int64(segment.FrameCount), Channels: segment.Channels,
		}
		cursor += int64(segment.FrameCount)
	}
	return spans
}

func printJSON(value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot encode JSON output", err)
	}
	fmt.Println(string(data))
	return nil
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot encode JSON file", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot create output directory", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot write JSON file", err).WithPath(path)
	}
	return nil
}
