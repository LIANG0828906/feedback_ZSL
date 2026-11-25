package workbench_test

import (
	"context"
	"path/filepath"
	"testing"

	"mvccworkbench/internal/checkpoint"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/generator"
	"mvccworkbench/internal/workbench"
)

func TestRecoverUsesFullReplayForVersionMismatch(t *testing.T) {
	ctx := context.Background()
	suite, _ := generator.DefaultSuite(424242)
	project := suite.Batches["B3-tie-order"].Project
	dbPath := filepath.Join(t.TempDir(), "version-recover.db")

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := engine.NewStreamRunner(project, engine.DefaultOptions())
	if err := runner.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.SaveCheckpoint(ctx, store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE checkpoint_meta SET project_version = 'old/v0'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	session, err := workbench.Recover(ctx, project, dbPath, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Store.Close()
	if session.Mode != workbench.RecoveryFullReplay {
		t.Fatalf("mode=%s want %s", session.Mode, workbench.RecoveryFullReplay)
	}
	if err := session.Runner.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.Runner.SaveCheckpoint(ctx, session.Store); err != nil {
		t.Fatal(err)
	}
}

func TestInjectedBadSnapshotCanBeQuarantined(t *testing.T) {
	ctx := context.Background()
	suite, _ := generator.DefaultSuite(424242)
	project := suite.Batches["B1-visibility"].Project
	dbPath := filepath.Join(t.TempDir(), "bad-snapshot.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := engine.NewStreamRunner(project, engine.DefaultOptions())
	if err := runner.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.SaveCheckpoint(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := store.InjectSnapshotVersion(ctx, "SNAP-BAD", "snapshot/v0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx); domain.CodeOf(err) != domain.ESnapshotVersionBad {
		t.Fatalf("code=%s err=%v", domain.CodeOf(err), err)
	}
	quarantined, err := store.QuarantineIncompatibleSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 1 || quarantined[0] != "SNAP-BAD" {
		t.Fatalf("quarantined=%v", quarantined)
	}
	state, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("good committed state unavailable after quarantine: %v", err)
	}
	if state.CommittedOpSeq != len(project.Ops) {
		t.Fatalf("seq=%d want %d", state.CommittedOpSeq, len(project.Ops))
	}
}

func TestTruncatedCheckpointCanFullReplayAndRemainAvailable(t *testing.T) {
	ctx := context.Background()
	suite, _ := generator.DefaultSuite(424242)
	project := suite.Batches["B1-visibility"].Project
	dbPath := filepath.Join(t.TempDir(), "truncated-recover.db")

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := engine.NewStreamRunner(project, engine.DefaultOptions())
	if err := runner.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.SaveCheckpoint(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := store.InjectTruncatedChain(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx); domain.CodeOf(err) != domain.ECkptTruncated {
		t.Fatalf("code=%s err=%v", domain.CodeOf(err), err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	session, err := workbench.Recover(ctx, project, dbPath, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Store.Close()
	if session.Mode != workbench.RecoveryFaultReplay {
		t.Fatalf("mode=%s want %s", session.Mode, workbench.RecoveryFaultReplay)
	}
	if err := session.Runner.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.Runner.SaveCheckpoint(ctx, session.Store); err != nil {
		t.Fatal(err)
	}
	state, err := session.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.CommittedOpSeq != len(project.Ops) {
		t.Fatalf("replayed seq=%d want %d", state.CommittedOpSeq, len(project.Ops))
	}
}
