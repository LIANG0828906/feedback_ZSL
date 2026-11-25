package checkpoint_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"graphlab/internal/checkpoint"
	"graphlab/internal/engine"
	"graphlab/internal/generator"
	"graphlab/internal/loader"
	"graphlab/internal/model"
)

func setup(t *testing.T) (*engine.Runtime, *model.Project, *generator.Sample) {
	t.Helper()
	sample, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := loader.LoadBundle(sample.Documents)
	if err != nil {
		t.Fatal(err)
	}
	base := bundle.Projects[model.SchemaV1]
	rt, err := engine.NewRuntime(base, engine.DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.ApplyOperation(base.Operations[0], engine.LayoutIndex); err != nil {
		t.Fatal(err)
	}
	return rt, base, sample
}

func TestCheckpointRecoverAndContinue(t *testing.T) {
	rt, base, sample := setup(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(rt, checkpoint.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovered, header, err := store.Recover(rt.Project.ID, rt.Project.Version, engine.DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if header.CommitSeq != rt.CommitSeq || header.ProjectVersion != rt.Project.Version {
		t.Fatalf("header mismatch: %+v", header)
	}
	if model.CoreDigest(recovered.Project) != model.CoreDigest(rt.Project) {
		t.Fatal("recovered project differs")
	}
	for id, originalShard := range rt.Index.Shards {
		recoveredShard := recovered.Index.Shards[id]
		if recoveredShard == nil || recoveredShard.Version != originalShard.Version ||
			!same(recoveredShard.VertexOrder, originalShard.VertexOrder) ||
			!same(recoveredShard.EdgeOrder, originalShard.EdgeOrder) {
			t.Fatalf("recovered shard state differs at %s", id)
		}
	}
	recoveredKeys := recovered.Index.KeyViews()
	originalKeys := rt.Index.KeyViews()
	if len(recoveredKeys) != len(originalKeys) {
		t.Fatal("recovered index key count differs")
	}
	for i := range originalKeys {
		if recoveredKeys[i].Key != originalKeys[i].Key ||
			recoveredKeys[i].Version != originalKeys[i].Version ||
			!same(recoveredKeys[i].RefShards, originalKeys[i].RefShards) ||
			!same(recoveredKeys[i].Entities, originalKeys[i].Entities) {
			t.Fatalf("recovered key differs at %s", originalKeys[i].Key)
		}
	}
	gotResults, err := recovered.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		t.Fatal(err)
	}
	wantResults, err := rt.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		t.Fatal(err)
	}
	if diffs := engine.CompareQueryResults(gotResults, wantResults); len(diffs) > 0 {
		t.Fatalf("recovered query results differ: %v", diffs)
	}
	for _, entry := range rt.Cache.EntriesSorted() {
		if recovered.Cache.Entries[entry.CacheID] == nil {
			t.Fatalf("recovered cache entry missing: %s", entry.CacheID)
		}
	}

	if _, _, err := store.Recover(rt.Project.ID, rt.Project.Version-1, engine.DefaultBuildOptions()); !model.IsCode(err, model.ECheckpointVersionMismatch) {
		t.Fatalf("expected E_CHECKPOINT_VERSION_MISMATCH, got %v", err)
	}
	if _, err := recovered.ApplyOperation(base.Operations[1], engine.LayoutIndex); err != nil {
		t.Fatalf("recovered runtime cannot continue: %v", err)
	}
	if _, err := store.Commit(recovered, checkpoint.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = sample
}

func TestCheckpointFailpointKeepsLastCompleteCommit(t *testing.T) {
	rt, base, _ := setup(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Commit(rt, checkpoint.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	versionBefore := rt.Project.Version
	seqBefore := rt.CommitSeq
	if _, err := rt.ApplyOperation(base.Operations[1], engine.LayoutIndex); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected crash after index refs")
	_, err = store.Commit(rt, checkpoint.CommitOptions{
		Failpoint: func(stage string) error {
			if stage == checkpoint.StageIndexRefsWritten {
				return sentinel
			}
			return nil
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected failpoint sentinel, got %v", err)
	}
	recovered, header, err := store.Recover(rt.Project.ID, versionBefore, engine.DefaultBuildOptions())
	if err != nil {
		t.Fatalf("previous checkpoint unavailable after failed commit: %v", err)
	}
	if header.CommitSeq != seqBefore || recovered.Project.Version != versionBefore {
		t.Fatalf("checkpoint advanced past incomplete commit: header=%+v", header)
	}

	stages := []string{
		checkpoint.StageShardIndexWritten,
		checkpoint.StageIndexRefsWritten,
		checkpoint.StageCacheWritten,
		checkpoint.StageCommitLogWritten,
		checkpoint.StageMetaWritten,
	}
	for _, stage := range stages {
		_, err = store.Commit(rt, checkpoint.CommitOptions{
			Failpoint: func(current string) error {
				if current == stage {
					return errors.New("injected " + stage)
				}
				return nil
			},
		})
		if err == nil {
			t.Fatalf("stage %s failpoint did not abort", stage)
		}
		recovered, header, err = store.Recover(rt.Project.ID, versionBefore, engine.DefaultBuildOptions())
		if err != nil {
			t.Fatalf("stage %s lost previous checkpoint: %v", stage, err)
		}
		if header.CommitSeq != seqBefore || recovered.Project.Version != versionBefore {
			t.Fatalf("stage %s advanced checkpoint: %+v", stage, header)
		}
	}
	if _, err := store.Commit(rt, checkpoint.CommitOptions{}); err != nil {
		t.Fatalf("checkpoint could not commit after repeated aborts: %v", err)
	}
}

func TestCheckpointTruncatedAndVersionMismatch(t *testing.T) {
	rt, _, _ := setup(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(rt, checkpoint.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Recover("other-project", rt.Project.Version, engine.DefaultBuildOptions()); !model.IsCode(err, model.ECheckpointVersionMismatch) {
		t.Fatalf("expected project id mismatch, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()/3); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.Open(path)
	if !model.IsCode(err, model.ECheckpointTruncated) {
		t.Fatalf("expected E_CHECKPOINT_TRUNCATED, got %v", err)
	}
	emptyPath := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.Open(emptyPath)
	if !model.IsCode(err, model.ECheckpointTruncated) {
		t.Fatalf("expected empty file to be E_CHECKPOINT_TRUNCATED, got %v", err)
	}
}

func TestCommitIsDeterministicForSameRuntime(t *testing.T) {
	rt, _, _ := setup(t)
	dir := t.TempDir()
	var blobs [][]byte
	for i := 0; i < 2; i++ {
		path := filepath.Join(dir, "cp"+string(rune('a'+i))+".db")
		store, err := checkpoint.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Commit(rt, checkpoint.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
		entries := rt.Cache.EntriesSorted()
		var b bytes.Buffer
		for _, e := range entries {
			b.WriteString(e.CacheID)
			b.WriteByte('\n')
		}
		blobs = append(blobs, b.Bytes())
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Cache identity is logical-query based, so two commits of the same runtime
	// have identical cache ordering and IDs.
	if !bytes.Equal(blobs[0], blobs[1]) {
		t.Fatal("cache IDs changed between commits")
	}
}

func same(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
