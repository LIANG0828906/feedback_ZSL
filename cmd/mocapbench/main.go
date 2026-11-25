package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mocapbench/internal/core"
	"mocapbench/internal/service"
	"mocapbench/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "generate":
		err = runGenerate(args)
	case "validate":
		err = runValidate(args)
	case "verify":
		err = runVerify(args)
	case "serve":
		err = runServe(args)
	case "version":
		fmt.Println("mocapbench", core.AlgorithmVersion, "sample-schema", core.SchemaVersion)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		if e, ok := err.(*core.Error); ok {
			data, _ := json.MarshalIndent(e, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`mocapbench commands:
  generate [-out samples] [-seed N]
  validate [-samples samples]
  verify   [-samples samples] [-clip ID] [-target ID] [-out reports/verification.json] [-workers 3] [-buffer 4] [-crash-after N]
  serve    [-samples samples] [-checkpoints checkpoints] [-addr 127.0.0.1:8080]`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "sample output directory")
	seed := fs.Int64("seed", core.DefaultSeed, "fixed deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := core.GenerateSampleSet(core.GeneratorOptions{OutDir: *out, Seed: *seed})
	if err != nil {
		return err
	}
	fmt.Printf("generated %d clips, %d targets, %d truth files in %s (seed=%d)\n",
		len(manifest.Clips), len(manifest.Targets), len(manifest.Truth), *out, manifest.Seed)
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	dir := fs.String("samples", "samples", "sample directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, lerr := core.LoadSampleSet(*dir)
	if lerr != nil {
		return lerr
	}
	allOK := true
	for _, decl := range set.Manifest.Clips {
		clip := set.Clips[decl.ID]
		truth := set.Truth[decl.ID]
		report := core.VerifyTruth(clip, truth)
		fmt.Printf("clip=%s joints=%d frames=%d schema=OK cycle=absent quat=OK truth_ok=%v world_max=%.3g\n",
			clip.ID, len(clip.Skeleton.Joints), len(clip.Frames), report.OK, report.MaxWorldError)
		if !report.OK {
			allOK = false
			for _, m := range report.Mismatches {
				fmt.Println("  mismatch:", m)
			}
		}
	}
	for id, target := range set.Targets {
		fmt.Printf("target=%s joints=%d height=%.4f\n", id, len(target.Joints), core.SkeletonHeight(target))
	}
	if !allOK {
		return core.NewError(core.ECodeSchema, "truth validation failed")
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	samplesDir := fs.String("samples", "samples", "sample directory")
	clipID := fs.String("clip", "", "clip id (default first)")
	targetID := fs.String("target", "", "target id (default first)")
	out := fs.String("out", "reports/verification.json", "JSON report output")
	workers := fs.Int("workers", 3, "maximum parallel frame workers")
	buffer := fs.Int("buffer", 4, "per-worker buffer limit")
	crashAfter := fs.Int("crash-after", 0, "simulated durable frames before restart")
	quantStep := fs.Float64("quant-step", core.DefaultParameters().QuantStep, "quaternion quantization step")
	driftLimit := fs.Float64("contact-drift-limit", core.DefaultParameters().ContactDriftLimit, "contact drift upper bound")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, serr := service.New(*samplesDir, "checkpoints")
	if serr != nil {
		return serr
	}
	params := core.DefaultParameters()
	params.QuantStep = *quantStep
	params.ContactDriftLimit = *driftLimit
	resources := core.ResourceConfig{Workers: *workers, BufferPerWorker: *buffer}
	report, verr := svc.Verify(*clipID, *targetID, params, resources, *crashAfter)
	if report != nil && *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err == nil {
			data, _ := json.MarshalIndent(report, "", "  ")
			_ = os.WriteFile(*out, data, 0o644)
		}
	}
	if verr != nil {
		return verr
	}
	fmt.Printf("VERIFY OK clip=%s target=%s frames=%d max_iterations=%d max_violation=%.3g max_drift=%.6f\n",
		report.ClipID, report.TargetID, report.Summary.Frames, report.Summary.MaxIterations,
		report.Summary.MaxViolation, report.Summary.MaxContactDrift)
	for _, line := range report.VerificationLog {
		fmt.Println(" -", line)
	}
	fmt.Println("report:", *out)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	samplesDir := fs.String("samples", "samples", "sample directory")
	checkpointDir := fs.String("checkpoints", "checkpoints", "SQLite checkpoint directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return web.ListenAndServe(*addr, *samplesDir, *checkpointDir)
}
