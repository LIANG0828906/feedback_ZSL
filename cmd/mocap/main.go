package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/pipeline"
	"mocapworkbench/internal/retarget"
	"mocapworkbench/internal/sample"
	"mocapworkbench/internal/verify"
	"mocapworkbench/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "verify-manifest":
		err = runVerifyManifest(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "run":
		err = runRetarget(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "version":
		fmt.Printf("mocap-workbench %s schema=%s generator=%s\n", model.AlgorithmVersion, model.SchemaVersion, model.GeneratorVersion)
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		writeError(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `mocap-workbench

Usage:
  mocap generate -out artifacts/samples
  mocap verify-manifest -path artifacts/samples/manifest.json
  mocap validate -clip CLIP.json -target TARGET.json
  mocap run -clip CLIP.json -target TARGET.json -mode auto -db artifacts/run/checkpoint.db -out artifacts/run/animation.json
  mocap run ... -stop-after 20          # normal stop, then rerun without it
  mocap verify -clip CLIP.json -target TARGET.json -mode auto -out artifacts/run/verify.json
  mocap serve -clip CLIP.json -target TARGET.json -mode auto -addr 127.0.0.1:8088`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "artifacts/samples", "sample output directory")
	seed := fs.Int64("seed", 0, "deterministic seed override (0 uses built-in seed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := sample.Generate(*out, *seed)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d deterministic files in %s (seed=%d)\n", len(manifest.Files), *out, manifest.Seed)
	for _, entry := range manifest.Files {
		fmt.Printf("  %-45s %s\n", entry.Path, entry.SHA256[:12])
	}
	return nil
}

func runVerifyManifest(args []string) error {
	fs := flag.NewFlagSet("verify-manifest", flag.ContinueOnError)
	path := fs.String("path", "artifacts/samples/manifest.json", "manifest path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := loader.VerifyManifest(*path); err != nil {
		return err
	}
	fmt.Printf("manifest verified: %s\n", *path)
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	clipPath := fs.String("clip", "", "clip JSON")
	targetPath := fs.String("target", "", "target skeleton JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clip, err := loader.LoadClip(*clipPath)
	if err != nil {
		return err
	}
	target, err := loader.LoadTarget(*targetPath)
	if err != nil {
		return err
	}
	if err := loader.ValidateTarget(target, clip.Skeleton); err != nil {
		return err
	}
	if err := retarget.ValidateContactMarkers(clip, target); err != nil {
		return err
	}
	if _, err := retarget.BuildMapping(clip.Skeleton, target); err != nil {
		return err
	}
	result := map[string]any{
		"ok": true, "clip_id": clip.ID, "target_id": target.ID,
		"frames": len(clip.Frames), "source_joints": len(clip.Skeleton.Joints),
		"target_joints": len(target.Joints),
	}
	return printJSON(result)
}

func runRetarget(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	clipPath := fs.String("clip", "", "clip JSON")
	targetPath := fs.String("target", "", "target skeleton JSON")
	dbPath := fs.String("db", "artifacts/run/checkpoint.db", "SQLite checkpoint path; empty disables persistence")
	outPath := fs.String("out", "", "write animation JSON")
	stopAfter := fs.Int("stop-after", 0, "normal stop after N total committed frames")
	forceFull := fs.Bool("full", false, "discard snapshot and fully recompute")
	cfg := addConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	clip, target, err := loadInputs(*clipPath, *targetPath)
	if err != nil {
		return err
	}
	animation, err := pipeline.Run(clip, target, *cfg, pipeline.Options{
		StorePath: *dbPath, StopAfter: *stopAfter, ForceFull: *forceFull,
	})
	if err != nil {
		return err
	}
	if err := writeJSONFile(*outPath, animation); err != nil {
		return err
	}
	summary := map[string]any{
		"ok": true, "clip_id": animation.ClipID, "target_id": animation.TargetID,
		"mode": animation.Plan.Mode, "strategy": animation.Plan.Strategy,
		"frames": len(animation.Frames), "resumed_frames": animation.ResumedFrames,
		"snapshot_action":   animation.SnapshotAction,
		"max_residual":      animation.Summary.MaxResidual,
		"max_contact_drift": animation.Summary.MaxContactDrift,
	}
	if *outPath != "" {
		summary["out"] = *outPath
	}
	return printJSON(summary)
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	clipPath := fs.String("clip", "", "clip JSON")
	targetPath := fs.String("target", "", "target skeleton JSON")
	dbPath := fs.String("db", "artifacts/run/verify.db", "temporary resume database")
	outPath := fs.String("out", "", "write verification report JSON")
	cfg := addConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	clip, target, err := loadInputs(*clipPath, *targetPath)
	if err != nil {
		return err
	}
	report, runErr := verify.Run(clip, target, *cfg, *dbPath)
	if report != nil {
		_ = writeJSONFile(*outPath, report)
	}
	if runErr != nil {
		return runErr
	}
	return printJSON(report)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	clipPath := fs.String("clip", "artifacts/samples/clips/clip_short.json", "clip JSON")
	targetPath := fs.String("target", "artifacts/samples/targets/target_tall.json", "target skeleton JSON")
	dbPath := fs.String("db", "artifacts/run/dashboard.db", "checkpoint database")
	addr := fs.String("addr", "127.0.0.1:8088", "HTTP listen address")
	cfg := addConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	clip, target, err := loadInputs(*clipPath, *targetPath)
	if err != nil {
		return err
	}
	animation, err := pipeline.Run(clip, target, *cfg, pipeline.Options{StorePath: *dbPath})
	if err != nil {
		return err
	}
	exactCfg := *cfg
	exactCfg.Mode = "exact"
	exact, err := retarget.Run(clip, target, exactCfg)
	if err != nil {
		return err
	}
	report, verifyErr := verify.Run(clip, target, *cfg, filepath.Join(filepath.Dir(*dbPath), "verify-panel.db"))
	if verifyErr != nil {
		return verifyErr
	}
	server := web.New(clip, target, animation, exact, report, *dbPath, *cfg)
	fmt.Printf("dashboard: http://%s (mode=%s frames=%d)\n", *addr, animation.Plan.Mode, len(animation.Frames))
	return http.ListenAndServe(*addr, server.Handler())
}

func addConfigFlags(fs *flag.FlagSet) *model.Config {
	cfg := model.DefaultConfig()
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "auto|exact|sampled")
	fs.BoolVar(&cfg.Preview, "preview", cfg.Preview, "prefer low-latency sampled plan")
	fs.IntVar(&cfg.SmoothWindow, "smooth-window", cfg.SmoothWindow, "temporal smoothing window in frames")
	fs.IntVar(&cfg.MaxIterations, "max-iterations", cfg.MaxIterations, "maximum projection iterations")
	fs.Float64Var(&cfg.AngleToleranceDeg, "angle-tolerance", cfg.AngleToleranceDeg, "angle constraint tolerance (degrees)")
	fs.Float64Var(&cfg.ReachTolerance, "reach-tolerance", cfg.ReachTolerance, "end-effector reach tolerance (metres)")
	fs.IntVar(&cfg.Budget.MaxExactFrames, "max-exact-frames", cfg.Budget.MaxExactFrames, "exact-mode frame budget")
	fs.IntVar(&cfg.Budget.MaxExactJoints, "max-exact-joints", cfg.Budget.MaxExactJoints, "exact-mode joint budget")
	fs.IntVar(&cfg.Budget.MaxExactWork, "max-exact-work", cfg.Budget.MaxExactWork, "exact-mode work-unit budget")
	fs.IntVar(&cfg.Budget.MaxSolveFrames, "max-solve-frames", cfg.Budget.MaxSolveFrames, "sampled-mode keyframe budget")
	fs.IntVar(&cfg.Budget.MaxSmoothWindow, "max-smooth-window", cfg.Budget.MaxSmoothWindow, "maximum legal smoothing window")
	return &cfg
}

func loadInputs(clipPath, targetPath string) (model.Clip, model.TargetSkeleton, error) {
	if clipPath == "" || targetPath == "" {
		return model.Clip{}, model.TargetSkeleton{}, model.NewError(model.ErrParam, "input", "clip/target", "both -clip and -target are required")
	}
	clip, err := loader.LoadClip(clipPath)
	if err != nil {
		return clip, model.TargetSkeleton{}, err
	}
	target, err := loader.LoadTarget(targetPath)
	if err != nil {
		return clip, target, err
	}
	return clip, target, nil
}

func writeJSONFile(path string, value any) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func printJSON(value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func writeError(err error) {
	var appErr *model.AppError
	payload := map[string]any{"ok": false, "error": err.Error()}
	if errors.As(err, &appErr) {
		payload["code"] = appErr.Code
		payload["detail"] = appErr
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Fprintln(os.Stderr, string(data))
}
