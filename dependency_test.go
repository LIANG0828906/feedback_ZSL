package loudness

import (
	"context"
	"testing"
)

func TestDependencyInvalidationIsPrecise(t *testing.T) {
	generated := mustGeneratedV3(t)
	graph, err := BuildDependencyGraph(generated.Project)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := graph.Snapshot()
	kinds := make(map[NodeKind]int)
	for _, node := range snapshot.Nodes {
		kinds[node.Kind]++
	}
	if kinds[NodeSource] != len(generated.Project.Clips) || kinds[NodeCalibration] != len(generated.Project.Clips) || kinds[NodeFrameEnvelope] == 0 {
		t.Fatalf("graph node counts: %+v", kinds)
	}

	gainInvalidations, err := graph.MarkChange(Change{Kind: ChangeGain, ClipID: 2, GainQ12: 6144})
	if err != nil {
		t.Fatal(err)
	}
	if len(gainInvalidations) == 0 {
		t.Fatal("gain edit did not invalidate a range")
	}
	for _, invalidation := range gainInvalidations {
		if invalidation.ClipID != 2 || invalidation.Phase != PhaseEnvelope {
			t.Fatalf("gain invalidation escaped clip: %+v", invalidation)
		}
	}

	graph, _ = BuildDependencyGraph(generated.Project)
	targetInvalidations, err := graph.MarkChange(Change{Kind: ChangeTarget, TargetLoudnessDBQ8: generated.Project.TargetLoudnessDBQ8 - 256})
	if err != nil {
		t.Fatal(err)
	}
	seenClips := make(map[int]bool)
	for _, invalidation := range targetInvalidations {
		seenClips[invalidation.ClipID] = true
		if invalidation.Phase != PhaseCalibration {
			t.Fatalf("target invalidation phase = %s", invalidation.Phase)
		}
	}
	if len(seenClips) != len(generated.Project.Clips) {
		t.Fatalf("target invalidated %d clips, want %d", len(seenClips), len(generated.Project.Clips))
	}
	for _, node := range graph.Snapshot().Nodes {
		if node.Kind == NodeFrameEnvelope && !node.Valid {
			t.Fatal("target edit invalidated a measurement frame node")
		}
	}

	graph, _ = BuildDependencyGraph(generated.Project)
	frameInvalidations, err := graph.MarkChange(Change{Kind: ChangeFrameRate, FrameLength: 192, FrameShift: 96})
	if err != nil {
		t.Fatal(err)
	}
	seenClips = make(map[int]bool)
	for _, invalidation := range frameInvalidations {
		seenClips[invalidation.ClipID] = true
	}
	if len(seenClips) != len(generated.Project.Clips) {
		t.Fatal("frame-rate edit did not invalidate all clips")
	}
}

func TestCacheKeysFollowUpstreamAndQuantization(t *testing.T) {
	generated := mustGeneratedV3(t)
	baseGraph, _ := BuildDependencyGraph(generated.Project)
	baseFrames := baseGraph.FrameCacheKeys(2)
	baseCal := baseGraph.CalibrationCacheKey(2)
	otherFrames := baseGraph.FrameCacheKeys(3)

	targetProject := CloneProject(generated.Project)
	targetProject.TargetLoudnessDBQ8 -= 256
	targetGraph, _ := BuildDependencyGraph(targetProject)
	if got := targetGraph.FrameCacheKeys(2); !equalStrings(got, baseFrames) {
		t.Fatal("target edit changed measurement frame cache keys")
	}
	if targetGraph.CalibrationCacheKey(2) == baseCal {
		t.Fatal("target edit did not change calibration cache key")
	}

	gainProject := CloneProject(generated.Project)
	clip, _ := gainProject.ClipByID(2)
	clip.GainQ12 += 256
	gainGraph, _ := BuildDependencyGraph(gainProject)
	if equalStrings(gainGraph.FrameCacheKeys(2), baseFrames) {
		t.Fatal("gain edit did not change affected frame cache keys")
	}
	if !equalStrings(gainGraph.FrameCacheKeys(3), otherFrames) {
		t.Fatal("gain edit changed unaffected frame cache keys")
	}

	keyA := MakeCacheKey("content", "range", "q1", []string{"up-a", "up-b"})
	keyB := MakeCacheKey("content", "range", "q1", []string{"up-b", "up-a"})
	keyC := MakeCacheKey("content", "range", "q2", []string{"up-a", "up-b"})
	if keyA != keyB || keyA == keyC {
		t.Fatal("cache key formula is not stable or quant-version-sensitive")
	}
}

func TestEngineSkipsNoOpChanges(t *testing.T) {
	generated := mustGeneratedV3(t)
	engine, err := NewEngine(generated.Project, EngineOptions{CheckpointDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	clip := generated.Project.Clips[2]
	invalidations, err := engine.ApplyChanges([]Change{
		{Kind: ChangeGain, ClipID: clip.ID, GainQ12: clip.GainQ12},
		{Kind: ChangeTarget, TargetLoudnessDBQ8: generated.Project.TargetLoudnessDBQ8},
		{Kind: ChangeFrameRate, FrameLength: generated.Project.FrameLength, FrameShift: generated.Project.FrameShift},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidations) != 0 {
		t.Fatalf("no-op changes invalidated %d intervals", len(invalidations))
	}
	if _, err := engine.Run(context.Background(), RunIncremental); err != nil {
		t.Fatal(err)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
