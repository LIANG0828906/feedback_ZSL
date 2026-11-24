package solver_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"levelacoustic/internal/editing"
	"levelacoustic/internal/generator"
	"levelacoustic/internal/model"
	"levelacoustic/internal/solver"
	"levelacoustic/internal/store"
	"levelacoustic/internal/verify"
)

func TestGeneratedTruthIncrementalAndRestore(t *testing.T) {
	ctx := context.Background()
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "acoustic.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := solver.Config{Store: st, Workers: 4, MaxBuffers: 2, BufferSamples: 16, CheckTruth: true}
	eng, err := solver.NewEngine(ctx, sample.Project, cfg)
	if err != nil {
		t.Fatal(err)
	}
	baseResult, err := eng.Solve(ctx)
	if err != nil {
		t.Fatalf("base solve: %v", err)
	}
	if baseResult.Progress != "6/6" {
		t.Fatalf("base progress = %s", baseResult.Progress)
	}
	if err := verify.CompareTruthTolerance(sample.Project, baseResult); err != nil {
		t.Fatalf("base truth comparison: %v", err)
	}
	if baseResult.Resources.QueuedJobs == 0 {
		t.Logf("resource summary: %+v", baseResult.Resources)
	}
	if baseResult.Resources.MaxParallel != 4 || baseResult.Resources.BufferCount != 2 || baseResult.Resources.MaxBufferSamples != 16 {
		t.Fatalf("resource limits not reported: %+v", baseResult.Resources)
	}
	if len(baseResult.Resources.Events) == 0 {
		t.Fatal("expected sample buffer wait events")
	}

	current := sample.Project
	for i, op := range sample.Edits.Ops {
		next, err := editing.ApplyOne(current, op)
		if err != nil {
			t.Fatalf("apply edit %d: %v", i, err)
		}
		report, err := eng.ApplyProject(ctx, next, op.Op)
		if err != nil {
			t.Fatalf("engine apply edit %d: %v", i, err)
		}
		result, err := eng.Solve(ctx)
		if err != nil {
			t.Fatalf("solve after edit %d: %v", i, err)
		}
		if err := verify.CompareTruthTolerance(next, result); err != nil {
			t.Fatalf("truth after edit %d: %v", i, err)
		}
		t.Logf("edit %d %-18s invalidated=%v reused=%v recomputed=%v",
			i, op.Op, report.InvalidatedRooms, report.ReusedRooms, result.RecomputedRooms)
		for _, entry := range report.Entries {
			t.Logf("  %s invalid=%v reused=%v reason=%s pairs=%d", entry.RoomID, entry.Invalidated, entry.Reused, entry.Reason, len(entry.AffectedPairs))
		}
		current = next
	}

	fullCfg := solver.Config{Workers: 3, MaxBuffers: 2, BufferSamples: 15, CheckTruth: true}
	fullEngine, err := solver.NewEngine(ctx, current, fullCfg)
	if err != nil {
		t.Fatal(err)
	}
	fullResult, err := fullEngine.Solve(ctx)
	if err != nil {
		t.Fatalf("full solve: %v", err)
	}
	incrementalResult, err := eng.Solve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify.CompareExact(fullResult, incrementalResult); err != nil {
		t.Fatalf("full vs incremental: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	restoredCfg := solver.Config{Store: restoredStore, Workers: 2, MaxBuffers: 1, BufferSamples: 16, CheckTruth: true}
	restored, err := solver.NewEngine(ctx, current, restoredCfg)
	if err != nil {
		t.Fatal(err)
	}
	report := restored.Restore()
	if report.Discarded || len(report.ReusedRooms) != len(current.Rooms) {
		t.Fatalf("restore report = %+v", report)
	}
	restoredResult, err := restored.Solve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(restoredResult.RecomputedRooms) != 0 {
		t.Fatalf("restore recomputed rooms = %v", restoredResult.RecomputedRooms)
	}
	if err := verify.CompareExact(fullResult, restoredResult); err != nil {
		t.Fatalf("restored vs full: %v", err)
	}
}

func TestCheckpointHashMismatchDiscardsWholeSet(t *testing.T) {
	ctx := context.Background()
	sample, err := generator.Generate(9)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "corrupt.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := solver.Config{Store: st, Workers: 2, MaxBuffers: 2, BufferSamples: 32, CheckTruth: true}
	eng, err := solver.NewEngine(ctx, sample.Project, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Solve(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE room_checkpoints SET content_hash='corrupted' WHERE room_id='R3'`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restoredEngine, err := solver.NewEngine(ctx, sample.Project, solver.Config{
		Store: reopened, Workers: 2, MaxBuffers: 2, BufferSamples: 32, CheckTruth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	restore := restoredEngine.Restore()
	if !restore.Discarded {
		t.Fatalf("damaged checkpoint set was not discarded: %+v", restore)
	}
	if len(restore.ReusedRooms) != 0 {
		t.Fatalf("mixed old rooms after corruption: %v", restore.ReusedRooms)
	}
	result, err := restoredEngine.Solve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RecomputedRooms) != len(sample.Project.Rooms) {
		t.Fatalf("expected full recompute, got %v", result.RecomputedRooms)
	}
	rows, err := reopened.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(sample.Project.Rooms) {
		t.Fatalf("checkpoint rows = %d", len(rows))
	}
}

func TestPerRoomRetryAfterInterruption(t *testing.T) {
	ctx := context.Background()
	sample, err := generator.Generate(11)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := solver.NewEngine(ctx, sample.Project, solver.Config{
		Workers: 2, MaxBuffers: 1, BufferSamples: 16, CheckTruth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := eng.SolveWithLimit(ctx, 3)
	if err == nil {
		t.Fatal("expected interruption")
	}
	me, ok := model.AsError(err)
	if !ok || me.Code != model.ErrInterrupted {
		t.Fatalf("want E_INTERRUPTED, got %v", err)
	}
	var failedRoom string
	for _, state := range partial.RoomStates {
		if state.State == model.StateFailed {
			failedRoom = state.RoomID
			break
		}
	}
	if failedRoom == "" {
		t.Fatalf("no failed room after interruption: %+v", partial.RoomStates)
	}
	retried, err := eng.RetryRoom(ctx, failedRoom)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried.RecomputedRooms) != 1 || retried.RecomputedRooms[0] != failedRoom {
		t.Fatalf("retry recomputed = %v, want [%s]", retried.RecomputedRooms, failedRoom)
	}
	if retried.CommittedRooms != 4 {
		t.Fatalf("committed after retry = %d", retried.CommittedRooms)
	}
}

func TestTruthRejectsTamperedChain(t *testing.T) {
	sample, err := generator.Generate(7)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sample.Project.Truth.Entries {
		entry := &sample.Project.Truth.Entries[i]
		if entry.Reachable && len(entry.RoomChain) > 1 {
			entry.RoomChain[0], entry.RoomChain[1] = entry.RoomChain[1], entry.RoomChain[0]
			entry.Segments[0].RoomID, entry.Segments[1].RoomID = entry.Segments[1].RoomID, entry.Segments[0].RoomID
			break
		}
	}
	fp, err := model.ProjectFingerprint(sample.Project)
	if err != nil {
		t.Fatal(err)
	}
	sample.Project.Truth.ProjectFingerprint = fp
	eng, err := solver.NewEngine(context.Background(), sample.Project, solver.Config{CheckTruth: true})
	if err == nil {
		_, err = eng.Solve(context.Background())
	}
	if err == nil {
		t.Fatal("expected truth chain validation failure")
	}
	me, ok := model.AsError(err)
	if !ok || me.Code != model.ErrChain {
		t.Fatalf("want E_CHAIN, got %v", err)
	}
}
