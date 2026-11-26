package pipeline_test

import (
	"encoding/json"
	"sync"
	"testing"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/generate"
	"lip-pipeline-prototype/internal/pipeline"
)

func TestTruthPacksFramesAndPreviewConsistency(t *testing.T) {
	sample, err := generate.BuildSample()
	if err != nil {
		t.Fatal(err)
	}
	shardByID := map[string]domain.Shard{}
	for _, dlg := range sample.Project.Dialogues {
		for _, shard := range dlg.Shards {
			shardByID[shard.ID] = shard
		}
	}

	transitionCount := 0
	for _, id := range sample.TruthOrder {
		result := sample.TruthByShard[id]
		shard := shardByID[id]
		if len(result.Packs) == 0 || len(result.Frames) == 0 {
			t.Fatalf("%s produced empty result", id)
		}
		for _, pack := range result.Packs {
			if len(pack.PhonemeIDs) > sample.Project.Params.MaxPhonemesPerPack {
				t.Fatalf("pack too large: %d", len(pack.PhonemeIDs))
			}
			hasSilence := false
			for _, category := range pack.Categories {
				hasSilence = hasSilence || category == domain.CategorySilence
			}
			if hasSilence && len(pack.PhonemeIDs) != 1 {
				t.Fatalf("pack crosses silence boundary: %+v", pack.PhonemeIDs)
			}
		}
		if int64(len(result.Frames)) != result.TotalFrames {
			t.Fatalf("frame length %d != total %d", len(result.Frames), result.TotalFrames)
		}
		for i, frame := range result.Frames {
			if frame.Index != int64(i) {
				t.Fatalf("frame index order at %d: %d", i, frame.Index)
			}
		}
		transitionCount += len(result.Transitions)

		preview, err := pipeline.BuildPreview(shard, sample.Project.Params)
		if err != nil {
			t.Fatalf("preview %s: %v", id, err)
		}
		if !preview.Stats.WithinBudget {
			t.Fatalf("preview exceeded declared budget: %+v", preview.Stats)
		}
		if err := pipeline.ComparePreviewExact(preview, result); err != nil {
			t.Fatalf("preview/exact mismatch %s: %v", id, err)
		}
	}
	if transitionCount == 0 {
		t.Fatal("generator should create non-contiguous family transitions")
	}
}

func TestPipelineIsDeterministicAcrossConcurrentBuilds(t *testing.T) {
	sample, err := generate.BuildSample()
	if err != nil {
		t.Fatal(err)
	}
	shard := sample.Project.Dialogues[0].Shards[0]
	first, err := pipeline.BuildShard(shard, sample.Project.Params, "h", domain.SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(first)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := pipeline.BuildShard(shard, sample.Project.Params, "h", domain.SchemaV3)
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := json.Marshal(got)
			if string(data) != string(want) {
				t.Error("concurrent build result changed")
			}
		}()
	}
	wg.Wait()
}

func TestFamilySpanBeyondToleranceReturnsELip(t *testing.T) {
	sample, err := generate.BuildSample()
	if err != nil {
		t.Fatal(err)
	}
	params := sample.Project.Params
	params.TransitionTolerance = 1
	found := false
	for _, dlg := range sample.Project.Dialogues {
		for _, shard := range dlg.Shards {
			_, err := pipeline.BuildShard(shard, params, dlg.ContentHash, domain.SchemaV3)
			de, ok := domain.AsError(err)
			if ok && de.Code == domain.CodeLip {
				found = true
				if de.Frame == nil || de.Actual != 2 {
					t.Fatalf("E_LIP should include frame and measured span=2: %+v", de)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected at least one E_LIP when tolerance=1")
	}
}
