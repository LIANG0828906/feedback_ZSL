package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"inputkernel/internal/app"
	"inputkernel/internal/model"
	"inputkernel/internal/samplegen"
	"inputkernel/internal/sampleio"
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
	case "validate":
		err = runValidate(os.Args[2:])
	case "run":
		err = runExecute(os.Args[2:])
	case "version":
		fmt.Println("inputkernel 1.0.0 (Go standard library only)")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		var kernelErr *model.Error
		if errors.As(err, &kernelErr) {
			data, _ := json.MarshalIndent(kernelErr, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", samplegen.DefaultSeed, "deterministic sample seed")
	out := fs.String("out", "samples", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := samplegen.Write(*out, *seed)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Printf("generated %d scenarios in %s with seed %d\n%s\n", len(manifest.Scenarios), *out, *seed, data)
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	samples := fs.String("samples", "samples", "sample directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := sampleio.Load(*samples)
	if err != nil {
		return err
	}
	fmt.Printf("validated %d scenarios with seed %d\n", len(set.Scenarios), set.Manifest.Seed)
	return nil
}

func runExecute(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	samples := fs.String("samples", "samples", "sample directory")
	store := fs.String("store", "snapshots", "snapshot directory")
	out := fs.String("out", "artifacts/report.json", "report JSON path")
	workers := fs.Int("workers", 4, "maximum worker concurrency")
	buffer := fs.Int("buffer", 8, "per-worker queued work buffer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := app.Execute(context.Background(), app.Options{
		SamplesDir: *samples, StoreDir: *store, ReportPath: *out,
		Workers: *workers, Buffer: *buffer,
	})
	if report != nil {
		for _, scenario := range report.Scenarios {
			fmt.Printf("%-14s truth=%-5t actions=%-3d axis=%-3d incr_equal=%-5t snapshot=%s\n",
				scenario.ID, scenario.Truth.Pass, scenario.FullReplay.Actions,
				scenario.FullReplay.AxisSamples, scenario.Incremental.Comparison.Equal, scenario.Schema)
		}
		fmt.Printf("status=%s report=%s\n", report.Status, *out)
	}
	return err
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: inputkernel <generate|validate|run|version> [flags]")
}
