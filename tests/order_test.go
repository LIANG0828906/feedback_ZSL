package tests

import (
	"context"
	"testing"

	"fixedsampler/internal/engine"
	"fixedsampler/internal/loader"
)

func TestSwapOrderIncrementalMatchesFull(t *testing.T) {
	dir := generate(t)
	original := loadProject(t, dir, "mixed422")
	store := openStore(t)
	runStore(t, original, store, 8, 2, 2)
	prev, err := store.LoadPrevious(context.Background(), original.ProjectID)
	if err != nil {
		t.Fatal(err)
	}

	swapped := loadProject(t, dir, "mixed422")
	swapped.Config[0], swapped.Config[1] = swapped.Config[1], swapped.Config[0]
	for i := range swapped.Config {
		swapped.Config[i].Seq = i + 1
	}
	if err := loader.ValidateProject(swapped); err != nil {
		t.Fatalf("swapped order should remain dimensionally valid: %v", err)
	}

	inc := runMemory(t, swapped, 8, 3, 1, prev)
	full := runMemory(t, swapped, 8, 1, 1, nil)
	if loc := engine.CompareChunkMaps(inc.Artifacts, full.Artifacts, inc.CommitSequence, full.CommitSequence); loc != nil {
		t.Fatalf("swapped-order incremental/full mismatch: %s", loc.Error())
	}

	affected := map[int]int{}
	for _, inv := range inc.Invalidations {
		affected[inv.StageSeq]++
	}
	chunks := swapped.FullWidth / 8 * swapped.FullHeight / 8
	for seq := 1; seq <= len(swapped.Config); seq++ {
		if affected[seq] != chunks {
			t.Fatalf("stage %d invalidated chunks=%d want %d; all swapped-chain nodes should recompute", seq, affected[seq], chunks)
		}
	}
}

func TestAlignedChunkDependenciesDoNotFanOut(t *testing.T) {
	dir := generate(t)
	p := loadProject(t, dir, "blocks420")
	res := runMemory(t, p, 8, 2, 2, nil)
	if len(res.DependencyEdges) == 0 {
		t.Fatal("expected explicit dependency edges")
	}
	for _, edge := range res.DependencyEdges {
		if edge.FromChunk != edge.ToChunk {
			t.Fatalf("aligned chunk dependency fanned out: %+v", edge)
		}
		if edge.Plane == "" || edge.Reason == "" || edge.CacheKey == "" {
			t.Fatalf("dependency edge missing diagnostic fields: %+v", edge)
		}
	}
}
