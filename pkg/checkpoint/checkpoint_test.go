package checkpoint_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"lipsync.local/prototype/pkg/checkpoint"
	"lipsync.local/prototype/pkg/domain"
)

func TestStateMachineKeepsStateOnIllegalTransition(t *testing.T) {
	machine := checkpoint.NewShardMachine("s1", "d1")
	if err := machine.Transition(checkpoint.StateQuantizing); err != nil {
		t.Fatal(err)
	}
	if err := machine.SetProgress(50); err != nil {
		t.Fatal(err)
	}
	err := machine.Transition(checkpoint.StateCommitted)
	if code := domain.CodeOf(err); code != domain.ErrState {
		t.Fatalf("want E_STATE, got %s: %v", code, err)
	}
	snap := machine.Snapshot()
	if snap.State != checkpoint.StateQuantizing || snap.Progress != 50 {
		t.Fatalf("illegal transition mutated state: %+v", snap)
	}
}

func TestAtomicCheckpointAndVersionedRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.sqlite")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := json.RawMessage(`{"accepted":["d1/pkg/000"]}`)
	for i, id := range []string{"s1", "s2"} {
		if err := store.CommitCheckpoint(context.Background(), checkpoint.Checkpoint{
			ShardID: id, DialogueID: "d" + id, PackageSummary: []string{"pkg-" + id},
			Dispatch: dispatch, ContentHash: "hash-" + id,
			RecipeVersion: "r1", AlgorithmVersion: "a1",
		}); err != nil {
			t.Fatal(err)
		}
		if count, err := store.ShardCount(context.Background()); err != nil || count != i+1 {
			t.Fatalf("count=%d err=%v", count, err)
		}
	}
	committed, err := store.Committed(context.Background(), "r1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 2 || committed[0].CommitSeq != 1 || committed[1].CommitSeq != 2 {
		t.Fatalf("unstable commit sequence: %+v", committed)
	}
	if string(committed[0].Dispatch) != string(dispatch) || committed[0].PackageSummary[0] != "pkg-s1" {
		t.Fatalf("checkpoint fields not preserved: %+v", committed[0])
	}
	recovered, err := store.Recover(context.Background(), map[string]string{"s1": "hash-s1"}, "r1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ShardID != "s1" {
		t.Fatalf("unexpected recovery: %+v", recovered)
	}
	recovered, err = store.Recover(context.Background(), map[string]string{"s1": "hash-s1"}, "r2", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 0 {
		t.Fatalf("version-mismatched shards recovered: %+v", recovered)
	}
	store.Close()

	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	committed, err = store.Committed(context.Background(), "r1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 2 {
		t.Fatalf("reopen lost committed data: %d", len(committed))
	}
}

func TestCheckpointRejectsMissingFields(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.CommitCheckpoint(context.Background(), checkpoint.Checkpoint{ShardID: "s1"})
	if code := domain.CodeOf(err); code != domain.ErrSchema {
		t.Fatalf("want E_SCHEMA, got %s: %v", code, err)
	}
	count, _ := store.ShardCount(context.Background())
	if count != 0 {
		t.Fatalf("invalid checkpoint wrote rows: %d", count)
	}
}

func TestConcurrentCommitsAreSerialized(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "concurrent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const shards = 12
	var wg sync.WaitGroup
	errCh := make(chan error, shards)
	for i := 0; i < shards; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("shard-%03d", i)
			dispatch := json.RawMessage(fmt.Sprintf(`{"shard":%d}`, i))
			errCh <- store.CommitCheckpoint(context.Background(), checkpoint.Checkpoint{
				ShardID: id, DialogueID: fmt.Sprintf("d-%03d", i),
				PackageSummary: []string{"pkg-" + id}, Dispatch: dispatch,
				ContentHash: "hash-" + id, RecipeVersion: "r1", AlgorithmVersion: "a1",
			})
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	committed, err := store.Committed(context.Background(), "r1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != shards {
		t.Fatalf("got %d checkpoints", len(committed))
	}
	seen := make(map[int64]string, shards)
	for _, cp := range committed {
		if previous, ok := seen[cp.CommitSeq]; ok {
			t.Fatalf("interleaved commit sequence %d: %s and %s", cp.CommitSeq, previous, cp.ShardID)
		}
		seen[cp.CommitSeq] = cp.ShardID
		var payload map[string]int
		if err := json.Unmarshal(cp.Dispatch, &payload); err != nil {
			t.Fatal(err)
		}
		if "shard-"+fmt.Sprintf("%03d", payload["shard"]) != cp.ShardID {
			t.Fatalf("interleaved checkpoint payload: %+v", cp)
		}
	}
	for i := int64(1); i <= shards; i++ {
		if _, ok := seen[i]; !ok {
			t.Fatalf("missing commit seq %d", i)
		}
	}
}

func TestCancelledCommitDoesNotAdvanceRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.sqlite")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cp := checkpoint.Checkpoint{
		ShardID: "s1", DialogueID: "d1", PackageSummary: []string{"pkg"},
		Dispatch: json.RawMessage(`{}`), ContentHash: "h1",
		RecipeVersion: "r1", AlgorithmVersion: "a1",
	}
	if err := store.CommitCheckpoint(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cp.ShardID = "s2"
	cp.DialogueID = "d2"
	cp.ContentHash = "h2"
	if err := store.CommitCheckpoint(ctx, cp); err == nil {
		t.Fatal("cancelled checkpoint unexpectedly committed")
	}
	store.Close()

	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovered, err := store.Recover(context.Background(), map[string]string{"s1": "h1", "s2": "h2"}, "r1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ShardID != "s1" {
		t.Fatalf("recovery used uncommitted state: %+v", recovered)
	}
}
