package syncgame

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestCheckpointRestartAndResume(t *testing.T) {
	sample := testFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoints.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	runID := "restart-test"
	opts := RunOptions{
		Parallel:    true,
		Workers:     4,
		Store:       store,
		RunID:       runID,
		WindowTicks: 20,
		InterruptAt: 80,
	}
	if _, err := RunRealtime(sample, opts); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkpoint, err := store.LoadLatest(context.Background(), sample, runID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Tick != 80 {
		t.Fatalf("expected persisted tick 80, got %d", checkpoint.Tick)
	}
	recovered, err := ResumeAuthoritative(sample, checkpoint, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := CompareAuthority(baseline, recovered); mismatch != nil {
		t.Fatalf("resume mismatch: %+v", mismatch)
	}
}

func TestSnapshotIdentityMismatchInvalidatesWholeRun(t *testing.T) {
	sample := testFixture(t)
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runID := "identity-test"
	if _, err := RunRealtime(sample, RunOptions{
		Store: store, RunID: runID, WindowTicks: 20, InterruptAt: 40,
	}); err != nil {
		t.Fatal(err)
	}
	bad := cloneSample(t, sample)
	bad.Seed++
	_, err = store.LoadLatest(context.Background(), bad, runID)
	gameError := requireCode(t, err, CodeSnapshot)
	if gameError.Details["invalidated_run"] != runID {
		t.Fatalf("expected invalidated run details: %+v", gameError)
	}
	count, err := store.CheckpointCount(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected all snapshots deleted, got %d", count)
	}
}

func TestCommittedWindowCannotBeRewritten(t *testing.T) {
	sample := testFixture(t)
	auth, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkpoint := makeCheckpoint(t, sample, auth, "immutable-test", 0, 0, 20)
	if _, err := store.SaveWindow(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	changed := makeCheckpoint(t, sample, auth, "immutable-test", 0, 0, 20)
	changed.State.Units[0].Resource++
	changed.Trace[len(changed.Trace)-1] = CloneWorld(changed.State)
	changed.Seal()
	_, err = store.SaveWindow(context.Background(), changed)
	gameError := requireCode(t, err, CodeConflict)
	if gameError.Field != "snapshot_hash" {
		t.Fatalf("unexpected conflict: %+v", gameError)
	}
	committed, err := store.SaveWindow(context.Background(), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("idempotent retry should not insert another checkpoint")
	}
}

func TestCheckpointOrderAndCorruption(t *testing.T) {
	sample := testFixture(t)
	auth, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	outOfOrder := makeCheckpoint(t, sample, auth, "order-test", 1, 0, 20)
	_, err = store.SaveWindow(context.Background(), outOfOrder)
	requireCode(t, err, CodeOrder)

	checkpoint := makeCheckpoint(t, sample, auth, "corrupt-test", 0, 0, 20)
	if _, err := store.SaveWindow(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	badState, _ := json.Marshal(auth.States[20])
	var unmarshaled map[string]any
	_ = json.Unmarshal(badState, &unmarshaled)
	unmarshaled["finished"] = true
	badState, _ = json.Marshal(unmarshaled)
	if _, err := store.db.Exec(`UPDATE checkpoints SET state_json=? WHERE run_id=? AND window_index=0`, badState, "corrupt-test"); err != nil {
		t.Fatal(err)
	}
	_, err = store.LoadLatest(context.Background(), sample, "corrupt-test")
	requireCode(t, err, CodeSnapshot)
	count, err := store.CheckpointCount(context.Background(), "corrupt-test")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("corrupt snapshot run was partially retained: %d", count)
	}
}

func TestFailedWindowRetryDoesNotAffectCommittedWindows(t *testing.T) {
	sample := testFixture(t)
	auth, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runID := "retry-test"
	if err := store.RecordWindowFailure(context.Background(), runID, 1, 20, 40, errors.New("injected")); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordWindowFailure(context.Background(), runID, 2, 40, 60, errors.New("injected")); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryWindow(context.Background(), runID, 1); err != nil {
		t.Fatal(err)
	}
	windows, err := store.Windows(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].Status != WindowRetrying || windows[1].Status != WindowFailed {
		t.Fatalf("window failure isolation broken: %+v", windows)
	}
	checkpoint := makeCheckpoint(t, sample, auth, runID, 0, 0, 20)
	if _, err := store.SaveWindow(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordWindowFailure(context.Background(), runID, 0, 0, 20, errors.New("late")); err == nil {
		t.Fatal("committed window must not accept a failure record")
	}
}

func TestSerialAndParallelCheckpointSequencesMatch(t *testing.T) {
	sample := testFixture(t)
	dir := t.TempDir()
	serialStore, err := OpenStore(filepath.Join(dir, "serial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer serialStore.Close()
	parallelStore, err := OpenStore(filepath.Join(dir, "parallel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer parallelStore.Close()
	serial, err := RunRealtime(sample, RunOptions{Store: serialStore, RunID: "serial", WindowTicks: 60})
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := RunRealtime(sample, RunOptions{Parallel: true, Workers: 4, Store: parallelStore, RunID: "parallel", WindowTicks: 60})
	if err != nil {
		t.Fatal(err)
	}
	if len(serial.Windows) != len(parallel.Windows) {
		t.Fatalf("window count differs: %d vs %d", len(serial.Windows), len(parallel.Windows))
	}
	for i := range serial.Windows {
		if serial.Windows[i].CommittedTick != parallel.Windows[i].CommittedTick ||
			serial.Windows[i].SnapshotHash != parallel.Windows[i].SnapshotHash {
			t.Fatalf("commit sequence differs at window %d: %+v vs %+v", i, serial.Windows[i], parallel.Windows[i])
		}
	}
}

func makeCheckpoint(t *testing.T, sample *Sample, auth *AuthorityRun, runID string, index, start, end int) Checkpoint {
	t.Helper()
	trace := cloneStates(auth.States[:end+1])
	checkpoint := Checkpoint{
		RunID:                runID,
		WindowIndex:          index,
		StartTick:            start,
		EndTick:              end,
		Tick:                 end,
		Seed:                 sample.Seed,
		AlgorithmVersion:     sample.AlgorithmVersion,
		StreamFingerprint:    sample.StreamFingerprint(),
		State:                CloneWorld(trace[len(trace)-1]),
		Trace:                trace,
		InputOffset:          end,
		RandomCursor:         end,
		ClientConfirmedFrame: end * sample.RenderFPS / sample.TickRate,
	}
	checkpoint.Seal()
	return checkpoint
}
