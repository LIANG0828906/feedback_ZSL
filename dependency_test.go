package iconmask

import "testing"

func TestCacheKeyDependencies(t *testing.T) {
	base := CacheKeyInput{
		BlockContentHash: "content",
		SegmentID:        "s0",
		Radius:           2,
		Round:            0,
		UpstreamKey:      "upstream",
	}
	key := BuildCacheKey(base)
	if key == "" {
		t.Fatal("cache key must not be empty")
	}
	mutations := []CacheKeyInput{
		{BlockContentHash: "content2", SegmentID: "s0", Radius: 2, Round: 0, UpstreamKey: "upstream"},
		{BlockContentHash: "content", SegmentID: "s1", Radius: 2, Round: 0, UpstreamKey: "upstream"},
		{BlockContentHash: "content", SegmentID: "s0", Stage: "close", Radius: 2, Round: 0, UpstreamKey: "upstream"},
		{BlockContentHash: "content", SegmentID: "s0", Radius: 3, Round: 0, UpstreamKey: "upstream"},
		{BlockContentHash: "content", SegmentID: "s0", Radius: 2, Round: 1, UpstreamKey: "upstream"},
		{BlockContentHash: "content", SegmentID: "s0", Radius: 2, Round: 0, UpstreamKey: "upstream2"},
	}
	for _, mutation := range mutations {
		if BuildCacheKey(mutation) == key {
			t.Fatalf("cache key did not change for mutation %+v", mutation)
		}
	}
}

func TestPreciseParameterAndMaskInvalidation(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-128")
	cfg := DefaultRepairConfig()
	table, err := ResolveRadii(image.ID, image.Truth.Segments, cfg.Radius)
	if err != nil {
		t.Fatal(err)
	}
	graph := NewDependencyGraph(image.ID, image.InitialMask, image.Truth.Segments, cfg.BlockSize, cfg.Radius.Max, cfg.MaxRounds, table)
	if len(graph.Blocks) == 0 || len(graph.Segments) == 0 {
		t.Fatal("expected populated dependency graph")
	}
	for _, block := range graph.Blocks {
		if !graph.IsValid(block.Block) {
			t.Fatal("fresh graph must have all blocks valid")
		}
	}

	noChange, err := graph.InvalidateParams(image.ID, cfg.Radius, cfg.Radius)
	if err != nil {
		t.Fatal(err)
	}
	if len(noChange) != 0 {
		t.Fatalf("identical parameters should not invalidate anything: %+v", noChange)
	}
	changed, err := graph.InvalidateParams(image.ID, cfg.Radius, RadiusConfig{Min: 1, Max: 7, CurvatureBands: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) == 0 {
		t.Fatal("changed radius band should invalidate dependent blocks")
	}
	for _, invalidation := range changed {
		if invalidation.Reason == "" || invalidation.SegmentID == "" {
			t.Fatalf("incomplete invalidation record: %+v", invalidation)
		}
	}
	validBlocks := 0
	for _, block := range graph.Blocks {
		if graph.IsValid(block.Block) {
			validBlocks++
		}
	}
	if validBlocks == 0 {
		t.Fatal("expected some unaffected blocks to remain reusable")
	}
	checkpoints := make([]Checkpoint, 0, len(graph.Blocks))
	for _, dependency := range graph.Blocks {
		checkpoints = append(checkpoints, Checkpoint{Block: dependency.Block})
	}
	reusable, invalidBlocks := graph.ReusableCheckpoints(checkpoints)
	if len(reusable) == 0 || len(invalidBlocks) == 0 {
		t.Fatalf("expected partial checkpoint reuse, reusable=%d invalid=%d", len(reusable), len(invalidBlocks))
	}

	graph = NewDependencyGraph(image.ID, image.InitialMask, image.Truth.Segments, cfg.BlockSize, cfg.Radius.Max, cfg.MaxRounds, table)
	edited := image.InitialMask.Clone()
	// Toggle a single pixel inside the first foreground component; only its
	// block and the blocks downstream of the nearest segment should disappear.
	changedPixel := -1
	for i, value := range edited.Data {
		if value != 0 {
			edited.Data[i] = 0
			changedPixel = i
			break
		}
	}
	if changedPixel < 0 {
		t.Fatal("sample has no foreground pixel to edit")
	}
	maskInvalid, err := graph.InvalidateInitialMask(image.ID, image.InitialMask, edited, cfg.Radius.Max)
	if err != nil {
		t.Fatal(err)
	}
	if len(maskInvalid) == 0 {
		t.Fatal("mask edit should invalidate at least one block")
	}
	hasReason := false
	for _, invalidation := range maskInvalid {
		if invalidation.Reason == "initial_mask_edited" {
			hasReason = true
		}
	}
	if !hasReason {
		t.Fatalf("expected initial_mask_edited reason: %+v", maskInvalid)
	}
	if graph.IsValid(Coord{X: changedPixel % image.Width / cfg.BlockSize, Y: changedPixel / image.Width / cfg.BlockSize}) {
		t.Fatal("edited block must be invalid")
	}
}
