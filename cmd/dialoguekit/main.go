// Command dialoguekit generates fixed-seed samples and runs the local
// acceptance demonstration. It uses only the standard library.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"dialogueaudio/diagnostics"
	"dialogueaudio/engine"
	"dialogueaudio/generator"
	"dialogueaudio/pipeline"
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
	case "demo":
		err = runDemo(os.Args[2:])
	case "version":
		fmt.Println("dialoguekit 1.0.0 (Go standard library only)")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  dialoguekit generate --seed 20261127 --out var/samples
  dialoguekit demo     --seed 20261127 --out var/demo

demo runs schema loading, fixed-point round trips, truth comparison, the
three-path checkpoint check, precise invalidation, bounded concurrency, and
stable error reproduction entirely locally.`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic seed")
	out := fs.String("out", "var/samples", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := generator.Build(*seed)
	if err != nil {
		return err
	}
	if err := set.Write(*out); err != nil {
		return err
	}
	if err := generator.ValidateManifest(*out); err != nil {
		return err
	}
	summary := map[string]any{
		"seed": *seed, "out": *out, "clips": len(set.Project.Clips),
		"frames": set.Manifest.FrameCount, "manifest_valid": true,
		"files": set.Manifest.Files,
	}
	return printJSON(summary)
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic seed")
	out := fs.String("out", "var/demo", "diagnostic output directory")
	maxParallel := fs.Int("max-parallel", 2, "bounded worker count")
	queueBudget := fs.Int("queue-budget", 1, "pending clip queue budget")
	sampleBuffer := fs.Int("sample-buffer", 960, "per-worker PCM sample buffer budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := pipeline.Options{
		Batch: engine.BatchOptions{
			MaxParallel: *maxParallel, QueueBudget: *queueBudget,
			SampleBufferSamples: *sampleBuffer,
		},
	}
	report, err := diagnostics.Run(*seed, *out, opts)
	if err != nil {
		return err
	}
	if err := diagnostics.ValidateReport(report); err != nil {
		return fmt.Errorf("acceptance validation failed: %w", err)
	}
	abs, _ := filepath.Abs(filepath.Join(*out, "report.json"))
	fmt.Fprintln(os.Stderr, "acceptance validated:", abs)
	return printJSON(report)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
