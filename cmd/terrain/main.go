package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"terrainkernel/internal/bake"
	"terrainkernel/internal/gen"
	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
	"terrainkernel/internal/store"
)

func main() {
	workdir := flag.String("workdir", "work", "writable directory for samples and SQLite checkpoint")
	workers := flag.Int("workers", 2, "max parallel baking workers")
	budget := flag.Int("buffer-budget-px", 96, "global in-flight pixel buffer budget")
	stopAfter := flag.Int("stop-after", -1, "pause checkpoint after committing this many chunks (-1 = no stop)")
	seed := flag.Int64("seed", 0, "generator seed (0 = current UTC date as YYYYMMDD)")
	flag.Parse()

	if *seed == 0 {
		var y, m, d int
		fmt.Sscanf(time.Now().UTC().Format("20060102"), "%4d%2d%2d", &y, &m, &d)
		*seed = int64(y*10000 + m*100 + d)
	}
	if err := run(*workdir, *workers, *budget, *stopAfter, *seed); err != nil {
		var loss *kerr.LossError
		if ok := asLoss(err, &loss); ok {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func asLoss(err error, target **kerr.LossError) bool {
	le, ok := kerr.AsLossError(err)
	if ok {
		*target = le
	}
	return ok
}

func run(workdir string, workers, budget, stopAfter int, seed int64) error {
	if workers <= 0 || budget <= 0 {
		return fmt.Errorf("workers and buffer budget must be positive")
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	samples, err := gen.Generate(gen.Options{Seed: seed, ChunkCols: 2, ChunkRows: 2, ChunkSizePx: 8})
	if err != nil {
		return err
	}
	if err := writeSamples(workdir, samples); err != nil {
		return err
	}
	fmt.Printf("[gen] project=%s chunks=%d chunkSize=%d layers=%d norm=%s\n",
		samples.Project.ProjectID, samples.Project.ChunkCount(), samples.Project.ChunkSizePx,
		len(samples.Project.Layers), samples.Project.NormMode)

	if err := verifyGroundTruth(samples); err != nil {
		return err
	}

	cfg := bake.EngineConfig{
		MaxWorkers:     workers,
		BufferBudgetPx: budget,
		Precise:        true,
		StopAfter:      stopAfter,
	}
	resumeCfg := cfg
	resumeCfg.StopAfter = -1
	graph := bake.BuildGraph(samples.Project)
	checkpoint := filepath.Join(workdir, "checkpoint.db")
	fp := store.BuildFingerprint(samples.Project)
	db, err := store.Open(checkpoint, fp)
	if err != nil {
		return err
	}
	engine, err := bake.NewEngine(samples.Project, graph, cfg, store.NewCommitter(db))
	if err != nil {
		db.Close()
		return err
	}
	report1, err := engine.Run(context.Background(), nil, nil)
	if err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	fmt.Printf("[run1] committed chunks: %v\n", report1.CommitSequence)

	db, err = store.Open(checkpoint, fp)
	if err != nil {
		return err
	}
	loaded, _, resetReason, err := db.LoadCommitted(samples.Project.ChunkCount(),
		expectedDependencies(graph), bake.ChunkParamVersions(samples.Project, graph))
	if err != nil {
		db.Close()
		return err
	}
	_ = resetReason
	engine, err = bake.NewEngine(samples.Project, graph, resumeCfg, store.NewCommitter(db))
	if err != nil {
		db.Close()
		return err
	}
	missing := chunksMissing(samples.Project, loaded)
	report2, err := engine.Run(context.Background(), missing, loaded)
	if err != nil {
		db.Close()
		return err
	}
	run2Seq, err := db.CommitSequence()
	if err != nil {
		db.Close()
		return err
	}
	fmt.Printf("[run2] resumed chunks: %v reused: %v\n", run2Seq, report2.Reused)
	fullResults := report2.Results
	if report1.Paused {
		if err := bake.CompareResults(fullResults, report2.Results); err != nil {
			db.Close()
			return err
		}
	}
	preview, previewSeq, _, err := bake.RunPreview(samples.Project, resumeCfg)
	if err != nil {
		db.Close()
		return err
	}
	if err := bake.CompareResults(preview, fullResults); err != nil {
		db.Close()
		return err
	}
	if err := bake.CompareCommitSequences(previewSeq, run2Seq); err != nil {
		db.Close()
		return err
	}
	fmt.Println("[verify] preview == full bake, pixel-exact")

	edited := samples.Project.Clone()
	if len(edited.Layers) < 2 {
		db.Close()
		return fmt.Errorf("sample project has too few layers")
	}
	before := cloneLayer(edited.Layers[1])
	edited.Layers[1].Curve.Points[1].Y += 40_000
	after := cloneLayer(edited.Layers[1])
	edit := bake.Edit{
		LayerID: "grass",
		Reason:  "curve control point[1] edited",
		Before:  before,
		After:   after,
	}
	invalidations := graph.Propagate([]bake.Edit{edit})
	fmt.Printf("[incremental] invalidated entries: %d\n", len(invalidations))
	affected := bake.InvalidationChunks(invalidations)
	if err := db.InvalidateChunks(affected); err != nil {
		db.Close()
		return err
	}
	editedGraph := bake.BuildGraph(edited)
	editedFP := store.BuildFingerprint(edited)
	if editedFP != fp {
		db.Close()
		return fmt.Errorf("edit unexpectedly changed the global checkpoint fingerprint")
	}
	editedCache, _, _, err := db.LoadCommitted(edited.ChunkCount(),
		expectedDependencies(editedGraph), bake.ChunkParamVersions(edited, editedGraph))
	if err != nil {
		db.Close()
		return err
	}
	engine, err = bake.NewEngine(edited, editedGraph, resumeCfg, store.NewCommitter(db))
	if err != nil {
		db.Close()
		return err
	}
	incrementalReport, err := engine.Run(context.Background(), affected, editedCache)
	if err != nil {
		db.Close()
		return err
	}
	editedFull, _, err := bake.FullBake(edited)
	if err != nil {
		db.Close()
		return err
	}
	if err := bake.CompareResults(editedFull, incrementalReport.Results); err != nil {
		db.Close()
		return err
	}
	if err := bake.CompareCommitSequences(affected, incrementalReport.CommitSequence); err != nil {
		db.Close()
		return err
	}
	db.Close()
	fmt.Println("[verify] incremental rebake == full rebake on edited project, pixel-exact")
	fmt.Printf("[done] samples, checkpoint and ground truth written under %s\n", workdir)
	return nil
}

func writeSamples(workdir string, samples *gen.SampleSet) error {
	files := map[string][]byte{
		"project_v1.json": samples.V1,
		"project_v2.json": samples.V2,
		"project_v3.json": samples.V3,
	}
	traces, err := json.MarshalIndent(samples.Traces, "", "  ")
	if err != nil {
		return err
	}
	files["upgrade_traces.json"] = traces
	truth, err := gen.MarshalGroundTruth(samples.GroundTruth)
	if err != nil {
		return err
	}
	files["ground_truth.json"] = truth
	losses, err := json.MarshalIndent(samples.Losses, "", "  ")
	if err != nil {
		return err
	}
	files["loss_report.json"] = append(losses, '\n')
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(workdir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func verifyGroundTruth(samples *gen.SampleSet) error {
	for id, truths := range samples.GroundTruth {
		for _, truth := range truths {
			pixel, err := bake.BakePixel(samples.Project, truth.PX, truth.PY)
			if err != nil {
				return err
			}
			if pixel.HeightMM != truth.HeightMM || pixel.SlopeURad != truth.SlopeURad ||
				pixel.Dominant != truth.Dominant || pixel.Blend != truth.Blend ||
				!equalInt64s(pixel.Weights, truth.Weights) {
				return fmt.Errorf("ground truth mismatch in chunk %d at (%d,%d)", id, truth.PX, truth.PY)
			}
		}
	}
	return nil
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func expectedDependencies(graph *bake.Graph) map[int][]string {
	out := make(map[int][]string)
	for id := 0; id < graph.ChunkCount(); id++ {
		out[id] = graph.ChunkDependencies(id)
	}
	return out
}

func chunksMissing(p *model.Project, loaded map[int]*bake.ChunkResult) []int {
	var out []int
	for id := 0; id < p.ChunkCount(); id++ {
		if _, ok := loaded[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func cloneLayer(layer model.Layer) *model.Layer {
	out := layer
	if layer.Curve != nil {
		curve := *layer.Curve
		curve.Points = append([]model.CurvePoint(nil), layer.Curve.Points...)
		out.Curve = &curve
	}
	if layer.Noise != nil {
		noise := *layer.Noise
		out.Noise = &noise
	}
	return &out
}
