package loudness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func sampleCheckpoint(t *testing.T, project *Project, clip Clip) ClipCheckpoint {
	t.Helper()
	measurement, err := MeasureClip(project, clip)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := calibrateClip(project, clip, measurement)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ := BuildDependencyGraph(project)
	attachGraphKeysForTest(&result, graph)
	return checkpointFromResult(project, result, clip.ID)
}

func attachGraphKeysForTest(result *ClipResult, graph *DependencyGraph) {
	frameKeys := graph.FrameCacheKeys(result.ClipID)
	for i := range result.Frames {
		if i < len(frameKeys) {
			result.Frames[i].CacheKey = frameKeys[i]
		}
	}
	result.MeasurementCacheKey = graph.MeasurementCacheKey(result.ClipID)
	result.CalibrationCacheKey = graph.CalibrationCacheKey(result.ClipID)
}

func TestCheckpointAtomicSerializedWrites(t *testing.T) {
	generated := mustGeneratedV3(t)
	store, err := NewCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := sampleCheckpoint(t, generated.Project, generated.Project.Clips[0])
	const writers = 24
	var wait sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		i := i
		wait.Add(1)
		go func() {
			defer wait.Done()
			checkpoint := base
			checkpoint.ClipID = i
			checkpoint.CommitIndex = i
			checkpoint.Frames = cloneFrames(base.Frames)
			for j := range checkpoint.Frames {
				checkpoint.Frames[j].ClipID = i
			}
			if err := store.Write(context.Background(), checkpoint); err != nil {
				errs <- err
				return
			}
			readback, ok, err := store.ReadClip(i)
			if err != nil || !ok {
				errs <- err
				return
			}
			if readback.EnvelopeDigest != checkpoint.EnvelopeDigest || len(readback.Frames) != len(checkpoint.Frames) {
				errs <- &DomainError{Code: ESchema, Path: "$.checkpoint", Detail: "concurrent readback mismatch"}
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != writers {
		t.Fatalf("read %d checkpoints, want %d", len(all), writers)
	}
	for i, checkpoint := range all {
		if checkpoint.CommitIndex != i {
			t.Fatalf("commit order %d = %d", i, checkpoint.CommitIndex)
		}
		if checkpoint.SourceVersion != generated.Version || checkpoint.QuantVersion == "" || checkpoint.AlgorithmVersion != AlgorithmVersion {
			t.Fatalf("checkpoint identity missing: %+v", checkpoint)
		}
	}
	entries, _ := os.ReadDir(store.Dir())
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatalf("temporary checkpoint file left behind: %s", entry.Name())
		}
	}
}

func TestCheckpointCorruptionAndPartialResume(t *testing.T) {
	generated := mustGeneratedV3(t)
	store, _ := NewCheckpointStore(t.TempDir())
	for i := 0; i < 3; i++ {
		checkpoint := sampleCheckpoint(t, generated.Project, generated.Project.Clips[i])
		checkpoint.CommitIndex = i
		if err := store.Write(context.Background(), checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RemoveClip(1); err != nil {
		t.Fatal(err)
	}
	all, err := store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ClipID != 0 || all[1].ClipID != 2 {
		t.Fatalf("partial resume checkpoints: %+v", all)
	}

	checkpoint := sampleCheckpoint(t, generated.Project, generated.Project.Clips[0])
	checkpoint.EnvelopeDigest = "corrupt"
	data := mustJSON(t, checkpoint)
	if err := os.WriteFile(filepath.Join(store.Dir(), "clip-000009.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.ReadAll()
	if !HasCode(err, ESchema) {
		t.Fatalf("corrupt digest should be E_SCHEMA, got %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
