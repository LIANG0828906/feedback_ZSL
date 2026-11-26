package main

import (
	"os"
	"path/filepath"

	"serviceregistry/anomaly"
	"serviceregistry/checkpoint"
	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

func commandAnomaly(args []string) int {
	flags := newFlagSet("anomaly")
	seed := flags.Int64("seed", samplegen.DefaultSeed, "sample seed")
	checkpointPath := flags.String("checkpoint", "", "checkpoint file (temporary if omitted)")
	if err := parseFlags(flags, args); err != nil {
		return fail(err, exitUsage)
	}
	bundle, err := samplegen.BuildBundle(*seed)
	if err != nil {
		return fail(err, exitBusiness)
	}
	document, err := registry.LoadDocument(bundle.Files[registry.SchemaV1])
	if err != nil {
		return fail(err, exitBusiness)
	}
	path := *checkpointPath
	cleanup := func() {}
	if path == "" {
		dir, err := os.MkdirTemp("", "registry-anomaly-*")
		if err != nil {
			return fail(err, exitBusiness)
		}
		path = filepath.Join(dir, "checkpoint.db")
		cleanup = func() { _ = os.RemoveAll(dir) }
	}
	defer cleanup()
	run, snapshots, err := registry.ComputeProject(document.Project, nil, true)
	if err != nil {
		return fail(err, exitBusiness)
	}
	commit := 7
	if len(snapshots) < commit+1 {
		commit = len(snapshots) - 1
	}
	if err := (&checkpoint.Store{Path: path}).Save(&checkpoint.Checkpoint{
		Snapshot: snapshots[commit],
		Steps:    append([]registry.StepResult(nil), run.Steps[:commit+1]...),
	}); err != nil {
		return fail(err, exitBusiness)
	}
	results, err := anomaly.RunSuite(anomaly.Input{
		Project:        document.Project,
		Documents:      bundle.Files,
		CheckpointPath: path,
	})
	if err != nil {
		return fail(err, exitBusiness)
	}
	allPassed := true
	for _, result := range results {
		if !result.Passed {
			allPassed = false
		}
	}
	report := map[string]any{
		"ok":         allPassed,
		"command":    "anomaly",
		"checkpoint": path,
		"cases":      results,
	}
	if !allPassed {
		report["code"] = registry.CodeVerificationFailed
	}
	if err := jsonEncodeStdout(report); err != nil {
		return fail(err, exitInternal)
	}
	if !allPassed {
		return exitBusiness
	}
	return exitOK
}
