package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"semanticshading/internal/checkpoint"
	"semanticshading/internal/dashboard"
	"semanticshading/internal/diagnostics"
	"semanticshading/internal/model"
	"semanticshading/internal/pipeline"
	"semanticshading/internal/sample"
	"semanticshading/internal/verify"
)

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

func main() {
	if err := run(os.Args[1:]); err != nil {
		var coded *exitCodeError
		if errors.As(err, &coded) {
			fmt.Fprintln(os.Stderr, "验证失败:", coded.err)
			os.Exit(coded.code)
		}
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "demo":
		return demo(args[1:])
	case "serve":
		return serve(args[1:])
	case "verify":
		return verifyCmd(args[1:])
	case "generate":
		return generate(args[1:])
	case "preflight":
		return preflight(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Println(`ssw - semantic shading workbench

commands:
  demo       generate deterministic sample, run 3 paths, verify and write dashboard state
  generate   write manifest/truth/raw textures to artifacts
  verify     independently verify a SQLite checkpoint against truth JSON
  serve      start local loopback diagnostics dashboard
  preflight  check writable directory, Go target and optionally run go build ./...`)
}

func demo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	dataDir := fs.String("data", filepath.Join("data", "checkpoints"), "checkpoint directory")
	artifactDir := fs.String("artifacts", "artifacts", "artifact directory")
	addr := fs.String("addr", "127.0.0.1:18080", "loopback listen address")
	startServer := fs.Bool("serve", false, "start dashboard after demo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	generated, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		return err
	}
	if err := writeSampleArtifacts(*artifactDir, generated); err != nil {
		return err
	}
	report, err := pipeline.RunThreePaths(ctx, generated, pipeline.Options{Dir: *dataDir, VerifyEachPath: true})
	if err != nil {
		return err
	}
	injections := diagnostics.RunAll(ctx, filepath.Join(filepath.Dir(*dataDir), "diagnostics"))
	state := &dashboard.State{
		Title:          "游戏贴图语义着色一致性工作台",
		GeneratedAt:    time.Now(),
		Pipeline:       report,
		Injections:     injections,
		DiagnosticsDir: filepath.Join(filepath.Dir(*dataDir), "diagnostics"),
	}
	statePath := filepath.Join(*artifactDir, "dashboard-state.json")
	if err := dashboard.SaveState(statePath, state); err != nil {
		return err
	}
	reportPath := filepath.Join(*artifactDir, "three-path-report.json")
	if err := writeJSON(reportPath, report); err != nil {
		return err
	}

	fmt.Printf("样本: %s %dx%d ch=%d tile=%d gear=%d\n", generated.Manifest.Name, generated.Manifest.Width, generated.Manifest.Height, generated.Manifest.Channels, generated.Manifest.TileSize, generated.Manifest.QuantGear)
	fmt.Printf("三路径一致: %v；区域数: %d；提交序列: %v\n", report.Comparison.OK, len(report.Full.Regions), report.Full.RunReport.CommitSequence)
	if report.Verification != nil {
		fmt.Printf("独立验证: ok=%v mismatches=%d\n", report.Verification.OK, len(report.Verification.Mismatches))
	}
	for _, injection := range injections {
		fmt.Printf("注入 %-8s expected=%-8s observed=%-8s ok=%v\n", injection.Kind, injection.ExpectedCode, injection.ObservedCode, injection.OK)
	}
	fmt.Printf("面板状态: %s\n", statePath)
	if !report.Comparison.OK || (report.Verification != nil && !report.Verification.OK) {
		return &exitCodeError{code: 2, err: errors.New("consistency or verification failed")}
	}
	if *startServer {
		fmt.Printf("Serving on http://%s/\n", *addr)
		return dashboard.NewServer(state, state.DiagnosticsDir).ListenAndServe(*addr)
	}
	fmt.Printf("启动面板: go run ./cmd/ssw serve --state %s --addr %s\n", statePath, *addr)
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:18080", "loopback listen address")
	statePath := fs.String("state", filepath.Join("artifacts", "dashboard-state.json"), "dashboard state JSON")
	dataDir := fs.String("data", filepath.Join("data", "checkpoints"), "checkpoint directory used if state is absent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	state, err := dashboard.LoadState(*statePath)
	if err != nil {
		ctx := context.Background()
		state, err = dashboard.BuildState(ctx, *dataDir)
		if err != nil {
			return err
		}
		_ = dashboard.SaveState(*statePath, state)
	}
	fmt.Printf("Semantic Shading Workbench: http://%s/\n", *addr)
	return dashboard.NewServer(state, filepath.Join(filepath.Dir(*dataDir), "diagnostics")).ListenAndServe(*addr)
}

func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dbPath := fs.String("db", filepath.Join("data", "checkpoints", "resume.sqlite"), "SQLite checkpoint path")
	truthPath := fs.String("truth", filepath.Join("artifacts", "truth.json"), "truth JSON path")
	outPath := fs.String("out", "", "optional JSON output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	truthData, err := os.ReadFile(*truthPath)
	if err != nil {
		return err
	}
	var truth model.Truth
	if err := json.Unmarshal(truthData, &truth); err != nil {
		return fmt.Errorf("decode truth: %w", err)
	}
	store, err := checkpoint.OpenExisting(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := verify.Verify(context.Background(), store, truth)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			return err
		}
	} else {
		fmt.Println(string(data))
	}
	if !report.OK {
		if report.First != nil {
			return &exitCodeError{code: 2, err: fmt.Errorf("first mismatch region=%d block=%d field=%s code=%s", report.First.RegionID, report.First.BlockIndex, report.First.Field, report.First.Code)}
		}
		return &exitCodeError{code: 2, err: errors.New("verification failed")}
	}
	fmt.Fprintln(os.Stderr, "independent verification OK")
	return nil
}

func generate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	outDir := fs.String("out", "artifacts", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	generated, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		return err
	}
	return writeSampleArtifacts(*outDir, generated)
}

func writeSampleArtifacts(dir string, s *sample.Sample) error {
	if err := checkpoint.PrepareDir(dir); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), s.Manifest); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "truth.json"), s.Truth); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "reference.rgba"), s.Ref, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "reconstructed.rgba"), s.Recon, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "labels.uint8"), s.Labels, 0o644)
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func preflight(args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	dir := fs.String("dir", filepath.Join("data", "checkpoints"), "persistent directory")
	runBuild := fs.Bool("build", false, "run go build ./...")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkpoint.PrepareDir(*dir); err != nil {
		return fmt.Errorf("persistent directory is not writable: %w", err)
	}
	out, err := exec.Command("go", "env", "GOOS").CombinedOutput()
	if err != nil {
		return fmt.Errorf("query go env GOOS: %w", err)
	}
	fmt.Printf("persistent dir writable: %s\nGOOS=%s", *dir, string(out))
	if *runBuild {
		cmd := exec.Command("go", "build", "./...")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		fmt.Println("go build ./... passed")
	}
	return nil
}
