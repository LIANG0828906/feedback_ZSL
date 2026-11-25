package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"soundlab/audiofp"
)

func runInject(args []string) int {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	samples := fs.String("samples", "samples", "sample directory")
	artifacts := fs.String("artifacts", "artifacts/faults", "fault output directory")
	workers := fs.Int("workers", 4, "extraction workers")
	buffer := fs.Int("buffer-samples", 8192, "sampling buffer budget")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	manifests, err := filepath.Glob(filepath.Join(*samples, "batch-*", "project.json"))
	if err != nil || len(manifests) == 0 {
		return fail(fmt.Errorf("no generated batches under %s", *samples))
	}
	sort.Strings(manifests)
	if err := os.RemoveAll(*artifacts); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*artifacts, 0o755); err != nil {
		return fail(err)
	}
	faults, err := runFaults(manifests[0], *artifacts, *workers, *buffer)
	if err != nil {
		return fail(err)
	}
	report := map[string]any{"ok": faultsOK(faults), "faults": faults}
	path := filepath.Join(*artifacts, "faults.json")
	if err := writeReport(path, report); err != nil {
		return fail(err)
	}
	printJSON(report)
	if !faultsOK(faults) {
		return 1
	}
	return 0
}

func faultsOK(faults []FaultReport) bool {
	for _, f := range faults {
		if !f.OK {
			return false
		}
	}
	return len(faults) == 4
}

func runFaults(manifest, artifacts string, workers, bufferSamples int) ([]FaultReport, error) {
	project, err := audiofp.LoadProject(manifest)
	if err != nil {
		return nil, err
	}
	finalAssets, recipe, _, err := project.ApplyEdits()
	if err != nil {
		return nil, err
	}
	var reports []FaultReport

	truncReport, err := injectTruncate(project)
	if err != nil {
		return nil, err
	}
	reports = append(reports, truncReport)

	recipeReport, err := injectRecipe(project)
	if err != nil {
		return nil, err
	}
	reports = append(reports, recipeReport)

	orderReport, err := injectOrder(finalAssets, recipe, workers, bufferSamples)
	if err != nil {
		return nil, err
	}
	reports = append(reports, orderReport)

	stageReport, err := injectStage(artifacts, finalAssets, recipe, workers, bufferSamples)
	if err != nil {
		return nil, err
	}
	reports = append(reports, stageReport)
	return reports, nil
}

func injectTruncate(project *audiofp.Project) (FaultReport, error) {
	manifest := project.Manifest
	manifest.BatchID = "fault-truncated"
	manifest.Assets = append([]audiofp.AssetSpec(nil), manifest.Assets...)
	manifest.Assets[0].TotalSamples++
	path, err := writeTempManifest(project.Dir, "truncated", manifest)
	if err != nil {
		return FaultReport{}, err
	}
	defer os.Remove(path)
	_, loadErr := audiofp.LoadProject(path)
	code := audiofp.CodeOf(loadErr)
	report := FaultReport{Kind: "truncate", ExpectedCode: audiofp.CodeDecode, ActualCode: code, OK: code == audiofp.CodeDecode}
	if typed, ok := audiofp.AsError(loadErr); ok {
		report.AssetID = typed.AssetID
	}
	return report, nil
}

func injectRecipe(project *audiofp.Project) (FaultReport, error) {
	manifest := project.Manifest
	manifest.BatchID = "fault-recipe"
	manifest.Recipe.FrameShift = manifest.Recipe.FrameLen + 1
	path, err := writeTempManifest(project.Dir, "recipe", manifest)
	if err != nil {
		return FaultReport{}, err
	}
	defer os.Remove(path)
	_, loadErr := audiofp.LoadProject(path)
	code := audiofp.CodeOf(loadErr)
	field := ""
	if typed, ok := audiofp.AsError(loadErr); ok {
		field = typed.Field
	}
	return FaultReport{
		Kind: "recipe_contradiction", ExpectedCode: audiofp.CodeParameter, ActualCode: code,
		OK: code == audiofp.CodeParameter && field == "frame_shift",
	}, nil
}

func injectOrder(assets []audiofp.PCMAsset, recipe audiofp.Recipe, workers, bufferSamples int) (FaultReport, error) {
	asset := assets[0]
	frame := 2
	_, _, err := audiofp.ExtractAsset(context.Background(), asset, recipe, audiofp.ExtractionOptions{
		Workers: workers, MaxSamplingBufferSamples: bufferSamples,
		Inject: &audiofp.Injection{Kind: "frame_order", AssetID: asset.ID, Shard: 1, Frame: frame},
	})
	code := audiofp.CodeOf(err)
	report := FaultReport{Kind: "frame_order", ExpectedCode: audiofp.CodeOrder, ActualCode: code,
		AssetID: asset.ID, Shard: 1, Frame: frame, OK: code == audiofp.CodeOrder}
	return report, nil
}

func injectStage(artifacts string, assets []audiofp.PCMAsset, recipe audiofp.Recipe, workers, bufferSamples int) (FaultReport, error) {
	path := filepath.Join(artifacts, "stage.db")
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	store, err := audiofp.OpenStore(path)
	if err != nil {
		return FaultReport{}, err
	}
	projectID := "fault-stage"
	opts := audiofp.BuildOptions{
		ShardFrames: 24,
		Extraction: audiofp.ExtractionOptions{
			Workers: workers, MaxSamplingBufferSamples: bufferSamples,
			Inject: &audiofp.Injection{Kind: "stage", AssetID: assets[0].ID, Shard: 0, FailOnce: true},
		},
	}
	failed, firstErr := audiofp.BuildCheckpoint(context.Background(), store, projectID, assets, recipe,
		audiofp.SnapshotFresh, opts)
	if audiofp.CodeOf(firstErr) != audiofp.CodeStage {
		_ = store.Close()
		return FaultReport{}, fmt.Errorf("expected E_STAGE, got %v", firstErr)
	}
	committedBefore := len(failed.Commits)
	_ = store.Close()

	store, err = audiofp.OpenStore(path)
	if err != nil {
		return FaultReport{}, err
	}
	defer store.Close()
	opts.Extraction.Inject = nil
	recovered, err := audiofp.BuildCheckpoint(context.Background(), store, projectID, assets, recipe,
		audiofp.SnapshotResume, opts)
	if err != nil {
		return FaultReport{}, err
	}
	fullOpts := opts
	fullOpts.Extraction.Inject = nil
	fullStore, err := audiofp.OpenStore(filepath.Join(artifacts, "stage-full.db"))
	if err != nil {
		return FaultReport{}, err
	}
	defer fullStore.Close()
	full, err := audiofp.BuildCheckpoint(context.Background(), fullStore, "fault-stage-full", assets, recipe,
		audiofp.SnapshotFresh, fullOpts)
	if err != nil {
		return FaultReport{}, err
	}
	// Project IDs differ only in snapshot metadata; analyses and shard order
	// must still be identical.
	if err := audiofp.CompareAnalyses(full.Analyses, recovered.Analyses); err != nil {
		return FaultReport{}, err
	}
	return FaultReport{
		Kind: "stage_failure_retry", ExpectedCode: audiofp.CodeStage, ActualCode: audiofp.CodeStage,
		AssetID: assets[0].ID, Shard: 0, CommittedBeforeRetry: committedBefore,
		ReusedAfterRetry: recovered.ReusedShards,
		OK:               committedBefore > 0 && recovered.ReusedShards > 0,
	}, nil
}

func writeTempManifest(dir, suffix string, manifest audiofp.Manifest) (string, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".fault-"+suffix+"-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
