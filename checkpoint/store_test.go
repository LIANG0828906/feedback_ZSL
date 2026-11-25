package checkpoint_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"example.com/columnar/checkpoint"
	"example.com/columnar/domain"
	"example.com/columnar/engine"
	"example.com/columnar/generator"
	"example.com/columnar/loader"
)

func TestCheckpointRecoveryRollbackVersionAndTruncation(t *testing.T) {
	ctx := context.Background()
	s, err := generator.Generate(generator.Seed)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loader.Load(s.VersionJSON["v2"])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var last domain.CheckpointData
	result, err := engine.Run(p, engine.Options{OnCommit: func(data domain.CheckpointData) error {
		last = data
		return store.Save(ctx, data)
	}})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Load(ctx, checkpoint.Expectation{
		SchemaVersion: "v2", EngineVersion: engine.EngineVersion, ContentDigest: engine.ContentDigest(p),
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Integrity != "ok" || recovered.Data.CommittedOp != 11 || len(recovered.Data.Batches) != 11 || len(recovered.Data.ScanCache) != 8 {
		t.Fatalf("unexpected recovery: %+v", recovered)
	}
	if !reflect.DeepEqual(recovered.Data.Batches, result.FinalBatches) {
		t.Fatal("recovered batch state differs from committed engine state")
	}

	failed := last
	failed.CommittedOp = 7
	err = store.SaveWithFailpoint(ctx, failed, func(stage string) error {
		if stage == "after_batches" {
			return errors.New("injected stop before meta update")
		}
		return nil
	})
	if !domain.HasCode(err, domain.CodeCheckpointWrite) {
		t.Fatalf("expected checkpoint write failure, got %v", err)
	}
	afterRollback, err := store.Load(ctx, checkpoint.Expectation{SchemaVersion: "v2", EngineVersion: engine.EngineVersion})
	if err != nil {
		t.Fatal(err)
	}
	if afterRollback.Data.CommittedOp != 11 || !reflect.DeepEqual(afterRollback.Data.Batches, result.FinalBatches) {
		t.Fatalf("rollback exposed partial batch state: %+v", afterRollback.Data)
	}

	_, err = store.Load(ctx, checkpoint.Expectation{SchemaVersion: "v3", EngineVersion: engine.EngineVersion})
	if !domain.HasCode(err, domain.CodeCheckpointVersion) {
		t.Fatalf("expected version mismatch, got %v", err)
	}
	_, err = store.Load(ctx, checkpoint.Expectation{SchemaVersion: "v2", EngineVersion: "other-engine"})
	if !domain.HasCode(err, domain.CodeCheckpointVersion) {
		t.Fatalf("expected engine mismatch, got %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated.db")
	if err := os.WriteFile(truncated, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := checkpoint.Open(truncated)
	if ts != nil {
		_ = ts.Close()
	}
	if !domain.HasCode(err, domain.CodeCheckpointTruncated) {
		t.Fatalf("expected truncated checkpoint, got %v", err)
	}

	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not a sqlite database at all, just bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, err := checkpoint.Open(corrupt)
	if cs != nil {
		_ = cs.Close()
	}
	if !domain.HasCode(err, domain.CodeCheckpointTruncated) {
		t.Fatalf("expected corrupt/truncated header, got %v", err)
	}

	emptyStore, err := checkpoint.Open(filepath.Join(dir, "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer emptyStore.Close()
	_, err = emptyStore.Load(ctx, checkpoint.Expectation{SchemaVersion: "v2", EngineVersion: engine.EngineVersion})
	if !domain.HasCode(err, domain.CodeCheckpointCorrupt) {
		t.Fatalf("expected empty checkpoint error, got %v", err)
	}
}

func TestForcedTerminationAtBatchWrite(t *testing.T) {
	if os.Getenv("COLUMNAR_CRASH_HELPER") == "1" {
		crashHelper()
		return
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestForcedTerminationAtBatchWrite$")
	cmd.Env = append(os.Environ(), "COLUMNAR_CRASH_HELPER=1", "COLUMNAR_CRASH_DB="+dbPath)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("helper did not terminate at failpoint: err=%v output=%s", err, out)
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovered, err := store.Load(context.Background(), checkpoint.Expectation{
		SchemaVersion: "v2", EngineVersion: engine.EngineVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Data.CommittedOp != 1 {
		t.Fatalf("child exposed uncommitted op: %d", recovered.Data.CommittedOp)
	}
	if len(recovered.Data.Batches) != 2 || recovered.Data.Batches[0].ID != 10 || recovered.Data.Batches[1].ID != 20 {
		t.Fatalf("child exposed uncommitted batches: %+v", recovered.Data.Batches)
	}
}

func crashHelper() {
	ctx := context.Background()
	path := os.Getenv("COLUMNAR_CRASH_DB")
	store, err := checkpoint.Open(path)
	if err != nil {
		os.Exit(31)
	}
	initial := domain.CheckpointData{
		SchemaVersion: "v2", EngineVersion: engine.EngineVersion, CommittedOp: 1,
		Batches: []domain.Batch{
			{ID: 10, Family: "core", Column: "id", RowStart: 0, RowEnd: 100, Rows: 100, Encoding: "rle", CompressedBytes: 100, Digest: 11, DecodeBufferBytes: 800, Status: domain.BatchActive},
			{ID: 20, Family: "core", Column: "id", RowStart: 100, RowEnd: 200, Rows: 100, Encoding: "rle", CompressedBytes: 100, Digest: 22, DecodeBufferBytes: 800, Status: domain.BatchActive},
		},
	}
	if err := store.Save(ctx, initial); err != nil {
		os.Exit(32)
	}
	crash := initial
	crash.CommittedOp = 2
	crash.Batches[0].Rows = 999
	_ = store.SaveWithFailpoint(ctx, crash, func(stage string) error {
		if stage == "after_batches" {
			os.Exit(23)
		}
		return nil
	})
	os.Exit(33)
}

func TestCheckpointBatchJSONStable(t *testing.T) {
	a := []domain.Batch{{ID: 1, DependsOn: nil}}
	b := []domain.Batch{{ID: 1, DependsOn: []int{}}}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("batch serialization is not canonical: %s != %s", ja, jb)
	}
}

func TestAnomalyAfterCommitKeepsPreviousCheckpointReadable(t *testing.T) {
	ctx := context.Background()
	s, err := generator.Generate(generator.Seed)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := loader.Load(s.VersionJSON["v2"])
	if err != nil {
		t.Fatal(err)
	}
	validRun, err := engine.Run(valid, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bad := valid.Clone()
	bad.Operations[3].Digest ^= 1
	dir := t.TempDir()
	store, err := checkpoint.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = engine.Run(bad, engine.Options{OnCommit: func(data domain.CheckpointData) error {
		return store.Save(ctx, data)
	}})
	if !domain.HasCode(err, domain.CodeCompactDrift) {
		t.Fatalf("expected compact drift, got %v", err)
	}
	recovered, err := store.Load(ctx, checkpoint.Expectation{SchemaVersion: "v2", EngineVersion: engine.EngineVersion})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Data.CommittedOp != 3 {
		t.Fatalf("checkpoint advanced past failed op: %d", recovered.Data.CommittedOp)
	}
	var want []domain.Batch
	for _, op := range validRun.Operations {
		if op.Index == 3 {
			want = op.Batches
			break
		}
	}
	if !reflect.DeepEqual(recovered.Data.Batches, want) {
		t.Fatal("previously committed batches changed or became unreadable")
	}
}
