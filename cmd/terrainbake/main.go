package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"terrainbake"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		emitError(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError("a command is required: gen, validate, bake, verify, truth or demo")
	}
	switch args[0] {
	case "gen":
		return runGen(args[1:])
	case "validate":
		return runValidate(args[1:])
	case "bake":
		return runBake(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "truth":
		return runTruth(args[1:])
	case "demo":
		return runDemo(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return usageError("unknown command %q", args[0])
	}
}

func runGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	out := fs.String("out", "sample", "output directory")
	if err := fs.Parse(args); err != nil {
		return usageError("%v", err)
	}
	p, err := terrainbake.GenerateProject(*seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return &terrainbake.Error{Code: terrainbake.CodeIO, Object: "gen", Field: "out", Message: err.Error()}
	}
	path := filepath.Join(*out, "project.json")
	if err := terrainbake.SaveProject(path, p); err != nil {
		return err
	}
	return writeJSON(map[string]any{
		"project": path, "seed": p.Seed, "blocks": len(p.Blocks),
		"materials": len(p.Materials), "seams": len(p.Seams),
		"truth_samples": len(p.Truth) + len(p.SeamTruth),
	})
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	projectPath := fs.String("project", "", "project JSON path")
	if err := fs.Parse(args); err != nil {
		return usageError("%v", err)
	}
	if *projectPath == "" {
		return usageError("--project is required")
	}
	p, err := terrainbake.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	return writeJSON(map[string]any{
		"valid": true, "project": *projectPath, "name": p.Name, "seed": p.Seed,
		"blocks": len(p.Blocks), "seams": len(p.Seams),
		"truth_samples": len(p.Truth) + len(p.SeamTruth),
	})
}

func runBake(args []string) error {
	fs := flag.NewFlagSet("bake", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	projectPath := fs.String("project", "", "project JSON path")
	mode := fs.String("mode", "full", "full, incremental or resume")
	editsPath := fs.String("edits", "", "incremental edit JSON path")
	projectOut := fs.String("project-out", "", "write the post-edit project snapshot here")
	reportPath := fs.String("report", "", "write HTML diagnostics to this directory")
	opts := addEngineFlags(fs, "store")
	if err := fs.Parse(args); err != nil {
		return usageError("%v", err)
	}
	if *projectPath == "" {
		return usageError("--project is required")
	}
	p, err := terrainbake.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	engine, err := terrainbake.NewEngine(p, *opts)
	if err != nil {
		return err
	}
	defer engine.Close()
	ctx := context.Background()
	var result *terrainbake.BakeResult
	switch *mode {
	case "full":
		result, err = engine.FullBake(ctx)
	case "incremental":
		if *editsPath == "" {
			return usageError("--edits is required for incremental mode")
		}
		edits, readErr := readEdits(*editsPath)
		if readErr != nil {
			return readErr
		}
		result, err = engine.IncrementalBake(ctx, edits)
	case "resume", "retry":
		result, err = engine.Resume(ctx)
	default:
		return usageError("unsupported mode %q", *mode)
	}
	if *projectOut != "" {
		edited, snapErr := engine.ProjectSnapshot()
		if snapErr == nil {
			snapErr = terrainbake.SaveProject(*projectOut, edited)
		}
		if snapErr != nil {
			if result != nil {
				_ = writeJSON(result)
			}
			return snapErr
		}
	}
	if result != nil {
		if *reportPath != "" {
			outputs, outErr := engine.Outputs()
			if outErr != nil && !result.Paused {
				return outErr
			}
			diagProject, _ := engine.ProjectSnapshot()
			if diagProject == nil {
				diagProject = p
			}
			diagnosticsIn := terrainbake.DiagnosticInput{
				Title: "Terrain bake diagnostics", Project: diagProject, Result: result, Outputs: outputs,
			}
			if _, repErr := terrainbake.WriteDiagnostics(*reportPath, diagnosticsIn); repErr != nil {
				return repErr
			}
		}
		if outErr := writeJSON(result); outErr != nil {
			return outErr
		}
	}
	return err
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	projectPath := fs.String("project", "", "project JSON path")
	editsPath := fs.String("edits", "", "edit sequence JSON path")
	root := fs.String("root", "verify-run", "working directory for three independent stores")
	outPath := fs.String("out", "", "optional verification report JSON path")
	opts := addEngineFlags(fs, "verify-store")
	if err := fs.Parse(args); err != nil {
		return usageError("%v", err)
	}
	if *projectPath == "" {
		return usageError("--project is required")
	}
	p, err := terrainbake.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	var edits []terrainbake.Edit
	if *editsPath != "" {
		edits, err = readEdits(*editsPath)
		if err != nil {
			return err
		}
	}
	report, verifyErr := terrainbake.VerifyThreePaths(context.Background(), p, edits, *root, *opts)
	if report != nil {
		if *outPath != "" {
			if err := writeJSONFile(*outPath, report); err != nil {
				return err
			}
		}
		if err := writeJSON(report); err != nil {
			return err
		}
	}
	return verifyErr
}

func runTruth(args []string) error {
	fs := flag.NewFlagSet("truth", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	projectPath := fs.String("project", "", "project JSON path")
	opts := addEngineFlags(fs, "")
	if err := fs.Parse(args); err != nil {
		return usageError("%v", err)
	}
	if *projectPath == "" {
		return usageError("--project is required")
	}
	p, err := terrainbake.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	if opts.StoreDir == "" {
		dir, mkErr := os.MkdirTemp("", "terrainbake-truth-*")
		if mkErr != nil {
			return &terrainbake.Error{Code: terrainbake.CodeIO, Object: "truth", Field: "store", Message: mkErr.Error()}
		}
		defer os.RemoveAll(dir)
		opts.StoreDir = dir
	}
	engine, err := terrainbake.NewEngine(p, *opts)
	if err != nil {
		return err
	}
	defer engine.Close()
	if _, err := engine.FullBake(context.Background()); err != nil {
		return err
	}
	checked, err := engine.VerifyTruth()
	if err != nil {
		return err
	}
	return writeJSON(map[string]any{"truth_checked": checked, "equal": true, "content_hash": terrainbake.ProjectHash(p)})
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "demo-out", "demo output directory")
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	opts := addEngineFlags(fs, "demo-store")
	if err := fs.Parse(args); err != nil {
		return usageError("%v", err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return &terrainbake.Error{Code: terrainbake.CodeIO, Object: "demo", Field: "out", Message: err.Error()}
	}
	p, err := terrainbake.GenerateProject(*seed)
	if err != nil {
		return err
	}
	baselinePath := filepath.Join(*out, "project.json")
	if err := terrainbake.SaveProject(baselinePath, p); err != nil {
		return err
	}
	edits := []terrainbake.Edit{
		{Kind: terrainbake.EditHeight, BlockID: "tl", X: 6, Y: 5, Value: 5.35},
		{Kind: terrainbake.EditMaterial, MaterialID: "grass", Field: "normal_scale", Value: 1.1},
	}
	edited, err := cloneProjectForCLI(p)
	if err != nil {
		return err
	}
	if _, err := terrainbake.ApplyEdits(edited, terrainbake.BuildDependencyGraph(edited), edits); err != nil {
		return err
	}
	editedPath := filepath.Join(*out, "project-edited.json")
	if err := terrainbake.SaveProject(editedPath, edited); err != nil {
		return err
	}

	baseStore := filepath.Join(*out, "baseline-store")
	baseOpts := *opts
	baseOpts.StoreDir = baseStore
	baseOpts.PauseAfter = 0
	baseEngine, err := terrainbake.NewEngine(p, baseOpts)
	if err != nil {
		return err
	}
	if _, err := baseEngine.FullBake(context.Background()); err != nil {
		return err
	}
	truthChecked, err := baseEngine.VerifyTruth()
	if err != nil {
		return err
	}

	verifyOpts := *opts
	verifyOpts.StoreDir = "demo-verify"
	verifyOpts.PauseAfter = 0
	report, verifyErr := terrainbake.VerifyThreePaths(context.Background(), p, edits, filepath.Join(*out, "three-path"), verifyOpts)
	if report != nil {
		if err := writeJSONFile(filepath.Join(*out, "verification.json"), report); err != nil {
			return err
		}
	}
	if verifyErr != nil {
		return verifyErr
	}

	fullOpts := *opts
	fullOpts.StoreDir = filepath.Join(*out, "edited-store")
	fullOpts.PauseAfter = 0
	fullEngine, err := terrainbake.NewEngine(edited, fullOpts)
	if err != nil {
		return err
	}
	fullResult, err := fullEngine.FullBake(context.Background())
	if err != nil {
		return err
	}
	outputs, err := fullEngine.Outputs()
	if err != nil {
		return err
	}
	diff, _ := terrainbake.CompareSnapshots(outputs, outputs, fullResult.SeamStats, fullResult.SeamStats, fullResult.CommitSequence, fullResult.CommitSequence)
	reportPath, err := terrainbake.WriteDiagnostics(filepath.Join(*out, "report"), terrainbake.DiagnosticInput{
		Title: "Terrain normal-map bake demo", Project: edited, Result: fullResult, Outputs: outputs, Diff: diff,
	})
	if err != nil {
		return err
	}
	return writeJSON(map[string]any{
		"ok":                true,
		"seed":              *seed,
		"baseline_project":  baselinePath,
		"edited_project":    editedPath,
		"verification":      filepath.Join(*out, "verification.json"),
		"diagnostics":       reportPath,
		"three_paths_equal": report.Equal,
		"truth_checked":     truthChecked,
		"blocks":            len(p.Blocks),
		"edits":             edits,
	})
}

func addEngineFlags(fs *flag.FlagSet, storeDefault string) *terrainbake.EngineOptions {
	opts := terrainbake.DefaultEngineOptions(storeDefault)
	fs.StringVar(&opts.StoreDir, "store", storeDefault, "checkpoint directory")
	fs.IntVar(&opts.MaxWorkers, "workers", opts.MaxWorkers, "maximum parallel bake workers")
	fs.IntVar(&opts.MaxThreadPixels, "thread-pixels", opts.MaxThreadPixels, "maximum pixels in one thread buffer")
	fs.Int64Var(&opts.NormalMapMemoryBytes, "memory-budget", opts.NormalMapMemoryBytes, "normal map memory budget in bytes")
	fs.IntVar(&opts.PauseAfter, "pause-after", 0, "pause after N committed blocks")
	fs.StringVar(&opts.Params.Operator, "operator", opts.Params.Operator, "differential operator: sobel, central or forward")
	fs.Float64Var(&opts.Params.Scale, "scale", opts.Params.Scale, "height-to-normal scale")
	return &opts
}

func readEdits(path string) ([]terrainbake.Edit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &terrainbake.Error{Code: terrainbake.CodeIO, Object: "edits", Field: "path", Message: err.Error()}
	}
	var edits []terrainbake.Edit
	if err := json.Unmarshal(data, &edits); err != nil {
		return nil, &terrainbake.Error{Code: terrainbake.CodeSchema, Object: "edits", Field: "json", Message: err.Error()}
	}
	return edits, nil
}

func cloneProjectForCLI(p *terrainbake.Project) (*terrainbake.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out terrainbake.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &terrainbake.Error{Code: terrainbake.CodeIO, Object: "json", Field: "path", Message: err.Error()}
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func emitError(err error) {
	var payload any
	if be, ok := terrainbake.AsError(err); ok {
		payload = be
	} else {
		payload = &terrainbake.Error{Code: terrainbake.CodeIO, Object: "cli", Field: "runtime", Message: err.Error()}
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Fprintln(os.Stderr, string(data))
}

func usageError(format string, args ...any) error {
	return &terrainbake.Error{
		Code: terrainbake.CodeParam, Object: "cli", Field: "arguments",
		Message: fmt.Sprintf(format, args...),
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `terrainbake - deterministic terrain normal-map baker

  terrainbake gen      --seed 42 --out sample
  terrainbake validate --project sample/project.json
  terrainbake bake     --project sample/project.json --store cache --mode full
  terrainbake bake     --project sample/project.json --store cache --mode incremental --edits edits.json --project-out edited.json
  terrainbake bake     --project edited.json --store cache --mode resume
  terrainbake verify   --project sample/project.json --edits edits.json --root verify-run
  terrainbake truth    --project sample/project.json
  terrainbake demo     --out demo-out`)
}
