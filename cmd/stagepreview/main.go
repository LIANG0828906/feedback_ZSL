package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"stagepreview/stage"
	"stagepreview/web"
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
	case "verify":
		err = runVerify(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `stagepreview - deterministic stage lighting preview kernel

Usage:
  stagepreview generate [-out samples] [-seed 42]
  stagepreview verify   [-samples samples] [-runtime runtime/verify]
  stagepreview serve    [-addr 127.0.0.1:8080] [-samples samples] [-runtime runtime]
`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "sample output directory")
	seed := fs.Int64("seed", stage.DefaultSampleSeed, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := stage.GenerateSampleBundle(*seed)
	if err != nil {
		return err
	}
	if err := bundle.Write(*out); err != nil {
		return err
	}
	manifest := map[string]any{
		"ok": true, "output": *out, "seed": *seed,
		"files":   []string{"manifest.json", "v1.json", "v2.json", "v3.json", "truth.json"},
		"schemas": stage.SupportedSchemas,
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(data))
	return nil
}

type verifyOutput struct {
	OK          bool               `json:"ok"`
	Samples     string             `json:"samples"`
	Runtime     string             `json:"runtime"`
	Seed        int64              `json:"seed"`
	SchemaLoads []schemaLoad       `json:"schema_loads"`
	Truth       map[string]any     `json:"truth"`
	Preview     map[string]any     `json:"preview"`
	ThreePaths  stage.VerifyReport `json:"three_paths"`
	ElapsedMS   int64              `json:"elapsed_ms"`
}

type schemaLoad struct {
	Version      string `json:"version"`
	File         string `json:"file"`
	Fixtures     int    `json:"fixtures"`
	Traces       int    `json:"conversion_traces"`
	Defaults     int    `json:"defaults"`
	SourceSHA256 string `json:"source_sha256"`
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	samplesDir := fs.String("samples", "samples", "sample directory")
	runtimeDir := fs.String("runtime", "runtime/verify", "verification output directory")
	workers := fs.Int("workers", 4, "parallel workers for full path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	start := time.Now()
	ctx := context.Background()
	p, err := stage.LoadSampleProject(*samplesDir)
	if err != nil {
		return fmt.Errorf("load samples: %w", err)
	}
	truthData, err := os.ReadFile(filepath.Join(*samplesDir, "truth.json"))
	if err != nil {
		return err
	}
	var truth stage.TruthManifest
	if err := json.Unmarshal(truthData, &truth); err != nil {
		return err
	}
	run, err := stage.RunProject(ctx, p, stage.RunOptions{Workers: *workers, Sampling: stage.ExactSampling()})
	if err != nil {
		return err
	}
	if err := stage.CompareTruth(run, truth); err != nil {
		return fmt.Errorf("truth mismatch: %w", err)
	}
	preview, err := stage.RunPreview(ctx, p, 2, 250)
	if err != nil {
		return err
	}
	comparison, err := stage.ComparePreviewWithExact(preview.Result, run, stage.Tolerance{})
	if err != nil {
		return fmt.Errorf("preview divergence: %w", err)
	}
	verify, err := stage.VerifyThreePaths(ctx, p, *runtimeDir, stage.PathVerifyOptions{
		Sampling: stage.ExactSampling(), LiveWorkers: 1, FullWorkers: *workers, ResumeStopAfter: 2,
	})
	if err != nil {
		return fmt.Errorf("three-path verification: %w", err)
	}
	var loads []schemaLoad
	for _, item := range []struct {
		version string
		file    string
	}{
		{stage.SchemaV1, "v1.json"}, {stage.SchemaV2, "v2.json"}, {stage.SchemaV3, "v3.json"},
	} {
		data, err := os.ReadFile(filepath.Join(*samplesDir, item.file))
		if err != nil {
			return err
		}
		loaded, err := stage.LoadProject(data)
		if err != nil {
			return err
		}
		defaults := 0
		for _, tr := range loaded.Traces {
			if tr.Operation == "apply_default" {
				defaults++
			}
		}
		loads = append(loads, schemaLoad{
			Version: item.version, File: item.file, Fixtures: len(loaded.Fixtures),
			Traces: len(loaded.Traces), Defaults: defaults, SourceSHA256: loaded.SourceHash,
		})
	}
	out := verifyOutput{
		OK: true, Samples: *samplesDir, Runtime: *runtimeDir, Seed: p.Seed,
		SchemaLoads: loads,
		Truth: map[string]any{
			"windows": len(truth.Windows), "points": truth.PointCount,
			"uncertain": truth.UncertainCount, "algorithm": truth.AlgorithmVersion,
		},
		Preview: map[string]any{
			"elapsed_ms": preview.ElapsedMS, "budget_ms": preview.BudgetMS,
			"within_budget": preview.WithinBudget, "density": preview.Sampling.Density,
			"accumulation_bits": preview.Sampling.AccumulationBits,
			"compared_points":   comparison.Compared, "ok": comparison.OK,
		},
		ThreePaths: verify,
		ElapsedMS:  time.Since(start).Milliseconds(),
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	if err := os.MkdirAll(*runtimeDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*runtimeDir, "report.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "loopback listen address")
	samplesDir := fs.String("samples", "samples", "sample directory")
	runtimeDir := fs.String("runtime", "runtime", "checkpoint and edit directory")
	seed := fs.Int64("seed", stage.DefaultSampleSeed, "seed used if samples are missing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv, err := web.New(*samplesDir, *runtimeDir, *seed)
	if err != nil {
		return err
	}
	return web.Listen(*addr, srv)
}
