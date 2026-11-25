package checkpoint_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"mvccworkbench/internal/checkpoint"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/generator"
)

func TestCheckpointResumeOnlyFromMatchingVersion(t *testing.T) {
	ctx := context.Background()
	suite, err := generator.DefaultSuite(424242)
	if err != nil {
		t.Fatal(err)
	}
	batch := suite.Batches["B1-visibility"]
	dbPath := filepath.Join(t.TempDir(), "resume.db")

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := engine.NewStreamRunner(batch.Project, engine.DefaultOptions())
	crashSeq := 7
	for {
		op, done, err := runner.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if err := runner.SaveCheckpoint(ctx, store); err != nil {
			t.Fatal(err)
		}
		if op.Index == crashSeq {
			break
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.CommittedOpSeq != crashSeq {
		t.Fatalf("checkpoint seq=%d want %d", state.CommittedOpSeq, crashSeq)
	}
	resumed := engine.NewStreamRunner(batch.Project, engine.DefaultOptions())
	if err := resumed.RestoreState(state); err != nil {
		t.Fatal(err)
	}
	if resumed.LastCommittedSeq() != crashSeq {
		t.Fatalf("resume seq=%d want %d", resumed.LastCommittedSeq(), crashSeq)
	}
	if err := resumed.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := resumed.SaveCheckpoint(ctx, reopened); err != nil {
		t.Fatal(err)
	}
	report := resumed.Report()
	for _, got := range report.Reads {
		if got.OpIndex <= crashSeq {
			continue
		}
		want := batch.Truth.Reads[got.OpID]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("resumed read %s mismatch:\ngot=%#v\nwant=%#v", got.OpID, got, want)
		}
	}
	for _, got := range report.GC {
		if got.AfterOpIndex <= crashSeq {
			continue
		}
		var want *domain.GCDecision
		for i := range batch.Truth.GC {
			if batch.Truth.GC[i].StepID == got.StepID {
				want = &batch.Truth.GC[i]
			}
		}
		if want == nil || !reflect.DeepEqual(got, *want) {
			t.Fatalf("resumed gc %s mismatch want=%#v", got.StepID, want)
		}
	}
}

func TestCheckpointVersionMismatchForcesFullReplay(t *testing.T) {
	ctx := context.Background()
	suite, _ := generator.DefaultSuite(424242)
	batch := suite.Batches["B2-conflict-gc"]
	store := openTestStore(t, "version.db")
	defer store.Close()

	runner := engine.NewStreamRunner(batch.Project, engine.DefaultOptions())
	if err := runner.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.SaveCheckpoint(ctx, store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE checkpoint_meta SET project_version = ?`, "wrong/v9"); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(ctx)
	if domain.CodeOf(err) != domain.ECkptVersionMismatch {
		t.Fatalf("code=%s err=%v", domain.CodeOf(err), err)
	}
	if err := store.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(ctx)
	if domain.CodeOf(err) != "E_CHECKPOINT_NOT_FOUND" {
		t.Fatalf("after wipe load code=%s state=%v err=%v", domain.CodeOf(err), state, err)
	}

	full, err := engine.Run(ctx, batch.Project, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Reads) != len(batch.Truth.Reads) {
		t.Fatalf("full replay reads=%d want %d", len(full.Reads), len(batch.Truth.Reads))
	}
}

func TestInjectedCheckpointExceptionsAreStable(t *testing.T) {
	ctx := context.Background()
	suite, _ := generator.DefaultSuite(424242)
	batch := suite.Batches["B1-visibility"]
	store := openTestStore(t, "exceptions.db")
	defer store.Close()
	runner := engine.NewStreamRunner(batch.Project, engine.DefaultOptions())
	if err := runner.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.SaveCheckpoint(ctx, store); err != nil {
		t.Fatal(err)
	}

	if err := store.InjectTruncatedChain(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(ctx)
	if domain.CodeOf(err) != domain.ECkptTruncated {
		t.Fatalf("truncated code=%s err=%v", domain.CodeOf(err), err)
	}

	// Restore a valid database state before injecting a bad snapshot version.
	if err := runner.SaveCheckpoint(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := store.InjectSnapshotVersion(ctx, "SNAP-BAD", "snapshot/v9"); err != nil {
		t.Fatal(err)
	}
	_, err = store.Load(ctx)
	if domain.CodeOf(err) != domain.ESnapshotVersionBad {
		t.Fatalf("snapshot code=%s err=%v", domain.CodeOf(err), err)
	}
}

func TestFailedSaveDoesNotPublishNewGlobalSeq(t *testing.T) {
	ctx := context.Background()
	suite, _ := generator.DefaultSuite(424242)
	batch := suite.Batches["B3-tie-order"]
	store := openTestStore(t, "atomic.db")
	defer store.Close()

	runner := engine.NewStreamRunner(batch.Project, engine.DefaultOptions())
	for i := 0; i < 3; i++ {
		if _, _, err := runner.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.SaveCheckpoint(ctx, store); err != nil {
		t.Fatal(err)
	}
	wantState, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if _, _, err := runner.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	store.FaultBeforeCommit = func() error { return context.DeadlineExceeded }
	if err := runner.SaveCheckpoint(ctx, store); domain.CodeOf(err) != domain.ECkptWrite {
		t.Fatalf("fault save err=%v", err)
	}
	gotState, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotState.CommittedOpSeq != wantState.CommittedOpSeq {
		t.Fatalf("seq published=%d want %d", gotState.CommittedOpSeq, wantState.CommittedOpSeq)
	}
}

func openTestStore(t *testing.T, name string) *checkpoint.Store {
	t.Helper()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return store
}
