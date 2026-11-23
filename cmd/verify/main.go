// verify runs the acceptance flow over generated sample projects:
// realtime processing with checkpoints, checkpoint-resume processing
// and one-shot full recomputation must agree on every caption window,
// every character highlight onset and every error-report statistic.
//
// Usage: verify [-samples DIR] [-work DIR]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"captionx/internal/checkpoint"
	"captionx/internal/errs"
	"captionx/internal/loader"
	"captionx/internal/model"
	"captionx/internal/pipeline"
	"captionx/internal/samplegen"
)

func main() {
	samplesDir := flag.String("samples", "samples", "samples directory (generated if missing)")
	workDir := flag.String("work", "verify-work", "checkpoint working directory")
	flag.Parse()

	if _, err := os.Stat(filepath.Join(*samplesDir, "project-1")); os.IsNotExist(err) {
		if _, err := samplegen.Generate(samplegen.Options{OutDir: *samplesDir, Count: 3, Seed: 20261124}); err != nil {
			fail("generate samples", err)
		}
	}
	projects, err := filepath.Glob(filepath.Join(*samplesDir, "project-*"))
	if err != nil || len(projects) == 0 {
		fail("list projects", fmt.Errorf("no projects under %s", *samplesDir))
	}
	os.RemoveAll(*workDir)
	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		fail("mkdir work", err)
	}

	failures := 0
	for _, dir := range projects {
		name := filepath.Base(dir)
		p, err := loader.Load(dir)
		if err != nil {
			fmt.Printf("[%s] LOAD FAIL: %v\n", name, err)
			failures++
			continue
		}
		res, err := verifyProject(p, filepath.Join(*workDir, name))
		if err != nil {
			fmt.Printf("[%s] FAIL: %v\n", name, err)
			failures++
			continue
		}
		fmt.Printf("[%s] OK: three paths consistent, %d captions, total duration deviation=%d bound=%d\n",
			name, len(res.Captions), res.Report.TotalDurationDeviation, res.Report.TotalDurationBound)
	}
	if failures > 0 {
		fmt.Printf("verify: %d project(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Printf("verify: all %d projects consistent across realtime / resume / full-recompute\n", len(projects))
}

func verifyProject(p *loader.Project, workDir string) (*model.Result, error) {
	// Path 1: realtime chunked processing with checkpoints, interrupted
	// after the first commit to prove checkpointing is real.
	store, err := checkpoint.NewStore(workDir)
	if err != nil {
		return nil, err
	}
	part, err := pipeline.RunChunked(p, pipeline.Options{Store: store, StopAfterCommits: 1})
	if err != nil {
		return nil, fmt.Errorf("realtime interrupted run: %w", err)
	}
	if part.Complete {
		return nil, fmt.Errorf("expected interrupted run to be incomplete")
	}
	rt, err := pipeline.RunChunked(p, pipeline.Options{Store: store, Resume: true})
	if err != nil {
		return nil, fmt.Errorf("realtime resumed run: %w", err)
	}
	if !rt.Complete {
		return nil, fmt.Errorf("resumed run did not complete")
	}

	// Path 2: resume again from the persisted checkpoint (all chunks
	// committed by now, so this exercises pure checkpoint replay).
	res, err := pipeline.RunChunked(p, pipeline.Options{Store: store, Resume: true})
	if err != nil {
		return nil, fmt.Errorf("resume run: %w", err)
	}

	// Path 3: one-shot full recomputation.
	full, err := pipeline.RunFull(p, nil)
	if err != nil {
		return nil, fmt.Errorf("full recompute: %w", err)
	}

	if err := pipeline.Compare(rt.Result, res.Result); err != nil {
		return nil, fmt.Errorf("realtime vs resume: %w", err)
	}
	if err := pipeline.Compare(rt.Result, full); err != nil {
		return nil, fmt.Errorf("realtime vs full: %w", err)
	}
	return rt.Result, nil
}

func fail(what string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v (code=%s)\n", what, err, errs.Code(err))
	os.Exit(1)
}
