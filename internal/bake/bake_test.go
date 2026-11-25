package bake_test

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"terrainkernel/internal/bake"
	"terrainkernel/internal/gen"
	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
)

func TestGroundTruthIsBakeSource(t *testing.T) {
	samples := mustSamples(t)
	seen := 0
	for id, truths := range samples.GroundTruth {
		for _, truth := range truths {
			pixel, err := bake.BakePixel(samples.Project, truth.PX, truth.PY)
			if err != nil {
				t.Fatal(err)
			}
			seen++
			if pixel.HeightMM != truth.HeightMM || pixel.SlopeURad != truth.SlopeURad ||
				pixel.Dominant != truth.Dominant || pixel.Blend != truth.Blend ||
				!reflect.DeepEqual(pixel.Weights, truth.Weights) {
				t.Fatalf("chunk %d truth mismatch at (%d,%d): got %+v want %+v", id, truth.PX, truth.PY, pixel, truth)
			}
		}
	}
	if seen != samples.Project.ChunkCount()*len(samples.SamplePixels) {
		t.Fatalf("checked %d truths", seen)
	}
}

func TestNormalizeSumsExactly(t *testing.T) {
	for _, mode := range []string{"global", "per_layer"} {
		for n := 1; n <= 17; n++ {
			raw := make([]int64, n)
			for i := range raw {
				raw[i] = int64((i*37+11)%1000) * 1000
			}
			project := &model.Project{NormMode: mode, Layers: make([]model.Layer, n)}
			for i := range project.Layers {
				project.Layers[i].Curve = &model.Curve{Points: []model.CurvePoint{{X: 0, Y: 1_000_000}}}
			}
			normalized, err := bake.Normalize(mode, raw, bake.LayerAnchors(project))
			if err != nil {
				t.Fatal(err)
			}
			var sum int64
			for _, value := range normalized {
				sum += value
			}
			if sum != model.WeightScale {
				t.Fatalf("%s n=%d sum=%d", mode, n, sum)
			}
		}
	}
}

func TestWorkerAndBudgetIndependentPixels(t *testing.T) {
	samples := mustSamples(t)
	a, seqA, _, err := bake.RunPreview(samples.Project, bake.EngineConfig{MaxWorkers: 1, BufferBudgetPx: 16})
	if err != nil {
		t.Fatal(err)
	}
	b, seqB, events, err := bake.RunPreview(samples.Project, bake.EngineConfig{MaxWorkers: 4, BufferBudgetPx: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := bake.CompareResults(a, b); err != nil {
		t.Fatal(err)
	}
	if err := bake.CompareCommitSequences(seqA, seqB); err != nil {
		t.Fatal(err)
	}
	foundQueue := false
	for _, event := range events {
		if event.Reason == "pixel_buffer_budget_exhausted" {
			foundQueue = true
		}
	}
	if !foundQueue {
		t.Fatal("expected queue event for exhausted pixel buffer")
	}
}

func TestFailureIsolationAndRetry(t *testing.T) {
	samples := mustSamples(t)
	committer := bake.NewMemoryCommitter()
	engine, err := bake.NewEngine(samples.Project, bake.BuildGraph(samples.Project),
		bake.EngineConfig{MaxWorkers: 2, BufferBudgetPx: 32, StopAfter: -1}, committer)
	if err != nil {
		t.Fatal(err)
	}
	engine.InjectFailure(2, "synthetic local failure")
	report, err := engine.Run(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := report.Failures[2]; !ok {
		t.Fatal("chunk 2 was not marked failed")
	}
	if _, ok := report.Results[2]; ok {
		t.Fatal("failed chunk polluted results")
	}
	if !reflect.DeepEqual(report.CommitSequence, []int{0, 1, 3}) {
		t.Fatalf("commit sequence = %v", report.CommitSequence)
	}
	retry, err := engine.Run(context.Background(), []int{2}, report.Results)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Failures) != 0 || retry.Results[2] == nil {
		t.Fatalf("retry failed: %+v", retry)
	}
	full, _, err := bake.FullBake(samples.Project)
	if err != nil {
		t.Fatal(err)
	}
	if err := bake.CompareResults(full, retry.Results); err != nil {
		t.Fatal(err)
	}
}

func TestPauseAndResume(t *testing.T) {
	samples := mustSamples(t)
	committer := bake.NewMemoryCommitter()
	engine, err := bake.NewEngine(samples.Project, bake.BuildGraph(samples.Project),
		bake.EngineConfig{MaxWorkers: 1, BufferBudgetPx: 64, StopAfter: -1}, committer)
	if err != nil {
		t.Fatal(err)
	}
	engine.Pause()
	type outcome struct {
		report *bake.RunReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := engine.Run(context.Background(), nil, nil)
		done <- outcome{report: report, err: err}
	}()
	select {
	case <-done:
		t.Fatal("run completed while paused")
	case <-time.After(25 * time.Millisecond):
	}
	engine.Resume()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		full, _, err := bake.FullBake(samples.Project)
		if err != nil {
			t.Fatal(err)
		}
		if err := bake.CompareResults(full, result.report.Results); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not resume")
	}
}

func TestExactInvalidationAndIncrementalEquality(t *testing.T) {
	samples := mustSamples(t)
	oldProject := samples.Project
	edited := oldProject.Clone()
	edited.Layers[2].Curve.Points[1].Y += 33_000
	before := cloneLayer(oldProject.Layers[2])
	after := cloneLayer(edited.Layers[2])
	graph := bake.BuildGraph(oldProject)
	invalidations := graph.Propagate([]bake.Edit{{
		LayerID: "rock",
		Reason:  "rock curve control point changed",
		Before:  before,
		After:   after,
	}})
	affected := bake.InvalidationChunks(invalidations)
	expected := bruteAffectedChunks(oldProject, edited, 2)
	if !reflect.DeepEqual(affected, expected) {
		t.Fatalf("affected chunks = %v, want %v", affected, expected)
	}
	if len(invalidations) != len(expected) {
		t.Fatalf("invalidations = %d, want %d", len(invalidations), len(expected))
	}
	for _, inv := range invalidations {
		if inv.LayerID != "rock" || !strings.Contains(inv.Reason, "composite") {
			t.Fatalf("bad invalidation: %+v", inv)
		}
	}

	cache, _, err := bake.FullBake(oldProject)
	if err != nil {
		t.Fatal(err)
	}
	incremental, sequence, err := bake.IncrementalBake(edited, cache, invalidations,
		bake.EngineConfig{MaxWorkers: 3, BufferBudgetPx: 24, StopAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	full, fullSeq, err := bake.FullBake(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := bake.CompareResults(full, incremental); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sequence, affected) {
		t.Fatalf("incremental commit order = %v, want %v", sequence, affected)
	}
	if !reflect.DeepEqual(fullSeq, edited.ChunkIDs()) {
		t.Fatalf("full commit order = %v", fullSeq)
	}
}

func TestChunkParamVersionTracksActualFactors(t *testing.T) {
	samples := mustSamples(t)
	oldProject := samples.Project
	graph := bake.BuildGraph(oldProject)
	deps := graph.ChunkDependencies(0)
	for _, layer := range oldProject.Layers {
		found := false
		for _, dep := range deps {
			if dep == "layer:"+layer.ID+":curve" {
				found = true
			}
		}
		if !found {
			t.Fatalf("dependency set lacks layer %s: %v", layer.ID, deps)
		}
	}
	foundNorm := false
	for _, dep := range deps {
		if dep == "normalization:"+oldProject.NormMode {
			foundNorm = true
		}
	}
	if !foundNorm {
		t.Fatalf("dependency set lacks normalization: %v", deps)
	}

	var edited *model.Project
	var affected []int
	for cap := int64(1_450_000); cap <= 1_590_000; cap += 5_000 {
		candidate := oldProject.Clone()
		before := cloneLayer(oldProject.Layers[3])
		candidate.Layers[3].SlopeCapURad = cap
		after := cloneLayer(candidate.Layers[3])
		inv := graph.Propagate([]bake.Edit{{LayerID: "snow", Reason: "slope cap edited", Before: before, After: after}})
		ids := bake.InvalidationChunks(inv)
		if len(ids) > 0 && len(ids) < oldProject.ChunkCount() {
			edited = candidate
			affected = ids
			break
		}
	}
	if edited == nil {
		t.Fatal("could not find a partially affecting slope cap edit")
	}
	affectedSet := make(map[int]struct{}, len(affected))
	for _, id := range affected {
		affectedSet[id] = struct{}{}
	}
	editedGraph := bake.BuildGraph(edited)
	for id := 0; id < oldProject.ChunkCount(); id++ {
		oldVersion := bake.ChunkParamVersion(oldProject, id, graph.ChunkDependencies(id))
		newVersion := bake.ChunkParamVersion(edited, id, editedGraph.ChunkDependencies(id))
		_, affected := affectedSet[id]
		if affected && oldVersion == newVersion {
			t.Fatalf("affected chunk %d kept its param version", id)
		}
		if !affected && oldVersion != newVersion {
			t.Fatalf("unaffected chunk %d changed its param version", id)
		}
	}
}

func TestCompareLocatesFirstDiff(t *testing.T) {
	samples := mustSamples(t)
	a, _, err := bake.FullBake(samples.Project)
	if err != nil {
		t.Fatal(err)
	}
	b := cloneResults(a)
	b[1].WeightRows[2*8+1][1]++
	err = bake.CompareResults(a, b)
	if err == nil {
		t.Fatal("expected diff")
	}
	diff, ok := err.(*bake.DiffPoint)
	if !ok {
		t.Fatalf("unexpected error type %T", err)
	}
	if diff.ChunkID != 1 || diff.PX != 1 || diff.PY != 2 || diff.Field != "weights" {
		t.Fatalf("diff location = %+v", diff)
	}
	if !strings.Contains(err.Error(), kerr.CodeDiff) {
		t.Fatalf("error text lacks E_DIFF: %v", err)
	}
	b = cloneResults(a)
	b[3].Blends[10]++
	err = bake.CompareResults(a, b)
	diff, ok = err.(*bake.DiffPoint)
	if !ok || diff.ChunkID != 3 || diff.PX != 2 || diff.PY != 1 || diff.Field != "blend" {
		t.Fatalf("blend diff location = %+v", err)
	}
	if err := bake.CompareCommitSequences([]int{0, 1}, []int{0, 2}); err == nil {
		t.Fatal("expected commit sequence diff")
	}
}

func mustSamples(t *testing.T) *gen.SampleSet {
	t.Helper()
	samples, err := gen.Generate(gen.Options{Seed: 20261125, ChunkCols: 2, ChunkRows: 2, ChunkSizePx: 8})
	if err != nil {
		t.Fatal(err)
	}
	return samples
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

func bruteAffectedChunks(oldProject, newProject *model.Project, layerIndex int) []int {
	var out []int
	for id := 0; id < oldProject.ChunkCount(); id++ {
		ox, oy, _ := oldProject.ChunkOrigin(id)
		affected := false
		for y := 0; y < oldProject.ChunkSizePx && !affected; y++ {
			for x := 0; x < oldProject.ChunkSizePx; x++ {
				_, _, oldRaw, err := bake.EvalRawWeights(oldProject, ox+x, oy+y)
				if err != nil {
					panic(err)
				}
				_, _, newRaw, err := bake.EvalRawWeights(newProject, ox+x, oy+y)
				if err != nil {
					panic(err)
				}
				if oldRaw[layerIndex] != newRaw[layerIndex] {
					affected = true
					break
				}
			}
		}
		if affected {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func cloneResults(in map[int]*bake.ChunkResult) map[int]*bake.ChunkResult {
	out := make(map[int]*bake.ChunkResult, len(in))
	for id, result := range in {
		c := *result
		c.Blends = append([]int64(nil), result.Blends...)
		c.Dominants = append([]int(nil), result.Dominants...)
		c.WeightRows = make([][]int64, len(result.WeightRows))
		for i := range result.WeightRows {
			c.WeightRows[i] = append([]int64(nil), result.WeightRows[i]...)
		}
		out[id] = &c
	}
	return out
}
