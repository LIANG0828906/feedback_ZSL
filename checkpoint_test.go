package orchestrator

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *CheckpointStore {
	t.Helper()
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "och.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestThreePathsProduceIdenticalResults(t *testing.T) {
	p := genProject(t)
	order := p.Policy.QueueOrder

	// Path 1: live orchestration with per-dialogue checkpoints.
	liveStore := openStore(t)
	live, lerr := RunLive(p, liveStore)
	if lerr != nil {
		t.Fatalf("live run: %v", lerr)
	}

	// Path 2: crash after 2 commits, restart and continue.
	resStore := openStore(t)
	if rerr := RunResumable(p, resStore, 2); rerr != nil {
		t.Fatalf("resumable run: %v", rerr)
	}
	resumed, cerr := ContinueFromCheckpoint(p, resStore)
	if cerr != nil {
		t.Fatalf("continue: %v", cerr)
	}

	// Path 3: one-shot full batch.
	batchStore := openStore(t)
	batch, berr := RunBatch(p, batchStore)
	if berr != nil {
		t.Fatalf("batch run: %v", berr)
	}

	if derr := CompareResults(live, resumed, order); derr != nil {
		t.Fatalf("live vs resumed: %v", derr)
	}
	if derr := CompareResults(live, batch, order); derr != nil {
		t.Fatalf("live vs batch: %v", derr)
	}

	// Commit sequences are stable and match the queue order.
	for name, store := range map[string]*CheckpointStore{"live": liveStore, "resumed": resStore, "batch": batchStore} {
		log, err := store.CommitLog()
		if err != nil {
			t.Fatalf("%s commit log: %v", name, err)
		}
		if len(log) != len(order) {
			t.Fatalf("%s commit log length %d != %d", name, len(log), len(order))
		}
		for i := range order {
			if log[i] != order[i] {
				t.Fatalf("%s commit %d: got %s want %s", name, i, log[i], order[i])
			}
		}
	}
}

func TestCheckpointInvalidationOnVersionOrHashChange(t *testing.T) {
	p := genProject(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "och.db")
	store, err := OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, lerr := RunLive(p, store); lerr != nil {
		t.Fatalf("live run: %v", lerr)
	}
	store.Close()

	// Corrupt the algo version in the snapshot.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`UPDATE meta SET value='0.0.0' WHERE key='algo_version'`); err != nil {
		t.Fatalf("tamper meta: %v", err)
	}
	raw.Close()

	store2, err := OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	valid, err := store2.CheckValidity(p.ContentHash())
	if err != nil {
		t.Fatalf("check validity: %v", err)
	}
	if valid {
		t.Fatal("snapshot with stale algo version must be invalid")
	}
	results, err := store2.ReadResults()
	if err != nil {
		t.Fatalf("read results: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("invalid snapshot must be wiped, %d results remain", len(results))
	}

	// Content hash mismatch also invalidates everything.
	store3 := openStore(t)
	if _, lerr := RunLive(p, store3); lerr != nil {
		t.Fatalf("live run: %v", lerr)
	}
	p.Dialogues[0].Rhythm[0] += 0.25
	valid, err = store3.CheckValidity(p.ContentHash())
	if err != nil {
		t.Fatalf("check validity: %v", err)
	}
	if valid {
		t.Fatal("snapshot with stale content hash must be invalid")
	}
}
