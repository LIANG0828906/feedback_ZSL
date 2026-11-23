package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
	"meshforge/internal/repair"
	"meshforge/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		if ce, ok := errs.As(err); ok {
			_ = json.NewEncoder(os.Stderr).Encode(ce)
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(exitCode(err))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `MeshForge - deterministic triangle mesh repair workbench

Commands:
  generate  Generate deterministic defective meshes and truth manifests
  run       Run the six-stage repair pipeline and write repaired mesh/checkpoints
  verify    Verify truth topology, exact/spatial consistency and parallel determinism
  serve     Serve the loopback-only visualization UI
  inspect   Print persisted pipeline state as JSON

Run "meshforge <command> -h" for command flags.`)
}

func exitCode(err error) int {
	ce, ok := errs.As(err)
	if !ok {
		return 1
	}
	switch ce.Code {
	case errs.CodeGeom:
		return 2
	case errs.CodeTolo:
		return 3
	case errs.CodePara:
		return 4
	case errs.CodeOrient:
		return 5
	case errs.CodeScale:
		return 6
	case errs.CodeState:
		return 7
	default:
		return 1
	}
}

type levelSpec struct {
	Level int
	ID    string
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "data", "output data directory")
	seed := fs.Int64("seed", 20261124, "deterministic seed")
	levels := fs.String("levels", "5,6,7", "comma-separated octahedron subdivision levels")
	if err := fs.Parse(args); err != nil {
		return err
	}
	specs, err := parseLevels(*levels)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	type summary struct {
		ID        string                    `json:"id"`
		Directory string                    `json:"directory"`
		Input     generator.Counts          `json:"input"`
		Truth     generator.Counts          `json:"truth"`
		Defects   generator.DiagnosticTruth `json:"defects"`
	}
	var all []summary
	for _, spec := range specs {
		start := time.Now()
		sampleSeed := *seed + int64(spec.Level*1009)
		s, err := generator.Generate(spec.Level, sampleSeed, spec.ID)
		if err != nil {
			return err
		}
		dir := filepath.Join(*out, spec.ID)
		if err := generator.SaveSample(dir, s); err != nil {
			return err
		}
		all = append(all, summary{
			ID: spec.ID, Directory: dir,
			Input: generator.Counts{
				Vertices: s.Truth.InputVertices, Faces: s.Truth.InputFaces,
				Edges: len(mesh.BuildTopology(s.Mesh).Edges),
				Euler: s.Truth.InputVertices - len(mesh.BuildTopology(s.Mesh).Edges) + s.Truth.InputFaces,
			},
			Truth: s.Truth.Repaired, Defects: s.Truth.Defects,
		})
		fmt.Printf("generated %-10s level=%d vertices=%d faces=%d defects=%+v elapsed=%s\n",
			spec.ID, spec.Level, len(s.Mesh.Vertices), len(s.Mesh.Faces), s.Truth.Defects, time.Since(start).Round(time.Millisecond))
	}
	raw, _ := json.MarshalIndent(all, "", "  ")
	return mesh.WriteFileAtomic(filepath.Join(*out, "samples.json"), append(raw, '\n'), 0o644)
}

func parseLevels(raw string) ([]levelSpec, error) {
	names := []string{"small", "medium", "large", "xlarge", "xxlarge"}
	var out []levelSpec
	for _, part := range strings.Split(raw, ",") {
		level, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || level < 2 || level > 9 {
			return nil, fmt.Errorf("invalid level %q", part)
		}
		name := fmt.Sprintf("level-%d", level)
		if level >= 5 && level-5 < len(names) {
			name = names[level-5]
		}
		out = append(out, levelSpec{Level: level, ID: name})
	}
	return out, nil
}

func addConfigFlags(fs *flag.FlagSet, cfg *repair.Config, mode *string) {
	fs.StringVar(mode, "mode", string(cfg.Mode), "auto|exact|spatial")
	fs.Float64Var(&cfg.WeldTolerance, "weld-tol", cfg.WeldTolerance, "weld spatial tolerance")
	fs.Float64Var(&cfg.MinComponentPct, "min-component-pct", cfg.MinComponentPct, "minimum auxiliary component face ratio")
	fs.Float64Var(&cfg.SmoothWeight, "smooth-weight", cfg.SmoothWeight, "Taubin/Laplacian positive weight")
	fs.IntVar(&cfg.SmoothIterations, "smooth-iterations", cfg.SmoothIterations, "Laplacian iterations")
	fs.IntVar(&cfg.FineFaceLimit, "fine-face-limit", cfg.FineFaceLimit, "auto mode exact face limit")
	fs.IntVar(&cfg.FineComponentLimit, "fine-component-limit", cfg.FineComponentLimit, "auto mode exact component limit")
	fs.IntVar(&cfg.ChunkTargetFaces, "chunk-target-faces", cfg.ChunkTargetFaces, "spatial mode target faces per chunk")
	fs.Int64Var(&cfg.TimeBudgetMS, "time-budget-ms", cfg.TimeBudgetMS, "estimated repair time budget")
	fs.Int64Var(&cfg.MemoryBudgetMB, "memory-budget-mb", cfg.MemoryBudgetMB, "estimated memory budget")
	fs.IntVar(&cfg.MaxParallel, "max-parallel", cfg.MaxParallel, "maximum stage worker parallelism")
	fs.Int64Var(&cfg.PerWorkerBufferMB, "per-worker-buffer-mb", cfg.PerWorkerBufferMB, "maximum face buffer per worker")
}

func finalizeConfig(cfg *repair.Config, mode string) error {
	cfg.Mode = repair.Mode(mode)
	*cfg = cfg.Normalized()
	if e := cfg.Validate(); e != nil {
		return e
	}
	return nil
}

type runReport struct {
	Sample        string               `json:"sample"`
	RunDirectory  string               `json:"run_directory"`
	CheckpointDir string               `json:"checkpoint_dir"`
	Algorithm     string               `json:"algorithm_version"`
	Parameter     string               `json:"parameter_version"`
	InputHash     string               `json:"input_hash"`
	OutputHash    string               `json:"output_hash,omitempty"`
	State         repair.PipelineState `json:"state"`
	Error         *errs.CodedError     `json:"error,omitempty"`
	RepairedMesh  string               `json:"repaired_mesh,omitempty"`
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sampleDir := fs.String("sample", "data/small", "sample directory")
	outDir := fs.String("out", "", "run output directory (default runs/<sample-id>)")
	cpDir := fs.String("checkpoint", "", "checkpoint directory (default <out>/checkpoints)")
	resume := fs.Bool("resume", true, "resume from a valid checkpoint")
	cfg := repair.DefaultConfig()
	mode := string(cfg.Mode)
	addConfigFlags(fs, &cfg, &mode)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := finalizeConfig(&cfg, mode); err != nil {
		return err
	}
	sample, err := generator.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	if *outDir == "" {
		*outDir = filepath.Join("runs", sample.Truth.ID)
	}
	if *cpDir == "" {
		*cpDir = filepath.Join(*outDir, "checkpoints")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	p, err := repair.NewPipeline(sample, cfg, *cpDir, *resume)
	if err != nil {
		return err
	}
	runErr := p.Run()
	state := p.State()
	report := runReport{
		Sample: *sampleDir, RunDirectory: *outDir, CheckpointDir: *cpDir,
		Algorithm: repair.AlgorithmVersion, Parameter: state.ParameterVersion,
		InputHash: state.InputHash, State: state,
	}
	if runErr != nil {
		if ce, ok := errs.As(runErr); ok {
			report.Error = ce
		} else {
			report.Error = errs.New(errs.CodeIO, "%v", runErr)
		}
	} else if final, ok := p.FinalMesh(); ok {
		path := filepath.Join(*outDir, "repaired.json")
		if err := mesh.SaveFile(path, final); err != nil {
			return err
		}
		report.OutputHash = final.ContentHash()
		report.RepairedMesh = path
	}
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := mesh.WriteFileAtomic(filepath.Join(*outDir, "run-report.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	fmt.Printf("repaired %s -> %s hash=%s mode=%s\n", sample.Truth.ID, report.RepairedMesh, report.OutputHash, state.Decision.Selected)
	return nil
}

type verification struct {
	SampleID    string                    `json:"sample_id"`
	Seed        int64                     `json:"seed"`
	Diagnostic  repair.Diagnostic         `json:"diagnostic"`
	Consistency *repair.ConsistencyReport `json:"consistency,omitempty"`
	Determinism *repair.DeterminismReport `json:"determinism,omitempty"`
	Error       *errs.CodedError          `json:"error,omitempty"`
	Passed      bool                      `json:"passed"`
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sampleDir := fs.String("sample", "data/small", "single sample directory")
	dataDir := fs.String("data", "", "verify every sample subdirectory in this directory")
	outFile := fs.String("out", "verification.json", "verification report path")
	cfg := repair.DefaultConfig()
	mode := string(cfg.Mode)
	addConfigFlags(fs, &cfg, &mode)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := finalizeConfig(&cfg, mode); err != nil {
		return err
	}
	var dirs []string
	if *dataDir != "" {
		entries, err := os.ReadDir(*dataDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(*dataDir, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, "mesh.json")); err == nil {
				dirs = append(dirs, dir)
			}
		}
	} else {
		dirs = []string{*sampleDir}
	}
	var reports []verification
	var firstErr error
	for _, dir := range dirs {
		sample, err := generator.LoadSample(dir)
		if err != nil {
			return err
		}
		v := verification{SampleID: sample.Truth.ID, Seed: sample.Truth.Seed}
		diag, de := repair.Diagnose(sample.Mesh, sample.Truth)
		if de != nil {
			v.Error = de
			firstErr = de
		} else {
			v.Diagnostic = diag
			if report, ce := repair.RunConsistency(sample, cfg); ce != nil {
				v.Consistency = report
				v.Error = ce
				firstErr = ce
			} else {
				v.Consistency = report
				if det, ce := repair.RunDeterminism(sample, cfg); ce != nil {
					v.Determinism = det
					v.Error = ce
					firstErr = ce
				} else {
					v.Determinism = det
					v.Passed = report.Passed && det.Equal
				}
			}
		}
		reports = append(reports, v)
		if v.Passed {
			fmt.Printf("verified %-10s consistency=%s determinism=%t\n", sample.Truth.ID, v.Consistency.Summary(), v.Determinism.Equal)
		} else {
			fmt.Printf("FAILED   %-10s %+v\n", sample.Truth.ID, v.Error)
		}
	}
	out := struct {
		AlgorithmVersion string         `json:"algorithm_version"`
		GeneratedAt      string         `json:"generated_at"`
		Passed           bool           `json:"passed"`
		Results          []verification `json:"results"`
	}{
		AlgorithmVersion: repair.AlgorithmVersion,
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Passed:           firstErr == nil,
		Results:          reports,
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	if err := mesh.WriteFileAtomic(*outFile, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("verification report: %s\n", *outFile)
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	sampleDir := fs.String("sample", "data/small", "sample directory")
	runDir := fs.String("run", "", "run/checkpoint directory")
	addr := fs.String("addr", "127.0.0.1:8787", "loopback listen address")
	autoRun := fs.Bool("auto-run", false, "run or resume before serving")
	cfg := repair.DefaultConfig()
	mode := string(cfg.Mode)
	addConfigFlags(fs, &cfg, &mode)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := finalizeConfig(&cfg, mode); err != nil {
		return err
	}
	if err := web.ValidateLoopback(*addr); err != nil {
		return errs.New(errs.CodePara, "invalid listen address: %v", err)
	}
	sample, err := generator.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	if *runDir == "" {
		*runDir = filepath.Join("runs", sample.Truth.ID)
	}
	cpDir := filepath.Join(*runDir, "checkpoints")
	srv, err := web.New(sample, cfg, cpDir)
	if err != nil {
		return err
	}
	if *autoRun {
		if err := srv.Pipeline.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "auto-run:", err)
		}
	}
	return srv.Serve(*addr)
}

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	sampleDir := fs.String("sample", "data/small", "sample directory")
	runDir := fs.String("run", "", "run directory")
	cfg := repair.DefaultConfig()
	mode := string(cfg.Mode)
	addConfigFlags(fs, &cfg, &mode)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := finalizeConfig(&cfg, mode); err != nil {
		return err
	}
	sample, err := generator.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	if *runDir == "" {
		*runDir = filepath.Join("runs", sample.Truth.ID)
	}
	p, err := repair.NewPipeline(sample, cfg, filepath.Join(*runDir, "checkpoints"), true)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(p.State())
}
