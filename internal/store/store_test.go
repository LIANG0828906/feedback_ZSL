package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"warmcache/internal/domain"
	"warmcache/internal/engine"
	"warmcache/internal/gen"
	"warmcache/internal/loader"
	"warmcache/internal/store"
)

func testRun(t *testing.T) domain.RunResult {
	t.Helper()
	samples, err := gen.Samples(gen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	project, err := loader.Load(samples[0].Raw, domain.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{Ticks: 30, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSQLiteCheckpointRoundTrip(t *testing.T) {
	result := testRun(t)
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	checkpoint, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SaveResult(result); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 4096 {
		t.Fatalf("SQLite file unexpectedly small: %d", info.Size())
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, err := reopened.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if latest.Tick != 30 {
		t.Fatalf("latest tick %d", latest.Tick)
	}
	if latest.SemanticHash != result.SemanticHash {
		t.Fatal("semantic hash changed")
	}
	if len(latest.Shards) != len(result.Timeline[29].Shards) {
		t.Fatalf("shard count %d", len(latest.Shards))
	}
	if latest.StateHash != store.TickHash(result.Timeline[29]) {
		t.Fatal("checkpoint state hash mismatch")
	}
}

func TestCheckpointAdvancesOnlyByCompleteTicks(t *testing.T) {
	result := testRun(t)
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	checkpoint, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	partial := result
	partial.Timeline = append([]domain.TickState(nil), result.Timeline[:7]...)
	if err := checkpoint.SaveResult(partial); err != nil {
		t.Fatal(err)
	}
	latest, err := checkpoint.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if latest.Tick != 7 {
		t.Fatalf("expected tick 7, got %d", latest.Tick)
	}
	if err := checkpoint.SaveResult(result); err != nil {
		t.Fatal(err)
	}
	latest, err = checkpoint.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if latest.Tick != 30 {
		t.Fatalf("expected advanced tick 30, got %d", latest.Tick)
	}
	_ = checkpoint.Close()
}

func TestTruncatedAndVersionMismatchCodes(t *testing.T) {
	directory := t.TempDir()
	truncated := filepath.Join(directory, "truncated.sqlite")
	if err := os.WriteFile(truncated, []byte("SQLite format 3"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.Open(truncated)
	if domain.AsBizError(err).Code != domain.CodeCheckpointTruncated {
		t.Fatalf("expected truncated code, got %v", err)
	}

	path := filepath.Join(directory, "version.sqlite")
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetMeta("snapshot_version", "warmcache-checkpoint.v0"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Open(path)
	if domain.AsBizError(err).Code != domain.CodeSnapshotVersion {
		t.Fatalf("expected version mismatch, got %v", err)
	}
}

func TestCheckpointOrderRejectsChildBeyondWatermark(t *testing.T) {
	result := testRun(t)
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	checkpoint, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Close()
	partial := result
	partial.Timeline = append([]domain.TickState(nil), result.Timeline[:7]...)
	if err := checkpoint.SaveResult(partial); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.DB().Exec(`INSERT INTO shard_state(
		run_id, tick, shard_id, node_id, target_version, visible_version, progress,
		committed_progress, preheat_ticks, available, lease_id, lease_node,
		lease_expires_tick, generation, pending_replay, budget_capacity, payload)
		VALUES(?, 99, 'orphan', 'edge-a', 4, 3, 1, 1, 4, 0, '', '', 0, 1, 0, 200, '{}')`, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.Latest()
	if domain.AsBizError(err).Code != domain.CodeCheckpointOrder {
		t.Fatalf("expected checkpoint order code, got %v", err)
	}
}

func TestExitSignalPersistsLastCompleteCommit(t *testing.T) {
	samples, err := gen.Samples(gen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	project, err := loader.Load(samples[1].Raw, domain.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{
		Ticks: 30, Workers: 3,
		Interrupts: []domain.Interrupt{{ID: "store-exit", Tick: 6, ShardID: "shard-03", Reason: "sigterm", ExitSignal: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Interrupted || result.StoppedAtTick != 6 {
		t.Fatalf("run did not stop at tick 6: %+v", result)
	}
	path := filepath.Join(t.TempDir(), "exit.sqlite")
	checkpoint, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SaveResult(result); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, err := reopened.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if latest.Tick != 6 {
		t.Fatalf("expected checkpoint at tick 6, got %d", latest.Tick)
	}
	for _, shard := range latest.Shards {
		if shard.Progress != shard.CommittedProgress {
			t.Fatalf("shard %s crossed a partial commit boundary", shard.ShardID)
		}
	}
}
